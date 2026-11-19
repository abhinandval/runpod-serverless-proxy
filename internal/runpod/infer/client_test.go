package infer

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"
)

// streamTransport answers every request in memory (no listening socket). The
// write function runs in its own goroutine and streams the body through a pipe,
// so tests can control when each chunk reaches the client.
type streamTransport struct {
	status int
	header http.Header
	write  func(r *http.Request, w io.Writer)
}

func (s streamTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	pr, pw := io.Pipe()
	go func() {
		s.write(r, pw)
		// Like a real connection, a cancelled request surfaces the context error
		// to the reader instead of a clean EOF. A nil error closes with EOF.
		pw.CloseWithError(r.Context().Err())
	}()
	h := s.header
	if h == nil {
		h = http.Header{}
	}
	return &http.Response{StatusCode: s.status, Header: h, Body: pr, Request: r}, nil
}

func clientWith(tr streamTransport) *Client {
	c := New("k")
	c.HTTP = &http.Client{Transport: tr}
	return c
}

func TestDoTimesSSEStream(t *testing.T) {
	c := clientWith(streamTransport{
		status: http.StatusOK,
		header: http.Header{"Content-Type": {"text/event-stream"}},
		write: func(r *http.Request, w io.Writer) {
			if got := r.Header.Get("Authorization"); got != "Bearer k" {
				t.Errorf("Authorization = %q", got)
			}
			fmt.Fprint(w, ": keepalive\n\n")
			time.Sleep(60 * time.Millisecond)
			fmt.Fprint(w, "event: message_start\ndata: {\"a\":1}\n\n")
			fmt.Fprint(w, "event: message_stop\ndata: {\"b\":2}\n\n")
		},
	})

	var lines int
	tm := c.Do(context.Background(), http.MethodPost, "https://x.test/v1/messages", map[string]any{"x": 1}, nil,
		func(time.Duration, string) { lines++ })

	if !tm.OK() {
		t.Fatalf("not ok: %+v", tm)
	}
	if tm.DataLines != 2 {
		t.Errorf("DataLines = %d, want 2", tm.DataLines)
	}
	if want := []string{"(comment)", "message_start", "message_stop"}; fmt.Sprint(tm.SSEEvents) != fmt.Sprint(want) {
		t.Errorf("SSEEvents = %v, want %v", tm.SSEEvents, want)
	}
	if tm.FirstByteMs < 0 || tm.FirstDataMs < tm.FirstByteMs+40 {
		t.Errorf("first data (%.1fms) should trail first byte (%.1fms) by the server delay", tm.FirstDataMs, tm.FirstByteMs)
	}
	if tm.MaxGapMs < 40 {
		t.Errorf("MaxGapMs = %.1f, want >= 40", tm.MaxGapMs)
	}
	if lines != tm.Lines {
		t.Errorf("callback saw %d lines, Timing has %d", lines, tm.Lines)
	}
	if tm.Headers["Content-Type"] != "text/event-stream" {
		t.Errorf("Headers = %v", tm.Headers)
	}
}

func TestDoRecordsErrorStatusAndBody(t *testing.T) {
	c := clientWith(streamTransport{
		status: http.StatusServiceUnavailable,
		write:  func(_ *http.Request, w io.Writer) { fmt.Fprint(w, "no workers available") },
	})
	tm := c.Do(context.Background(), http.MethodGet, "https://x.test/ping", nil, nil, nil)
	if tm.OK() || tm.Status != http.StatusServiceUnavailable || tm.BodyHead != "no workers available" {
		t.Errorf("want recorded 503 with body, got %+v", tm)
	}
}

func TestDoMarksMidStreamCancel(t *testing.T) {
	c := clientWith(streamTransport{
		status: http.StatusOK,
		write: func(r *http.Request, w io.Writer) {
			fmt.Fprint(w, "data: first\n\n")
			<-r.Context().Done()
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	tm := c.Do(ctx, http.MethodGet, "https://x.test/slow", nil, nil, nil)
	if !tm.Canceled || tm.Status != http.StatusOK || tm.DataLines != 1 || tm.OK() {
		t.Errorf("want canceled mid-stream after one data line, got %+v", tm)
	}
}

func TestJobCallDecodesEnvelope(t *testing.T) {
	c := clientWith(streamTransport{
		status: http.StatusOK,
		write: func(r *http.Request, w io.Writer) {
			if r.URL.Path != "/v2/ep/stream/job-1" {
				t.Errorf("path = %s", r.URL.Path)
			}
			fmt.Fprint(w, `{"id":"job-1","status":"IN_PROGRESS","stream":[{"output":"a"},{"output":"b"}]}`)
		},
	})
	job, tm := c.Stream(context.Background(), "https://x.test/v2/ep", "job-1")
	if !tm.OK() || job.ID != "job-1" || job.Terminal() || job.StreamItems() != 2 {
		t.Errorf("job = %+v timing = %+v", job, tm)
	}
}

func TestRunSyncBuildsURLAndEnvelope(t *testing.T) {
	c := clientWith(streamTransport{
		status: http.StatusOK,
		write: func(r *http.Request, w io.Writer) {
			body, _ := io.ReadAll(r.Body)
			if r.URL.Path != "/v2/ep/runsync" || r.URL.Query().Get("wait") != "20000" || string(body) != `{"input":{"prompt":"hi"}}` {
				t.Errorf("request = %s %s", r.URL, body)
			}
			fmt.Fprint(w, `{"id":"j","status":"COMPLETED","delayTime":1200,"executionTime":300,"output":"ok"}`)
		},
	})
	job, tm := c.RunSync(context.Background(), "https://x.test/v2/ep", map[string]any{"prompt": "hi"}, 20000)
	if !tm.OK() || !job.Terminal() || job.DelayTime != 1200 {
		t.Errorf("job = %+v timing = %+v", job, tm)
	}
}
