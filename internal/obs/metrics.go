// file: internal/obs/metrics.go
package obs

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Every metric here exists to answer a specific operational question during a contest.
// Metrics that answer no question are noise; do not add any.
var (
	// "Is the API healthy while the judges are saturated?" - the two must be independent.
	HTTPDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "arena_http_request_duration_seconds",
		Help:    "API latency. Must stay flat regardless of judging load.",
		Buckets: []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5},
	}, []string{"route", "method", "status"})

	// "How far behind are we?" - the KEDA autoscaling signal and the ETA shown to users.
	QueueBacklog = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "arena_queue_backlog",
		Help: "Jobs published but not yet delivered to any runner.",
	})
	QueuePending = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "arena_queue_pending",
		Help: "Jobs claimed by a runner and not yet acked (in flight).",
	})

	// "How long does a submission wait before a runner starts it?"
	QueueWait = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "arena_queue_wait_seconds",
		Help:    "Time from enqueue to a runner claiming the job.",
		Buckets: []float64{.05, .1, .25, .5, 1, 2, 5, 10, 30, 60},
	})

	// "How long does judging itself take, and is it stable?"
	JudgeDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "arena_judge_duration_seconds",
		Help:    "End-to-end judging time per submission.",
		Buckets: []float64{.1, .25, .5, 1, 2, 5, 10, 30},
	}, []string{"language"})

	SandboxSetup = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "arena_sandbox_setup_seconds",
		Help:    "Container create+start overhead. Justifies the warm pool.",
		Buckets: []float64{.01, .025, .05, .1, .25, .5, 1, 2},
	})

	// "What are participants actually hitting?" - a spike in IE means WE are broken.
	Verdicts = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "arena_verdicts_total",
		Help: "Verdicts issued, by verdict and language.",
	}, []string{"verdict", "language"})

	OOMKills = promauto.NewCounter(prometheus.CounterOpts{
		Name: "arena_oom_kills_total",
		Help: "Sandbox OOM kills. Proves memory limits are actually enforced.",
	})
	WallKills = promauto.NewCounter(prometheus.CounterOpts{
		Name: "arena_wall_kills_total",
		Help: "Wall-clock kills. Proves the infinite-loop guard fires.",
	})
	LeaseReclaims = promauto.NewCounter(prometheus.CounterOpts{
		Name: "arena_lease_reclaims_total",
		Help: "Jobs taken over from a dead or preempted runner.",
	})
	DeadLettered = promauto.NewCounter(prometheus.CounterOpts{
		Name: "arena_dead_lettered_total",
		Help: "Poison-pill submissions abandoned after max attempts.",
	})
	CacheHits = promauto.NewCounter(prometheus.CounterOpts{
		Name: "arena_verdict_cache_hits_total",
		Help: "Submissions served from the verdict cache without executing anything.",
	})
	RateLimited = promauto.NewCounter(prometheus.CounterOpts{
		Name: "arena_rate_limited_total",
		Help: "Submissions rejected by the token bucket or the in-flight lock.",
	})
	RunnerSlotsBusy = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "arena_runner_slots_busy",
		Help: "Occupied judging slots per runner.",
	}, []string{"runner"})
)
