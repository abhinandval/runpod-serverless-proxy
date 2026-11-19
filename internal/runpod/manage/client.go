package manage

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultBaseURL is the RunPod REST v2 production server.
const DefaultBaseURL = "https://api.runpod.io"

// Call describes one HTTP exchange with the management API, for tracing.
type Call struct {
	Method          string
	Path            string
	Status          int
	Duration        time.Duration
	RateLimit       string // RateLimit response header
	RateLimitPolicy string // RateLimit-Policy response header
	RetryAfter      string
	Err             error
}

// APIError is a non-2xx response from the management API.
type APIError struct {
	Status int      `json:"status"`
	Title  string   `json:"title"`
	Detail string   `json:"detail"`
	Errors []string `json:"errors"`
	Body   string   `json:"-"`
}

func (e *APIError) Error() string {
	msg := fmt.Sprintf("runpod api: %d %s: %s", e.Status, e.Title, e.Detail)
	if len(e.Errors) > 0 {
		msg += " (" + strings.Join(e.Errors, "; ") + ")"
	}
	if e.Title == "" && e.Detail == "" {
		msg = fmt.Sprintf("runpod api: %d: %s", e.Status, e.Body)
	}
	return msg
}

// Client talks to the RunPod REST v2 management API.
type Client struct {
	BaseURL string
	Key     string
	HTTP    *http.Client
	// Trace, if set, is called after every HTTP exchange.
	Trace func(Call)
	// MaxRetries is how many times a 429 is retried after its Retry-After delay.
	MaxRetries int
}

// New returns a client with sensible defaults.
func New(key string) *Client {
	return &Client{
		BaseURL:    DefaultBaseURL,
		Key:        key,
		HTTP:       &http.Client{Timeout: 60 * time.Second},
		MaxRetries: 3,
	}
}

func (c *Client) do(ctx context.Context, method, path string, q url.Values, in, out any) error {
	var payload []byte
	if in != nil {
		var err error
		if payload, err = json.Marshal(in); err != nil {
			return err
		}
	}
	u := strings.TrimRight(c.BaseURL, "/") + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	for attempt := 0; ; attempt++ {
		call, retryIn, err := c.once(ctx, method, u, path, payload, out)
		if c.Trace != nil {
			c.Trace(call)
		}
		if err == nil || call.Status != http.StatusTooManyRequests || attempt >= c.MaxRetries {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(retryIn):
		}
	}
}

func (c *Client) once(ctx context.Context, method, u, path string, payload []byte, out any) (Call, time.Duration, error) {
	call := Call{Method: method, Path: path}
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		call.Err = err
		return call, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Key)
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	start := time.Now()
	resp, err := c.HTTP.Do(req)
	if err != nil {
		call.Duration = time.Since(start)
		call.Err = err
		return call, 0, err
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(resp.Body)
	call.Duration = time.Since(start)
	call.Status = resp.StatusCode
	call.RateLimit = resp.Header.Get("RateLimit")
	call.RateLimitPolicy = resp.Header.Get("RateLimit-Policy")
	call.RetryAfter = resp.Header.Get("Retry-After")
	if readErr != nil {
		call.Err = readErr
		return call, 0, readErr
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		apiErr := &APIError{Status: resp.StatusCode, Body: truncate(string(raw), 500)}
		_ = json.Unmarshal(raw, apiErr)
		apiErr.Status = resp.StatusCode
		call.Err = apiErr
		retryIn := 5 * time.Second
		if secs, convErr := strconv.Atoi(call.RetryAfter); convErr == nil && secs >= 0 {
			retryIn = time.Duration(secs) * time.Second
		}
		return call, retryIn, apiErr
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			call.Err = fmt.Errorf("decode %s %s: %w", method, path, err)
			return call, 0, call.Err
		}
	}
	return call, 0, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// ListEndpoints returns every endpoint carrying the given tag. An empty tagKey
// lists all endpoints on the account.
func (c *Client) ListEndpoints(ctx context.Context, tagKey, tagValue string) ([]Endpoint, error) {
	q := url.Values{}
	if tagKey != "" {
		filter := map[string]any{"tags": []map[string]any{{"key": tagKey, "values": []string{tagValue}}}}
		raw, err := json.Marshal(filter)
		if err != nil {
			return nil, err
		}
		q.Set("filters", string(raw))
	}
	var all []Endpoint
	for {
		var page struct {
			Endpoints  []Endpoint `json:"endpoints"`
			Pagination pagination `json:"pagination"`
		}
		if err := c.do(ctx, http.MethodGet, "/v2/serverless", q, nil, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Endpoints...)
		if !page.Pagination.HasNextPage || page.Pagination.NextCursor == nil {
			return all, nil
		}
		q.Set("cursor", *page.Pagination.NextCursor)
	}
}

// GetEndpoint fetches one endpoint.
func (c *Client) GetEndpoint(ctx context.Context, id string) (*Endpoint, error) {
	var ep Endpoint
	if err := c.do(ctx, http.MethodGet, "/v2/serverless/"+url.PathEscape(id), nil, nil, &ep); err != nil {
		return nil, err
	}
	return &ep, nil
}

// CreateEndpoint creates a serverless endpoint.
func (c *Client) CreateEndpoint(ctx context.Context, req CreateEndpointRequest) (*Endpoint, error) {
	var ep Endpoint
	if err := c.do(ctx, http.MethodPost, "/v2/serverless", nil, req, &ep); err != nil {
		return nil, err
	}
	return &ep, nil
}

// UpdateEndpoint patches an endpoint. Changing the image creates a new release.
func (c *Client) UpdateEndpoint(ctx context.Context, id string, req UpdateEndpointRequest) (*Endpoint, error) {
	var ep Endpoint
	if err := c.do(ctx, http.MethodPatch, "/v2/serverless/"+url.PathEscape(id), nil, req, &ep); err != nil {
		return nil, err
	}
	return &ep, nil
}

// DeleteEndpoint permanently deletes an endpoint.
func (c *Client) DeleteEndpoint(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/v2/serverless/"+url.PathEscape(id), nil, nil, nil)
}

// ListWorkers returns the endpoint's current workers.
func (c *Client) ListWorkers(ctx context.Context, id string) (*WorkersResponse, error) {
	var out WorkersResponse
	if err := c.do(ctx, http.MethodGet, "/v2/serverless/"+url.PathEscape(id)+"/workers", nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListReleases returns the newest page of release history and the rollout state.
func (c *Client) ListReleases(ctx context.Context, id string) (*ReleasesResponse, error) {
	var out ReleasesResponse
	if err := c.do(ctx, http.MethodGet, "/v2/serverless/"+url.PathEscape(id)+"/releases", nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// EnsureTag returns the tag with this key and value, creating it if needed.
func (c *Client) EnsureTag(ctx context.Context, key, value string) (*Tag, error) {
	q := url.Values{}
	for {
		var page struct {
			Tags       []Tag      `json:"tags"`
			Pagination pagination `json:"pagination"`
		}
		if err := c.do(ctx, http.MethodGet, "/v2/tags", q, nil, &page); err != nil {
			return nil, err
		}
		for i := range page.Tags {
			if page.Tags[i].Key == key && page.Tags[i].Value == value {
				return &page.Tags[i], nil
			}
		}
		if !page.Pagination.HasNextPage || page.Pagination.NextCursor == nil {
			break
		}
		q.Set("cursor", *page.Pagination.NextCursor)
	}
	var tag Tag
	body := map[string]string{"key": key, "value": value}
	if err := c.do(ctx, http.MethodPost, "/v2/tags", nil, body, &tag); err != nil {
		return nil, err
	}
	return &tag, nil
}

// AttachTag attaches a tag to a serverless endpoint. Repeating it is a no-op.
func (c *Client) AttachTag(ctx context.Context, tagID, endpointID string) error {
	path := "/v2/tags/" + url.PathEscape(tagID) + "/resources/" + ResourceServerlessEndpoint + "/" + url.PathEscape(endpointID)
	return c.do(ctx, http.MethodPut, path, nil, nil, nil)
}

// ListServerlessGPUs returns the GPU catalog with serverless availability.
func (c *Client) ListServerlessGPUs(ctx context.Context) ([]GPUType, error) {
	q := url.Values{"include": {"AVAILABILITY"}, "product": {"SERVERLESS"}}
	var out struct {
		GPUs []GPUType `json:"gpus"`
	}
	if err := c.do(ctx, http.MethodGet, "/v2/catalog/gpus", q, nil, &out); err != nil {
		return nil, err
	}
	return out.GPUs, nil
}
