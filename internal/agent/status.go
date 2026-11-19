// Package agent is the process that runs inside every engine container. It is
// the only thing the gateway talks to in a unit: it downloads the model files,
// starts the engine on a private port, reports what phase the unit is in, and
// proxies requests to the engine once it is ready.
package agent

import (
	"sync"
	"time"
)

// Phase is where a unit is in its start-up.
type Phase string

const (
	PhaseStarting    Phase = "starting"
	PhaseDownloading Phase = "downloading"
	PhaseLoading     Phase = "loading"
	PhaseReady       Phase = "ready"
	PhaseFailed      Phase = "failed"
)

// Download is the progress of the model download.
type Download struct {
	DoneBytes  int64 `json:"done_bytes"`
	TotalBytes int64 `json:"total_bytes"` // 0 when the size could not be determined
	Files      int   `json:"files"`
	FilesDone  int   `json:"files_done"`
}

// Status is what GET /agent/status returns.
type Status struct {
	Phase    Phase    `json:"phase"`
	Engine   string   `json:"engine"`
	Model    string   `json:"model"` // the name the engine serves the model under
	WorkerID string   `json:"worker_id,omitempty"`
	Download Download `json:"download"`
	InFlight int64    `json:"in_flight"`
	Error    string   `json:"error,omitempty"`
	// UptimeSeconds is the time since the agent started; PhaseSeconds is the
	// time each finished phase took, so cold starts can be broken down later.
	UptimeSeconds float64            `json:"uptime_seconds"`
	PhaseSeconds  map[string]float64 `json:"phase_seconds"`
}

// tracker holds the live status. All methods are safe for concurrent use.
type tracker struct {
	mu         sync.Mutex
	st         Status
	started    time.Time
	phaseSince time.Time
	now        func() time.Time
}

func newTracker(engine, model, workerID string, now func() time.Time) *tracker {
	t := now()
	return &tracker{
		st:      Status{Phase: PhaseStarting, Engine: engine, Model: model, WorkerID: workerID, PhaseSeconds: map[string]float64{}},
		started: t, phaseSince: t, now: now,
	}
}

// set moves to a new phase and records how long the previous one took.
func (t *tracker) set(p Phase) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.st.Phase == p {
		return
	}
	now := t.now()
	t.st.PhaseSeconds[string(t.st.Phase)] = now.Sub(t.phaseSince).Seconds()
	t.st.Phase, t.phaseSince = p, now
}

// fail records the error and moves to the failed phase.
func (t *tracker) fail(err error) {
	t.set(PhaseFailed)
	t.mu.Lock()
	t.st.Error = err.Error()
	t.mu.Unlock()
}

func (t *tracker) setDownload(d Download) {
	t.mu.Lock()
	t.st.Download = d
	t.mu.Unlock()
}

func (t *tracker) phase() Phase {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.st.Phase
}

// snapshot returns a copy of the status with the live counters filled in.
func (t *tracker) snapshot(inFlight int64) Status {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.st
	s.InFlight = inFlight
	s.UptimeSeconds = t.now().Sub(t.started).Seconds()
	s.PhaseSeconds = make(map[string]float64, len(t.st.PhaseSeconds))
	for k, v := range t.st.PhaseSeconds {
		s.PhaseSeconds[k] = v
	}
	return s
}
