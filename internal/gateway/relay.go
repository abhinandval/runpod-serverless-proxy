package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// apiKind is the wire format a client speaks; errors and keep-alives differ.
type apiKind int

const (
	apiOpenAI apiKind = iota
	apiAnthropic
)

func apiOf(path string) apiKind {
	if strings.HasPrefix(path, "/v1/messages") {
		return apiAnthropic
	}
	return apiOpenAI
}

// errorBody renders an error in the client's own format.
func errorBody(api apiKind, typ, msg string) any {
	if api == apiAnthropic {
		return map[string]any{"type": "error", "error": map[string]string{"type": typ, "message": msg}}
	}
	return map[string]any{"error": map[string]any{"message": msg, "type": typ, "code": nil}}
}

func writeError(w http.ResponseWriter, api apiKind, code int, typ, msg string) {
	writeJSON(w, code, errorBody(api, typ, msg))
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// relay writes one response to the client. For a streaming request it may
// have to commit to "200 OK" and start sending keep-alives before the unit is
// up; after that point a failure can only be reported inside the stream.
type relay struct {
	w       http.ResponseWriter
	api     apiKind
	stream  bool
	started bool // the 200 and SSE headers have been sent
}

func (o *relay) flush() {
	if f, ok := o.w.(http.Flusher); ok {
		f.Flush()
	}
}

// keepAlive starts the stream at once and writes a keep-alive every interval
// until the returned stop function is called. It does nothing for
// non-streaming requests, which have no way to carry one.
func (o *relay) keepAlive(ctx context.Context, every time.Duration) (stop func()) {
	if !o.stream {
		return func() {}
	}
	h := o.w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no") // ask reverse proxies not to buffer the stream
	o.w.WriteHeader(http.StatusOK)
	o.flush()
	o.started = true

	ping := ": keep-alive\n\n"
	if o.api == apiAnthropic {
		ping = "event: ping\ndata: {\"type\": \"ping\"}\n\n"
	}
	quit, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-quit:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				io.WriteString(o.w, ping)
				o.flush()
			}
		}
	}()
	// stop waits for the goroutine, so nothing else writes concurrently.
	return func() { close(quit); <-done }
}

// fail reports an error: as a normal HTTP error if nothing was sent yet,
// otherwise as an error event in the stream's own format.
func (o *relay) fail(code int, typ, msg string) {
	if !o.started {
		writeError(o.w, o.api, code, typ, msg)
		return
	}
	raw, _ := json.Marshal(errorBody(o.api, typ, msg))
	if o.api == apiAnthropic {
		io.WriteString(o.w, "event: error\ndata: "+string(raw)+"\n\n")
	} else {
		io.WriteString(o.w, "data: "+string(raw)+"\n\ndata: [DONE]\n\n")
	}
	o.flush()
}

// copy relays the unit's response to the client unbuffered and closes it.
func (o *relay) copy(resp *http.Response) {
	defer resp.Body.Close()
	if o.started {
		if resp.StatusCode != http.StatusOK {
			// The 200 is already on the wire; surface the engine's error in-stream.
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
			o.fail(resp.StatusCode, "api_error", "engine returned "+resp.Status+": "+strings.TrimSpace(string(raw)))
			return
		}
	} else {
		h := o.w.Header()
		for k, vs := range resp.Header {
			if !hopHeaders[http.CanonicalHeaderKey(k)] {
				h[k] = vs
			}
		}
		o.w.WriteHeader(resp.StatusCode)
	}
	buf := make([]byte, 32*1024)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := o.w.Write(buf[:n]); werr != nil {
				return // the client went away; closing the body cancels the unit's work
			}
			o.flush()
		}
		if err != nil {
			return
		}
	}
}
