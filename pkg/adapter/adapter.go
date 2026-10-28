// Package adapter is the versioned contract between the Differently control
// plane and execution backends.
//
// An ExecutionAdapter creates and manages sandbox instances on one backend
// (an OpenSandbox server, an external provider API, a community plugin). The
// required interface is deliberately small; optional behaviour is expressed by
// implementing the optional interfaces below (FileSystem, Lifecycle, Pauser,
// Snapshotter, Streamer, PortExposer). The control plane advertises a feature
// only when the adapter both declares it and implements the interface, so a
// request for an unsupported operation fails with a precise error instead of
// silently degrading.
//
// Adapters run in-process (built in) or out-of-process behind the HTTP
// protocol in package adapterhttp.
package adapter

import (
	"context"
	"slices"

	"github.com/different-ai/differently/pkg/api"
)

// ProtocolVersion is the adapter contract version. Breaking changes bump it.
const ProtocolVersion = "1"

// Feature names advertised in Capabilities.Features and requested through
// CreateSandboxRequest.Capabilities.
const (
	FeatureExec          = "exec"
	FeatureExecStream    = "exec.stream"
	FeatureFiles         = "files"
	FeatureStopStart     = "lifecycle.stop-start"
	FeaturePauseResume   = "lifecycle.pause-resume"
	FeatureSnapshotDisk  = "snapshots.disk"
	FeatureSnapshotMem   = "snapshots.memory"
	FeatureFork          = "fork"
	FeaturePersistDisk   = "persistence.disk"
	FeaturePorts         = "ports.expose"
	FeatureSSH           = "ssh"
	FeaturePTY           = "pty"
	FeatureEgressControl = "network.egress-control"
	FeatureGPU           = "gpu"
	FeatureArchAMD64     = "arch.amd64"
	FeatureArchARM64     = "arch.arm64"
	FeatureMicroVM       = "isolation.microvm"
	FeatureContainer     = "isolation.container"
	FeatureNoIsolation   = "isolation.none"
)

// Well-known labels adapters attach to backend resources so the reconciler can
// find what this installation owns.
const (
	LabelSandboxID    = "differently.dev/sandbox-id"
	LabelInstallation = "differently.dev/installation"
	LabelOrg          = "differently.dev/org"
	LabelProject      = "differently.dev/project"
)

// Instance states reported by adapters.
const (
	StatePending = "pending"
	StateRunning = "running"
	StateStopped = "stopped"
	StatePaused  = "paused"
	StateError   = "error"
)

// CodeInsufficientCapacity signals a transient lack of capacity. The scheduler
// requeues the sandbox without marking the pool unhealthy.
const CodeInsufficientCapacity = "insufficient_capacity"

// Info identifies an adapter implementation.
type Info struct {
	Kind        string `json:"kind"`
	Version     string `json:"version"`
	Trust       string `json:"trust"`
	Isolation   string `json:"isolation"`
	Description string `json:"description,omitempty"`
	Protocol    string `json:"protocol"`
}

// Capabilities is what an adapter can do and the envelope it accepts.
type Capabilities struct {
	Features     []string `json:"features"`
	MaxCPU       int      `json:"maxCPU,omitempty"`
	MaxMemoryMiB int      `json:"maxMemoryMiB,omitempty"`
	MaxDiskGiB   int      `json:"maxDiskGiB,omitempty"`
	Templates    []string `json:"templates,omitempty"` // empty means any
	Regions      []string `json:"regions,omitempty"`
}

// Has reports whether a feature is advertised.
func (c Capabilities) Has(feature string) bool { return slices.Contains(c.Features, feature) }

// Missing returns the requested features that are not advertised.
func (c Capabilities) Missing(requested []string) []string {
	var out []string
	for _, f := range requested {
		if !c.Has(f) {
			out = append(out, f)
		}
	}
	return out
}

// Accepts reports whether a resource envelope and template fit the limits.
func (c Capabilities) Accepts(r api.Resources, template string) (bool, string) {
	switch {
	case c.MaxCPU > 0 && r.CPU > c.MaxCPU:
		return false, "cpu above adapter maximum"
	case c.MaxMemoryMiB > 0 && r.MemoryMiB > c.MaxMemoryMiB:
		return false, "memory above adapter maximum"
	case c.MaxDiskGiB > 0 && r.DiskGiB > c.MaxDiskGiB:
		return false, "disk above adapter maximum"
	case template != "" && len(c.Templates) > 0 && !slices.Contains(c.Templates, template):
		return false, "template " + template + " not available"
	}
	return true, ""
}

// Capacity is the free capacity an adapter currently reports. Unbounded
// adapters (most external providers) rely on the pool's concurrency cap.
type Capacity struct {
	Bounded        bool `json:"bounded"`
	TotalCPU       int  `json:"totalCPU,omitempty"`
	FreeCPU        int  `json:"freeCPU,omitempty"`
	TotalMemoryMiB int  `json:"totalMemoryMiB,omitempty"`
	FreeMemoryMiB  int  `json:"freeMemoryMiB,omitempty"`
	Nodes          int  `json:"nodes,omitempty"`
	// LargestFreeCPU and LargestFreeMemoryMiB describe the roomiest single
	// node, so fragmented capacity is not mistaken for room.
	LargestFreeCPU       int `json:"largestFreeCPU,omitempty"`
	LargestFreeMemoryMiB int `json:"largestFreeMemoryMiB,omitempty"`
}

// Fits reports whether one sandbox of r fits.
func (c Capacity) Fits(r api.Resources) bool {
	if !c.Bounded {
		return true
	}
	cpu, mem := c.FreeCPU, c.FreeMemoryMiB
	if c.LargestFreeCPU > 0 || c.LargestFreeMemoryMiB > 0 {
		cpu, mem = c.LargestFreeCPU, c.LargestFreeMemoryMiB
	}
	return r.CPU <= cpu && r.MemoryMiB <= mem
}

// CreateRequest asks an adapter for a new instance. ID is the Differently
// sandbox ID and is the idempotency key: creating the same ID twice must
// return the existing instance, never a second one.
type CreateRequest struct {
	ID           string            `json:"id"`
	Installation string            `json:"installation"`
	Template     string            `json:"template,omitempty"`
	Resources    api.Resources     `json:"resources"`
	Env          map[string]string `json:"env,omitempty"`
	Labels       map[string]string `json:"labels,omitempty"`
	TTLSeconds   int               `json:"ttlSeconds,omitempty"`
	FromSnapshot string            `json:"fromSnapshot,omitempty"`
}

// Instance is a backend resource backing a sandbox.
type Instance struct {
	Ref       string `json:"ref"`
	SandboxID string `json:"sandboxId"`
	State     string `json:"state"`
	Region    string `json:"region,omitempty"`
	Message   string `json:"message,omitempty"`
}

// Snapshot is a backend snapshot reference.
type Snapshot struct {
	Ref       string `json:"ref"`
	Kind      string `json:"kind"`
	SizeBytes int64  `json:"sizeBytes,omitempty"`
}

// ExecutionAdapter is the required contract every backend implements.
type ExecutionAdapter interface {
	Info() Info
	Capabilities(ctx context.Context) (Capabilities, error)
	Capacity(ctx context.Context) (Capacity, error)
	Health(ctx context.Context) error
	Create(ctx context.Context, req CreateRequest) (Instance, error)
	Get(ctx context.Context, ref string) (Instance, error)
	// List returns instances owned by the given installation.
	List(ctx context.Context, installation string) ([]Instance, error)
	// Delete is idempotent: deleting a missing instance succeeds.
	Delete(ctx context.Context, ref string) error
	Exec(ctx context.Context, ref string, req api.ExecRequest) (api.ExecResult, error)
}

// Streamer streams command output as it is produced.
type Streamer interface {
	ExecStream(ctx context.Context, ref string, req api.ExecRequest, emit func(api.ExecEvent) error) (exitCode int, err error)
}

// FileSystem reads and writes files inside an instance.
type FileSystem interface {
	ReadFile(ctx context.Context, ref, path string) ([]byte, error)
	WriteFile(ctx context.Context, ref, path string, data []byte) error
	ListFiles(ctx context.Context, ref, path string) ([]api.FileInfo, error)
}

// Lifecycle stops an instance while keeping its disk, and starts it again.
type Lifecycle interface {
	Stop(ctx context.Context, ref string) error
	Start(ctx context.Context, ref string) error
}

// Pauser freezes an instance in memory and resumes it.
type Pauser interface {
	Pause(ctx context.Context, ref string) error
	Resume(ctx context.Context, ref string) error
}

// Snapshotter captures instance state. Restoring happens through
// CreateRequest.FromSnapshot on the same adapter.
type Snapshotter interface {
	Snapshot(ctx context.Context, ref, kind string) (Snapshot, error)
	DeleteSnapshot(ctx context.Context, snapshotRef string) error
}

// PortExposer returns a reachable URL for a service port.
type PortExposer interface {
	ExposePort(ctx context.Context, ref string, port int) (string, error)
}

// EffectiveFeatures returns declared features that the adapter actually
// implements. Declaring "files" without implementing FileSystem drops it.
func EffectiveFeatures(a ExecutionAdapter, declared []string) []string {
	implements := func(f string) bool {
		switch f {
		case FeatureExecStream:
			_, ok := a.(Streamer)
			return ok
		case FeatureFiles:
			_, ok := a.(FileSystem)
			return ok
		case FeatureStopStart, FeaturePersistDisk:
			_, ok := a.(Lifecycle)
			return ok
		case FeaturePauseResume:
			_, ok := a.(Pauser)
			return ok
		case FeatureSnapshotDisk, FeatureSnapshotMem, FeatureFork:
			_, ok := a.(Snapshotter)
			return ok
		case FeaturePorts:
			_, ok := a.(PortExposer)
			return ok
		}
		return true
	}
	out := make([]string, 0, len(declared))
	for _, f := range declared {
		if implements(f) && !slices.Contains(out, f) {
			out = append(out, f)
		}
	}
	slices.Sort(out)
	return out
}

// NotFound is the error adapters return for a missing instance.
func NotFound(ref string) error {
	return api.Errorf(api.CodeNotFound, "instance %s not found", ref)
}

// Unsupported is the error for an operation a backend cannot perform.
func Unsupported(op string) error {
	return api.Errorf(api.CodeUnsupportedOperation, "operation %s is not supported by this backend", op).With("operation", op)
}

// Unavailable wraps a backend failure that should count against pool health.
func Unavailable(err error) error {
	return api.Errorf(api.CodeProviderUnavailable, "backend unavailable: %v", err)
}

// InsufficientCapacity is returned when the backend has no room right now.
func InsufficientCapacity(msg string) error {
	return &api.Error{Code: CodeInsufficientCapacity, Message: msg}
}

// OwnershipLabels returns the labels adapters should stamp on resources.
func OwnershipLabels(req CreateRequest) map[string]string {
	out := map[string]string{}
	for k, v := range req.Labels {
		out[k] = v
	}
	out[LabelSandboxID] = req.ID
	out[LabelInstallation] = req.Installation
	return out
}
