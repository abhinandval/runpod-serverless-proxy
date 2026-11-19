// Package manage is a thin client for the RunPod REST v2 management API
// (https://api.runpod.io/v2, spec at /v2/openapi.json). It covers only what the
// fleet reconciler needs: serverless endpoints, tags, releases, workers and the
// GPU catalog.
package manage

// EndpointType is the request-routing model of an endpoint. Immutable after create.
type EndpointType string

const (
	TypeQueue        EndpointType = "QUEUE"
	TypeLoadBalancer EndpointType = "LOAD_BALANCER"
)

const (
	ScalingQueueDelay   = "QUEUE_DELAY"   // queue endpoints only
	ScalingRequestCount = "REQUEST_COUNT" // required for load balancers
)

// ResourceServerlessEndpoint is the tag resource type for serverless endpoints.
const ResourceServerlessEndpoint = "SERVERLESS_ENDPOINT"

// Container is the inline container configuration shared by create, update and read.
type Container struct {
	Image      string            `json:"image,omitempty"`
	Cmd        []string          `json:"cmd,omitempty"`
	Entrypoint []string          `json:"entrypoint,omitempty"`
	Disk       int               `json:"disk,omitempty"` // GB, ephemeral
	Env        map[string]string `json:"env,omitempty"`  // PATCH replaces the whole map
	Ports      []string          `json:"ports,omitempty"`
	Registry   *string           `json:"registry,omitempty"`
}

// GPUConfig selects serverless GPU pools. Pools have no priority order. On PATCH,
// sending Pools without ExcludedTypes clears the exclusions.
type GPUConfig struct {
	Pools               []string `json:"pools,omitempty"`
	ExcludedTypes       []string `json:"excludedTypes,omitempty"`
	Count               int      `json:"count,omitempty"`
	AllowedCudaVersions []string `json:"allowedCudaVersions,omitempty"`
	MinCudaVersion      string   `json:"minCudaVersion,omitempty"`
}

// Workers holds worker-count and timeout settings, all in seconds.
type Workers struct {
	Min         *int `json:"min,omitempty"`
	Max         *int `json:"max,omitempty"`
	IdleTimeout *int `json:"idleTimeout,omitempty"`
	InitTimeout *int `json:"initTimeout,omitempty"`
}

// Scaling is a union discriminated by Type.
type Scaling struct {
	Type         string  `json:"type"`
	QueueDelay   float64 `json:"queueDelay,omitempty"`
	RequestCount int     `json:"requestCount,omitempty"`
}

// TagRef is a tag as it appears on a resource.
type TagRef struct {
	ID    string `json:"id"`
	Key   string `json:"key"`
	Value string `json:"value"`
}

// Endpoint is a serverless endpoint as returned by the API.
type Endpoint struct {
	Container
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	Type          EndpointType      `json:"type"`
	RequestURLs   map[string]string `json:"requestUrls"`
	GPU           *GPUConfig        `json:"gpu"`
	Workers       Workers           `json:"workers"`
	Scaling       Scaling           `json:"scaling"`
	DataCenterIDs []string          `json:"dataCenterIds"`
	Timeout       int               `json:"timeout"` // ms
	Flashboot     string            `json:"flashboot"`
	CreatedAt     string            `json:"createdAt"`
	Version       *int              `json:"version"`
	Tags          []TagRef          `json:"tags"`
}

// CreateEndpointRequest creates an endpoint with inline container config (no template).
type CreateEndpointRequest struct {
	Container
	Name          string       `json:"name"`
	Type          EndpointType `json:"type"`
	GPU           *GPUConfig   `json:"gpu,omitempty"`
	Workers       *Workers     `json:"workers,omitempty"`
	Scaling       Scaling      `json:"scaling"`
	DataCenterIDs []string     `json:"dataCenterIds,omitempty"`
	Timeout       int          `json:"timeout,omitempty"` // ms
	Flashboot     string       `json:"flashboot,omitempty"`
}

// UpdateEndpointRequest patches an endpoint; only non-zero fields are sent.
type UpdateEndpointRequest struct {
	Container
	Name      string     `json:"name,omitempty"`
	GPU       *GPUConfig `json:"gpu,omitempty"`
	Workers   *Workers   `json:"workers,omitempty"`
	Scaling   *Scaling   `json:"scaling,omitempty"`
	Timeout   int        `json:"timeout,omitempty"`
	Flashboot string     `json:"flashboot,omitempty"`
}

// Worker is one serverless worker (one pod).
type Worker struct {
	ID            string  `json:"id"`
	Status        string  `json:"status"` // RUNNING, IDLE, INITIALIZING, THROTTLED, UNHEALTHY
	IsStale       bool    `json:"isStale"`
	Version       *int    `json:"version"`
	GPUTypeID     *string `json:"gpuTypeId"`
	DataCenterID  *string `json:"dataCenterId"`
	UptimeSeconds *int    `json:"uptimeSeconds"`
	Image         *string `json:"image"`
}

// WorkersResponse lists an endpoint's workers.
type WorkersResponse struct {
	Workers         []Worker       `json:"workers"`
	Summary         map[string]any `json:"summary"`
	EndpointVersion *int           `json:"endpointVersion"`
}

// Rollout summarises how far a new release has propagated.
type Rollout struct {
	InProgress      bool `json:"inProgress"`
	WorkersOnLatest int  `json:"workersOnLatest"`
	WorkersTotal    int  `json:"workersTotal"`
	PercentOnLatest int  `json:"percentOnLatest"`
}

// Release is one entry of an endpoint's release history.
type Release struct {
	ID          string `json:"id"`
	Version     *int   `json:"version"`
	Source      string `json:"source"`
	WorkerCount int    `json:"workerCount"`
	CreatedAt   string `json:"createdAt"`
	Diff        []struct {
		Field string `json:"field"`
		Old   any    `json:"old"`
		New   any    `json:"new"`
	} `json:"diff"`
}

// ReleasesResponse is the release history, newest first, plus the rollout state.
type ReleasesResponse struct {
	EndpointVersion *int      `json:"endpointVersion"`
	Rollout         Rollout   `json:"rollout"`
	Releases        []Release `json:"releases"`
}

// Tag is an account-level tag that can be attached to resources.
type Tag struct {
	ID    string `json:"id"`
	Key   string `json:"key"`
	Value string `json:"value"`
}

// GPUType is a catalog entry. Pool is the id used in GPUConfig.Pools.
type GPUType struct {
	ID     string  `json:"id"`
	Name   string  `json:"name"`
	Pool   *string `json:"pool"`
	Memory int     `json:"memory"` // VRAM in GB
	Price  struct {
		Serverless float64 `json:"serverless"` // USD per GPU per hour, list price
	} `json:"price"`
	Availability string `json:"availability"` // NONE, LOW, MEDIUM, HIGH (with include=AVAILABILITY)
}

type pagination struct {
	NextCursor  *string `json:"nextCursor"`
	HasNextPage bool    `json:"hasNextPage"`
}
