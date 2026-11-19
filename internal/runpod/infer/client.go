// Package infer talks to the request side of RunPod serverless endpoints: the
// queue API (/run, /runsync, /stream, /status), the /openai/v1 route on queue
// endpoints, and direct HTTP on load-balancing endpoints. Every call returns a
// Timing so callers can measure cold starts and streaming behaviour.
package infer

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Hosts for the two endpoint families.
const (
	QueueHost = "https://api.runpod.ai/v2" // + /{endpointId}/...
	LBSuffix  = ".api.runpod.ai"           // https://{endpointId}.api.runpod.ai/...
)

// QueueBase returns the base URL of a queue endpoint.
func QueueBase(endpointID string) string { return QueueHost + "/" + endpointID }

// LBBase returns the base URL of a load-balancing endpoint.
func LBBase(endpointID string) string { return "https://" + endpointID + LBSuffix }

// Timing is the measured outcome of one HTTP exchange. Durations are milliseconds
// from the moment the request was sent; -1 means "did not happen".
type Timing struct {
	Method      string            `json:"method"`
	URL         string            `json:"url"`
	Status      int               `json:"status"`
	HeadersMs   float64           `json:"headers_ms"`
	FirstByteMs float64           `json:"first_byte_ms"`
	FirstDataMs float64           `json:"first_data_ms"` // first SSE data line with content
	TotalMs     float64           `json:"total_ms"`
	Bytes       int               `json:"bytes"`
	Lines       int               `json:"lines"`
	DataLines   int               `json:"data_lines"`
	MaxGapMs    float64           `json:"max_gap_ms"` // longest silence between body lines
	SSEEvents   []string          `json:"sse_events,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	BodyHead    string            `json:"body_head,omitempty"`
	BodyTail    string            `json:"body_tail,omitempty"`
	Err         string            `json:"err,omitempty"`
	Canceled    bool              `json:"canceled,omitempty"`
}

// OK reports whether the exchange completed with a 2xx status and no transport error.
func (t Timing) OK() bool { return t.Err == "" && t.Status >= 200 && t.Status <= 299 }

// recorded response headers: anything that explains routing, limits or timeouts.
var keepHeaders = []string{
	"Content-Type", "Retry-After", "Server", "Via", "Cf-Ray", "Cf-Cache-Status",
	"X-Request-Id", "X-Runpod-Worker-Id", "RateLimit", "RateLimit-Policy",
}

// Client sends inference requests with the account's (ideally narrow) API key.
type Client struct {
	Key  string
	HTTP *http.Client
}

// New returns a client without an overall timeout; callers bound requests with context.
func New(key string) *Client {
	return &Client{Key: key, HTTP: &http.Client{}}
}

// LineFunc is called for every line of the response body with the elapsed time.
type LineFunc func(elapsed time.Duration, line string)

// Do sends one request and measures it. body may be nil, []byte, or any JSON-encodable
// value. The response body is read line by line so SSE streams are timed per event.
func (c *Client) Do(ctx context.Context, method, url string, body any, headers map[string]string, onLine LineFunc) Timing {
	t := Timing{Method: method, URL: url, HeadersMs: -1, FirstByteMs: -1, FirstDataMs: -1}
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		rd = bytes.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			t.Err = err.Error()
			return t
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		t.Err = err.Error()
		return t
	}
	req.Header.Set("Authorization", "Bearer "+c.Key)
	if rd != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	start := time.Now()
	ms := func() float64 { return float64(time.Since(start).Microseconds()) / 1000 }
	fail := func(err error) Timing {
		t.TotalMs = ms()
		t.Err = err.Error()
		t.Canceled = ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
		return t
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fail(err)
	}
	defer resp.Body.Close()
	t.Status = resp.StatusCode
	t.HeadersMs = ms()
	t.Headers = map[string]string{}
	for _, k := range keepHeaders {
		if v := resp.Header.Get(k); v != "" {
			t.Headers[k] = v
		}
	}

	const keep = 2048
	var head, tail []byte
	seenEvents := map[string]bool{}
	last := time.Now()
	br := bufio.NewReaderSize(resp.Body, 64*1024)
	for {
		line, readErr := br.ReadString('\n')
		if len(line) > 0 {
			now := time.Now()
			if t.FirstByteMs < 0 {
				t.FirstByteMs = ms()
			} else if gap := float64(now.Sub(last).Microseconds()) / 1000; gap > t.MaxGapMs {
				t.MaxGapMs = gap
			}
			last = now
			t.Bytes += len(line)
			t.Lines++
			if len(head) < keep {
				head = append(head, line...)
			}
			tail = append(tail, line...)
			if len(tail) > keep {
				tail = tail[len(tail)-keep:]
			}
			trimmed := strings.TrimRight(line, "\r\n")
			switch {
			case strings.HasPrefix(trimmed, "data:"):
				t.DataLines++
				if t.FirstDataMs < 0 && strings.TrimSpace(trimmed[5:]) != "" {
					t.FirstDataMs = ms()
				}
			case strings.HasPrefix(trimmed, "event:"):
				name := strings.TrimSpace(trimmed[6:])
				if !seenEvents[name] {
					seenEvents[name] = true
					t.SSEEvents = append(t.SSEEvents, name)
				}
			case strings.HasPrefix(trimmed, ":"):
				if !seenEvents["(comment)"] {
					seenEvents["(comment)"] = true
					t.SSEEvents = append(t.SSEEvents, "(comment)")
				}
			}
			if onLine != nil {
				onLine(time.Since(start), trimmed)
			}
		}
		if readErr != nil {
			if len(head) > keep {
				head = head[:keep]
			}
			t.BodyHead = string(head)
			if t.Bytes > keep {
				t.BodyTail = string(tail)
			}
			if readErr != io.EOF {
				return fail(readErr)
			}
			break
		}
	}
	t.TotalMs = ms()
	return t
}

// Job is the queue API's job envelope.
type Job struct {
	ID            string          `json:"id"`
	Status        string          `json:"status"` // IN_QUEUE, IN_PROGRESS, COMPLETED, FAILED, CANCELLED, TIMED_OUT
	DelayTime     float64         `json:"delayTime"`
	ExecutionTime float64         `json:"executionTime"`
	Output        json.RawMessage `json:"output,omitempty"`
	Stream        json.RawMessage `json:"stream,omitempty"`
	Error         json.RawMessage `json:"error,omitempty"`
}

// Terminal reports whether the job has finished, successfully or not.
func (j Job) Terminal() bool {
	switch j.Status {
	case "COMPLETED", "FAILED", "CANCELLED", "TIMED_OUT":
		return true
	}
	return false
}

// StreamItems returns how many chunks a /stream response carried.
func (j Job) StreamItems() int {
	var items []json.RawMessage
	if len(j.Stream) == 0 || json.Unmarshal(j.Stream, &items) != nil {
		return 0
	}
	return len(items)
}

// JobCall performs a queue API call and decodes the job envelope from the body.
// The Timing is always returned; the Job is zero if the body was not a job.
func (c *Client) JobCall(ctx context.Context, method, url string, body any) (Job, Timing) {
	var buf strings.Builder
	t := c.Do(ctx, method, url, body, nil, func(_ time.Duration, line string) {
		buf.WriteString(line)
		buf.WriteByte('\n')
	})
	var job Job
	if t.Err == "" {
		if err := json.Unmarshal([]byte(buf.String()), &job); err != nil && t.OK() {
			t.Err = fmt.Sprintf("decode job: %v", err)
		}
	}
	return job, t
}

// Run submits an asynchronous job: POST /run with {"input": input}.
func (c *Client) Run(ctx context.Context, base string, input any) (Job, Timing) {
	return c.JobCall(ctx, http.MethodPost, base+"/run", map[string]any{"input": input})
}

// RunSync submits a job and waits up to waitMs (0 = platform default of 90 s).
func (c *Client) RunSync(ctx context.Context, base string, input any, waitMs int) (Job, Timing) {
	u := base + "/runsync"
	if waitMs > 0 {
		u += fmt.Sprintf("?wait=%d", waitMs)
	}
	return c.JobCall(ctx, http.MethodPost, u, map[string]any{"input": input})
}

// Status fetches a job's state: GET /status/{id}.
func (c *Client) Status(ctx context.Context, base, jobID string) (Job, Timing) {
	return c.JobCall(ctx, http.MethodGet, base+"/status/"+jobID, nil)
}

// StatusSync long-polls a job's state: GET /status-sync/{id}?wait=ms.
func (c *Client) StatusSync(ctx context.Context, base, jobID string, waitMs int) (Job, Timing) {
	u := base + "/status-sync/" + jobID
	if waitMs > 0 {
		u += fmt.Sprintf("?wait=%d", waitMs)
	}
	return c.JobCall(ctx, http.MethodGet, u, nil)
}

// Stream fetches the chunks yielded since the previous call: GET /stream/{id}.
func (c *Client) Stream(ctx context.Context, base, jobID string) (Job, Timing) {
	return c.JobCall(ctx, http.MethodGet, base+"/stream/"+jobID, nil)
}

// Cancel stops a queued or running job: POST /cancel/{id}.
func (c *Client) Cancel(ctx context.Context, base, jobID string) (Job, Timing) {
	return c.JobCall(ctx, http.MethodPost, base+"/cancel/"+jobID, nil)
}
