package manage

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newTestClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	c := New("mk")
	c.BaseURL = "https://runpod.test"
	c.HTTP = &http.Client{Transport: handlerTransport(h)}
	return c
}

// handlerTransport serves requests from a handler in memory, so tests need no
// listening socket.
type handlerTransport http.HandlerFunc

func (h handlerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	h(rec, r)
	return rec.Result(), nil
}

func TestListEndpointsFiltersByTagAndPaginates(t *testing.T) {
	var calls int
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer mk" {
			t.Errorf("missing bearer key")
		}
		var filter struct {
			Tags []struct {
				Key    string   `json:"key"`
				Values []string `json:"values"`
			} `json:"tags"`
		}
		if err := json.Unmarshal([]byte(r.URL.Query().Get("filters")), &filter); err != nil ||
			len(filter.Tags) != 1 || filter.Tags[0].Key != "managed-by" || filter.Tags[0].Values[0] != "x" {
			t.Errorf("bad filters param: %q", r.URL.Query().Get("filters"))
		}
		if r.URL.Query().Get("cursor") == "" {
			io.WriteString(w, `{"endpoints":[{"id":"ep1","name":"one","type":"QUEUE"}],"pagination":{"nextCursor":"c2","hasNextPage":true}}`)
			return
		}
		io.WriteString(w, `{"endpoints":[{"id":"ep2","name":"two","type":"LOAD_BALANCER","requestUrls":{"base":"https://ep2.api.runpod.ai"}}],"pagination":{"nextCursor":null,"hasNextPage":false}}`)
	})

	eps, err := c.ListEndpoints(context.Background(), "managed-by", "x")
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || len(eps) != 2 || eps[0].ID != "ep1" || eps[1].Type != TypeLoadBalancer || eps[1].RequestURLs["base"] == "" {
		t.Errorf("calls=%d endpoints=%+v", calls, eps)
	}
}

func TestCreateEndpointSendsFlatContainerFields(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		// Container fields must sit at the top level of the body, not nested.
		for _, k := range []string{"image", "env", "name", "type", "scaling", "gpu", "workers"} {
			if _, ok := body[k]; !ok {
				t.Errorf("request body missing %q: %v", k, body)
			}
		}
		if _, nested := body["Container"]; nested {
			t.Errorf("container config must be flattened: %v", body)
		}
		if body["scaling"].(map[string]any)["type"] != "REQUEST_COUNT" {
			t.Errorf("scaling = %v", body["scaling"])
		}
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"id":"ep9","name":"n","type":"LOAD_BALANCER","version":1,"workers":{"min":0,"max":2,"initTimeout":null}}`)
	})

	zero, two := 0, 2
	ep, err := c.CreateEndpoint(context.Background(), CreateEndpointRequest{
		Container: Container{Image: "img:1", Env: map[string]string{"PORT": "8000"}},
		Name:      "n", Type: TypeLoadBalancer,
		GPU:     &GPUConfig{Pools: []string{"ADA_24"}, Count: 1},
		Workers: &Workers{Min: &zero, Max: &two},
		Scaling: Scaling{Type: ScalingRequestCount, RequestCount: 4},
	})
	if err != nil {
		t.Fatal(err)
	}
	if ep.ID != "ep9" || ep.Version == nil || *ep.Version != 1 || ep.Workers.InitTimeout != nil {
		t.Errorf("endpoint = %+v", ep)
	}
}

func TestErrorsAndRateLimitRetry(t *testing.T) {
	var calls int
	var traced []Call
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch {
		case r.URL.Path == "/v2/serverless/missing":
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"title":"Not Found","status":404,"detail":"endpoint not found"}`)
		case calls == 1:
			w.Header().Set("Retry-After", "0")
			w.Header().Set("RateLimit-Policy", `"minute";q=180;w=60`)
			w.WriteHeader(http.StatusTooManyRequests)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	})
	c.Trace = func(call Call) { traced = append(traced, call) }

	// 429 is retried after Retry-After, then succeeds.
	if err := c.DeleteEndpoint(context.Background(), "ep1"); err != nil {
		t.Fatalf("delete after retry: %v", err)
	}
	if len(traced) != 2 || traced[0].Status != 429 || traced[0].RateLimitPolicy == "" || traced[1].Status != 204 {
		t.Errorf("traced = %+v", traced)
	}

	_, err := c.GetEndpoint(context.Background(), "missing")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 404 || apiErr.Detail != "endpoint not found" {
		t.Errorf("err = %v", err)
	}
}
