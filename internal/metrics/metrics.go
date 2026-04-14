// Package metrics registers per-pool Prometheus gauges and counters
// and serves the /metrics HTTP endpoint.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/Yooouuuuuuu/flowdispatch/internal/broker"
)

// Collector wraps the broker and implements prometheus.Collector so gauges
// are read fresh on every scrape.
type Collector struct {
	b *broker.Broker

	active    *prometheus.Desc
	idle      *prometheus.Desc
	queued    *prometheus.Desc
	conns     *prometheus.Desc
	completed *prometheus.Desc
	errors    *prometheus.Desc
}

func NewCollector(ns string, b *broker.Broker) *Collector {
	labels := []string{"pool"}
	return &Collector{
		b: b,
		active:    prometheus.NewDesc(ns+"_pool_active", "Sessions currently processing a job.", labels, nil),
		idle:      prometheus.NewDesc(ns+"_pool_idle", "Sessions connected and waiting for a job.", labels, nil),
		queued:    prometheus.NewDesc(ns+"_pool_queued", "Jobs waiting in the priority queue.", labels, nil),
		conns:     prometheus.NewDesc(ns+"_pool_conns", "Configured number of backend connections.", labels, nil),
		completed: prometheus.NewDesc(ns+"_pool_jobs_completed_total", "Total jobs completed successfully.", labels, nil),
		errors:    prometheus.NewDesc(ns+"_pool_jobs_errors_total", "Total jobs that returned an error.", labels, nil),
	}
}

func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.active
	ch <- c.idle
	ch <- c.queued
	ch <- c.conns
	ch <- c.completed
	ch <- c.errors
}

func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	for _, m := range c.b.Metrics() {
		l := m.Name
		ch <- prometheus.MustNewConstMetric(c.active, prometheus.GaugeValue, float64(m.Active), l)
		ch <- prometheus.MustNewConstMetric(c.idle, prometheus.GaugeValue, float64(m.Idle), l)
		ch <- prometheus.MustNewConstMetric(c.queued, prometheus.GaugeValue, float64(m.Queued), l)
		ch <- prometheus.MustNewConstMetric(c.conns, prometheus.GaugeValue, float64(m.Conns), l)
		ch <- prometheus.MustNewConstMetric(c.completed, prometheus.CounterValue, float64(m.Completed), l)
		ch <- prometheus.MustNewConstMetric(c.errors, prometheus.CounterValue, float64(m.Errors), l)
	}
}

// Handler returns an HTTP handler for /metrics that uses a dedicated registry
// (no default Go runtime metrics mixed in unless you want them).
// ns is used as the Prometheus metric name prefix (e.g. "myapp" → "myapp_pool_active").
func Handler(ns string, b *broker.Broker) http.Handler {
	reg := prometheus.NewRegistry()
	reg.MustRegister(NewCollector(ns, b))
	return promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
}
