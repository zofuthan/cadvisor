// Copyright 2014 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package api provides a handler for /api/
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/golang/glog"
	"github.com/google/cadvisor/events"
	httpMux "github.com/google/cadvisor/http/mux"
	info "github.com/google/cadvisor/info/v1"
	"github.com/google/cadvisor/manager"
)

const (
	apiResource = "/api/"
)

func RegisterHandlers(mux httpMux.Mux, m manager.Manager) error {
	apiVersions := getApiVersions()
	supportedApiVersions := make(map[string]ApiVersion, len(apiVersions))
	for _, v := range apiVersions {
		supportedApiVersions[v.Version()] = v
	}

	mux.HandleFunc(apiResource, func(w http.ResponseWriter, r *http.Request) {
		err := handleRequest(supportedApiVersions, m, w, r)
		if err != nil {
			http.Error(w, err.Error(), 500)
		}
	})
	return nil
}

// Captures the API version, requestType [optional], and remaining request [optional].
var apiRegexp = regexp.MustCompile("/api/([^/]+)/?([^/]+)?(.*)")

const (
	apiVersion = iota + 1
	apiRequestType
	apiRequestArgs
)

func handleRequest(supportedApiVersions map[string]ApiVersion, m manager.Manager, w http.ResponseWriter, r *http.Request) error {
	start := time.Now()
	defer func() {
		glog.V(2).Infof("Request took %s", time.Since(start))
	}()

	request := r.URL.Path

	const apiPrefix = "/api"
	if !strings.HasPrefix(request, apiPrefix) {
		return fmt.Errorf("incomplete API request %q", request)
	}

	// If the request doesn't have an API version, list those.
	if request == apiPrefix || request == apiResource {
		versions := make([]string, 0, len(supportedApiVersions))
		for v := range supportedApiVersions {
			versions = append(versions, v)
		}
		sort.Strings(versions)
		fmt.Fprintf(w, "Supported API versions: %s", strings.Join(versions, ","))
		return nil
	}

	// Verify that we have all the elements we expect:
	// /<version>/<request type>[/<args...>]
	requestElements := apiRegexp.FindStringSubmatch(request)
	if len(requestElements) == 0 {
		return fmt.Errorf("malformed request %q", request)
	}
	version := requestElements[apiVersion]
	requestType := requestElements[apiRequestType]
	requestArgs := strings.Split(requestElements[apiRequestArgs], "/")

	// Check supported versions.
	versionHandler, ok := supportedApiVersions[version]
	if !ok {
		return fmt.Errorf("unsupported API version %q", version)
	}

	// If no request type, list possible request types.
	if requestType == "" {
		requestTypes := versionHandler.SupportedRequestTypes()
		sort.Strings(requestTypes)
		fmt.Fprintf(w, "Supported request types: %q", strings.Join(requestTypes, ","))
		return nil
	}

	// Trim the first empty element from the request.
	if len(requestArgs) > 0 && requestArgs[0] == "" {
		requestArgs = requestArgs[1:]
	}

	return versionHandler.HandleRequest(requestType, requestArgs, m, w, r)

}

func writeResult(res interface{}, w http.ResponseWriter) error {
	out, err := json.Marshal(res)
	if err != nil {
		return fmt.Errorf("failed to marshall response %+v with error: %s", res, err)
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write(out)
	return nil

}

func streamResults(eventChannel *events.EventChannel, w http.ResponseWriter, r *http.Request, m manager.Manager) error {
	cn, ok := w.(http.CloseNotifier)
	if !ok {
		return errors.New("could not access http.CloseNotifier")
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		return errors.New("could not access http.Flusher")
	}

	w.Header().Set("Transfer-Encoding", "chunked")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	enc := json.NewEncoder(w)
	for {
		select {
		case <-cn.CloseNotify():
			glog.V(3).Infof("Received CloseNotify event")
			m.CloseEventChannel(eventChannel.GetWatchId())
			return nil
		case ev := <-eventChannel.GetChannel():
			glog.V(3).Infof("Received event from watch channel in api: %v", ev)
			err := enc.Encode(ev)
			if err != nil {
				glog.Errorf("error encoding message %+v for result stream: %v", ev, err)
			}
			flusher.Flush()
		}
	}
}

func getContainerInfoRequest(body io.ReadCloser) (*info.ContainerInfoRequest, error) {
	query := info.DefaultContainerInfoRequest()
	decoder := json.NewDecoder(body)
	err := decoder.Decode(&query)
	if err != nil && err != io.EOF {
		return nil, fmt.Errorf("unable to decode the json value: %s", err)
	}

	return &query, nil
}

// The user can set any or none of the following arguments in any order
// with any twice defined arguments being assigned the first value.
// If the value type for the argument is wrong the field will be assumed to be
// unassigned
// bools: historical, subcontainers, oom_events, creation_events, deletion_events
// ints: max_events, start_time (unix timestamp), end_time (unix timestamp)
// example r.URL: http://localhost:8080/api/v1.3/events?oom_events=true&historical=true&max_events=10
func getEventRequest(r *http.Request) (*events.Request, bool, error) {
	query := events.NewRequest()
	getHistoricalEvents := false

	urlMap := r.URL.Query()

	if val, ok := urlMap["historical"]; ok {
		newBool, err := strconv.ParseBool(val[0])
		if err == nil {
			getHistoricalEvents = newBool
		}
	}
	if val, ok := urlMap["subcontainers"]; ok {
		newBool, err := strconv.ParseBool(val[0])
		if err == nil {
			query.IncludeSubcontainers = newBool
		}
	}
	if val, ok := urlMap["oom_events"]; ok {
		newBool, err := strconv.ParseBool(val[0])
		if err == nil {
			query.EventType[events.TypeOom] = newBool
		}
	}
	if val, ok := urlMap["creation_events"]; ok {
		newBool, err := strconv.ParseBool(val[0])
		if err == nil {
			query.EventType[events.TypeContainerCreation] = newBool
		}
	}
	if val, ok := urlMap["deletion_events"]; ok {
		newBool, err := strconv.ParseBool(val[0])
		if err == nil {
			query.EventType[events.TypeContainerDeletion] = newBool
		}
	}
	if val, ok := urlMap["max_events"]; ok {
		newInt, err := strconv.Atoi(val[0])
		if err == nil {
			query.MaxEventsReturned = int(newInt)
		}
	}
	if val, ok := urlMap["start_time"]; ok {
		newTime, err := time.Parse(time.RFC3339, val[0])
		if err == nil {
			query.StartTime = newTime
		}
	}
	if val, ok := urlMap["end_time"]; ok {
		newTime, err := time.Parse(time.RFC3339, val[0])
		if err == nil {
			query.EndTime = newTime
		}
	}

	glog.V(2).Infof(
		"%v was returned in api/handler.go:getEventRequest from the url rawQuery %v",
		query, r.URL.RawQuery)
	return query, getHistoricalEvents, nil
}

func getContainerName(request []string) string {
	return path.Join("/", strings.Join(request, "/"))
}
