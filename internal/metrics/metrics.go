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
)

func init() {
	Registry.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	Registry.MustRegister(Transitions, Placements, ProvisionSeconds, QueueSeconds, Requests, RequestSeconds, Reconciled, AuditFailures)
}
