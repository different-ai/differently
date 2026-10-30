// Package metrics exposes Prometheus metrics for the control plane.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// Registry holds every Differently collector.
var Registry = prometheus.NewRegistry()

var (
	// Transitions counts sandbox state changes.
	Transitions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "differently_sandbox_transitions_total", Help: "Sandbox state transitions.",
	}, []string{"to", "pool"})
	// Placements counts placement outcomes.
	Placements = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "differently_placement_decisions_total", Help: "Placement decisions by outcome.",
	}, []string{"outcome", "pool"})
	// ProvisionSeconds measures time from activation to running.
	ProvisionSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "differently_provision_seconds", Help: "Time to provision a sandbox.",
		Buckets: []float64{.05, .1, .25, .5, 1, 2, 5, 10, 30, 60, 120},
	}, []string{"pool"})
	// QueueSeconds measures time spent queued before activation.
	QueueSeconds = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name: "differently_queue_wait_seconds", Help: "Time sandboxes wait in the queue.",
		Buckets: prometheus.ExponentialBuckets(0.01, 3, 12),
	})
	// Requests counts API requests.
	Requests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "differently_api_requests_total", Help: "API requests by route and status.",
	}, []string{"route", "status"})
	// RequestSeconds measures API latency.
	RequestSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "differently_api_request_seconds", Help: "API request latency.", Buckets: prometheus.DefBuckets,
	}, []string{"route"})
	// AuditFailures counts audit events that could not be stored.
	AuditFailures = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "differently_audit_append_failures_total", Help: "Audit events that could not be appended to the chain.",
	})
	// Reconciled counts reconciler corrections.
	Reconciled = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "differently_reconcile_actions_total", Help: "Corrections made by the reconciler.",
	}, []string{"action", "pool"})
	// PoolSlots is each pool's slot use: held, released (paused sandboxes
	// that gave their slot back), waiting_resume and limit.
	PoolSlots = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "differently_pool_slots", Help: "Pool slots by state: held, released, waiting_resume, limit.",
	}, []string{"pool", "state"})
	// Overcommitted is how many slots a pool holds beyond its limit (a
	// backend woke sandboxes on its own, for example through its gateway).
	Overcommitted = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "differently_pool_overcommitted", Help: "Slots held beyond the pool's maxConcurrency.",
	}, []string{"pool"})
	// ResumesQueued counts resumes that had to wait for a slot.
	ResumesQueued = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "differently_resumes_queued_total", Help: "Resumes that waited for a free slot.",
	}, []string{"pool"})
	// ResumeWaitSeconds measures how long queued resumes waited.
	ResumeWaitSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "differently_resume_wait_seconds", Help: "Time a queued resume waited for a slot.",
		Buckets: []float64{1, 5, 15, 30, 60, 120, 300, 600, 1800, 3600},
	}, []string{"pool"})
	// PauseSeconds measures how long the backend took to pause: on a
	// checkpointing pool, the time until the paused sandbox frees its slot
	// (internal/capsim's PauseTime).
	PauseSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "differently_pause_seconds", Help: "Time a pause took on its backend.",
		Buckets: []float64{1, 5, 15, 30, 45, 60, 90, 120, 180, 300, 600, 900},
	}, []string{"pool"})
	// Preemptions counts sandboxes paused to make room on a full pool.
	Preemptions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "differently_preemptions_total", Help: "Sandboxes paused to make room for waiting work.",
	}, []string{"pool", "mode"})
	// LifecycleActions counts the scheduler's own pauses and deletions:
	// preempted, max_run, max_paused.
	LifecycleActions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "differently_lifecycle_actions_total", Help: "Pauses and deletions by lifecycle limit or preemption.",
	}, []string{"action", "pool"})
)

func init() {
	Registry.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	Registry.MustRegister(Transitions, Placements, ProvisionSeconds, QueueSeconds, Requests, RequestSeconds, Reconciled, AuditFailures,
		PoolSlots, Overcommitted, ResumesQueued, ResumeWaitSeconds, PauseSeconds, Preemptions, LifecycleActions)
}
