package labeler

import "github.com/prometheus/client_golang/prometheus"

const namespace = "netbox_zone_labeler"

type metrics struct {
	labeled     prometheus.Counter
	errors      *prometheus.CounterVec
	lookup      *prometheus.HistogramVec
	withoutZone prometheus.Gauge
	lastPass    prometheus.Gauge
}

func newMetrics(reg prometheus.Registerer, queueLen func() int) *metrics {
	m := &metrics{
		labeled: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace, Name: "nodes_labeled_total",
			Help: "Nodes whose zone label was set or changed.",
		}),
		errors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "errors_total",
			Help: "Failures by reason: netbox (transient, retried), patch (retried), invalid_label, ambiguous.",
		}, []string{"reason"}),
		lookup: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace, Name: "lookup_duration_seconds",
			Help:    "Duration of one NetBox rack lookup (all HTTP calls and retries) by result: found, miss, error.",
			Buckets: prometheus.DefBuckets,
		}, []string{"result"}),
		withoutZone: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace, Name: "nodes_without_zone",
			Help: "Non-excluded nodes that currently carry no zone label.",
		}),
		lastPass: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace, Name: "last_full_pass_timestamp_seconds",
			Help: "Unix time of the last full pass over all nodes.",
		}),
	}
	// Pre-create the reason series so a first error shows as 0 -> 1.
	for _, r := range []string{"netbox", "patch", "invalid_label", "ambiguous"} {
		m.errors.WithLabelValues(r)
	}
	for _, r := range []string{"found", "miss", "error"} {
		m.lookup.WithLabelValues(r)
	}
	reg.MustRegister(m.labeled, m.errors, m.lookup, m.withoutZone, m.lastPass,
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Namespace: namespace, Name: "queue_depth",
			Help: "Nodes waiting to be looked up.",
		}, func() float64 { return float64(queueLen()) }))
	return m
}
