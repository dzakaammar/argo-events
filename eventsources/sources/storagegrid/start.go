/*
Copyright 2018 BlackRock, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package storagegrid

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-resty/resty/v2"
	"github.com/google/uuid"
	"github.com/joncalhoun/qson"

	"github.com/argoproj/argo-events/common"
	"github.com/argoproj/argo-events/common/logging"
	"github.com/argoproj/argo-events/eventsources/common/webhook"
	"github.com/argoproj/argo-events/eventsources/sources"
	apicommon "github.com/argoproj/argo-events/pkg/apis/common"
	"github.com/argoproj/argo-events/pkg/apis/eventsource/v1alpha1"
)

// controller controls the webhook operations
var (
	controller = webhook.NewController()
)

var (
	respBody = `
<PublishResponse xmlns="http://argoevents-sns-server/">
    <PublishResult> 
        <MessageId>` + generateUUID().String() + `</MessageId> 
    </PublishResult> 
    <ResponseMetadata>
       <RequestId>` + generateUUID().String() + `</RequestId>
    </ResponseMetadata> 
</PublishResponse>` + "\n"

	notificationBodyTemplate = `
<NotificationConfiguration>
	<TopicConfiguration>
	  <Id>%s</Id>
	  <Topic>%s</Topic>
	  %s
	</TopicConfiguration>
</NotificationConfiguration>
` + "\n"
)

// set up the activation and inactivation channels to control the state of routes.
func init() {
	go webhook.ProcessRouteStatus(controller)
}

// generateUUID returns a new uuid
func generateUUID() uuid.UUID {
	return uuid.New()
}

// filterName filters object key based on configured prefix and/or suffix
func filterName(notification *storageGridNotification, eventSource *v1alpha1.StorageGridEventSource) bool {
	if eventSource.Filter == nil {
		return true
	}
	if eventSource.Filter.Prefix != "" && eventSource.Filter.Suffix != "" {
		return strings.HasPrefix(notification.Message.Records[0].S3.Object.Key, eventSource.Filter.Prefix) && strings.HasSuffix(notification.Message.Records[0].S3.Object.Key, eventSource.Filter.Suffix)
	}
	if eventSource.Filter.Prefix != "" {
		return strings.HasPrefix(notification.Message.Records[0].S3.Object.Key, eventSource.Filter.Prefix)
	}
	if eventSource.Filter.Suffix != "" {
		return strings.HasSuffix(notification.Message.Records[0].S3.Object.Key, eventSource.Filter.Suffix)
	}
	return true
}

// GetEventSourceName returns name of event source
func (el *EventListener) GetEventSourceName() string {
	return el.EventSourceName
}

// GetEventName returns name of event
func (el *EventListener) GetEventName() string {
	return el.EventName
}

// GetEventSourceType return type of event server
func (el *EventListener) GetEventSourceType() apicommon.EventSourceType {
	return apicommon.StorageGridEvent
}

// Implement Router
// 1. GetRoute
// 2. HandleRoute
// 3. PostActivate
// 4. PostDeactivate

// GetRoute returns the route
func (router *Router) GetRoute() *webhook.Route {
	return router.route
}

// HandleRoute handles new route
func (router *Router) HandleRoute(writer http.ResponseWriter, request *http.Request) {
	route := router.route

	logger := route.Logger.WithFields(
		map[string]interface{}{
			logging.LabelEventSourceName: route.EventSourceName,
			logging.LabelEventName:       route.EventName,
			logging.LabelEndpoint:        route.Context.Endpoint,
			logging.LabelPort:            route.Context.Port,
			logging.LabelHTTPMethod:      route.Context.Method,
		})

	logger.Infoln("processing incoming request...")

	if !route.Active {
		logger.Warnln("endpoint is inactive, won't process the request")
		common.SendErrorResponse(writer, "inactive endpoint")
		return
	}

	logger.Infoln("parsing the request body...")
	body, err := ioutil.ReadAll(request.Body)
	if err != nil {
		logger.WithError(err).Errorln("failed to parse request body")
		common.SendErrorResponse(writer, "")
		return
	}

	if request.Method == http.MethodHead {
		respBody = ""
	}

	writer.WriteHeader(http.StatusOK)
	writer.Header().Add("Content-Type", "text/plain")
	if _, err := writer.Write([]byte(respBody)); err != nil {
		logger.WithError(err).Errorln("failed to write the response")
		return
	}

	// notification received from storage grid is url encoded.
	parsedURL, err := url.QueryUnescape(string(body))
	if err != nil {
		logger.WithError(err).Errorln("failed to unescape request body url")
		return
	}
	b, err := qson.ToJSON(parsedURL)
	if err != nil {
		logger.WithError(err).Errorln("failed to convert request body in JSON format")
		return
	}

	logger.Infoln("converting request body to storage grid notification")
	var notification *storageGridNotification
	err = json.Unmarshal(b, &notification)
	if err != nil {
		logger.WithError(err).Errorln("failed to convert the request body into storage grid notification")
		return
	}

	if filterName(notification, router.storageGridEventSource) {
		logger.WithError(err).Errorln("new event received, dispatching event on route's data channel")
		route.DataCh <- b
		return
	}

	logger.Warnln("discarding notification since it did not pass all filters")
}

// PostActivate performs operations once the route is activated and ready to consume requests
func (router *Router) PostActivate() error {
	eventSource := router.storageGridEventSource
	route := router.route

	authToken, ok := common.GetEnvFromSecret(eventSource.AuthToken)
	if !ok {
		return errors.New("AuthToken not found in ENV")
	}

	registrationURL := common.FormattedURL(eventSource.Webhook.URL, eventSource.Webhook.Endpoint)

	client := resty.New()

	logger := route.Logger.WithFields(
		map[string]interface{}{
			common.LabelEventSource: route.EventName,
			"registration-url":      registrationURL,
			"bucket":                eventSource.Bucket,
			"auth-secret-name":      eventSource.AuthToken.Name,
			"api-url":               eventSource.APIURL,
		})

	logger.Infoln("checking if the endpoint already exists...")

	response, err := client.R().
		SetHeader("Content-Type", common.MediaTypeJSON).
		SetAuthToken(authToken).
		SetResult(&getEndpointResponse{}).
		SetError(&genericResponse{}).
		Get(common.FormattedURL(eventSource.APIURL, "/org/endpoints"))
	if err != nil {
		return err
	}

	if !response.IsSuccess() {
		errObj := response.Error().(*genericResponse)
		return fmt.Errorf("failed to list existing endpoints. reason: %s", errObj.Message.Text)
	}

	endpointResponse := response.Result().(*getEndpointResponse)

	isURNExists := false

	for _, endpoint := range endpointResponse.Data {
		if endpoint.EndpointURN == eventSource.TopicArn {
			logger.Infoln("endpoint with topic urn already exists, won't register duplicate endpoint")
			isURNExists = true
			break
		}
	}

	if !isURNExists {
		logger.Infoln("endpoint urn does not exist, registering a new endpoint")
		newEndpoint := createEndpointRequest{
			DisplayName: router.route.EventName,
			EndpointURI: common.FormattedURL(eventSource.Webhook.URL, eventSource.Webhook.Endpoint),
			EndpointURN: eventSource.TopicArn,
			AuthType:    "anonymous",
			InsecureTLS: true,
		}

		newEndpointBody, err := json.Marshal(&newEndpoint)
		if err != nil {
			return err
		}

		response, err := client.R().
			SetHeader("Content-Type", common.MediaTypeJSON).
			SetAuthToken(authToken).
			SetBody(string(newEndpointBody)).
			SetResult(&genericResponse{}).
			SetError(&genericResponse{}).
			Post(common.FormattedURL(eventSource.APIURL, "/org/endpoints"))
		if err != nil {
			return err
		}

		if !response.IsSuccess() {
			errObj := response.Error().(*genericResponse)
			return fmt.Errorf("failed to register the endpoint. reason: %s", errObj.Message.Text)
		}

		logger.Infoln("successfully registered the endpoint")
	}

	logger.Infoln("registering notification configuration on storagegrid...")

	var events []string
	for _, event := range eventSource.Events {
		events = append(events, fmt.Sprintf("<Event>%s</Event>", event))
	}

	eventXML := strings.Join(events, "\n")

	notificationBody := fmt.Sprintf(notificationBodyTemplate, route.EventName, eventSource.TopicArn, eventXML)

	notification := &storageGridNotificationRequest{
		Notification: notificationBody,
	}

	notificationRequestBody, err := json.Marshal(notification)
	if err != nil {
		return err
	}

	response, err = client.R().
		SetHeader("Content-Type", common.MediaTypeJSON).
		SetAuthToken(authToken).
		SetBody(string(notificationRequestBody)).
		SetResult(&registerNotificationResponse{}).
		SetError(&genericResponse{}).
		Put(common.FormattedURL(eventSource.APIURL, fmt.Sprintf("/org/containers/%s/notification", eventSource.Bucket)))
	if err != nil {
		return err
	}

	if !response.IsSuccess() {
		errObj := response.Error().(*genericResponse)
		return fmt.Errorf("failed to configure notification. reason %s\n", errObj.Message.Text)
	}

	logger.Infoln("successfully registered notification configuration on storagegrid")
	return nil
}

// PostInactivate performs operations after the route is inactivated
func (router *Router) PostInactivate() error {
	return nil
}

// StartListening starts an event source
func (el *EventListener) StartListening(ctx context.Context, dispatch func([]byte) error) error {
	logger := logging.FromContext(ctx)
	log := logging.FromContext(ctx).WithFields(map[string]interface{}{
		logging.LabelEventSourceType: el.GetEventSourceType(),
		logging.LabelEventSourceName: el.GetEventSourceName(),
		logging.LabelEventName:       el.GetEventName(),
	})
	log.Infoln("started processing the Storage Grid event source...")
	defer sources.Recover(el.GetEventName())

	storagegridEventSource := &el.StorageGridEventSource
	route := webhook.NewRoute(storagegridEventSource.Webhook, logger, el.GetEventSourceName(), el.GetEventName())

	return webhook.ManageRoute(ctx, &Router{
		route:                  route,
		storageGridEventSource: storagegridEventSource,
	}, controller, dispatch)
}
