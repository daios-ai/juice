// SPDX-License-Identifier: AGPL-3.0-only

package main

// The operator's metrics (D20): what one kernel is doing, in Prometheus's text format, on an address
// of its own that is off unless configured. Every label has a fixed vocabulary, so nothing exported
// names a user, a peer, an action or a record, and the series a kernel exports cannot grow with what
// it serves. Counters live in memory and start again at a restart, which Prometheus's rate() reads
// as a reset; everything durable is read from the kernel's own books when scraped.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/daios-ai/juice/kernel"
)

// callBuckets span what a call may take here: milliseconds for a local one, hours for a remote one
// whose receipt is awaited (U35). The library's default stops at ten seconds and would put every
// such call in one bucket.
var callBuckets = []float64{0.1, 0.5, 1, 5, 30, 300, 3600, 86400}

type metrics struct {
	reg      *prometheus.Registry
	books    *bookCollector
	calls    *prometheus.CounterVec
	duration *prometheus.HistogramVec
	requests *prometheus.CounterVec
	retries  prometheus.Counter
}

// newMetrics builds the registry: the standard process and Go collectors, the build, the counters
// the kernel and transport report into, and the gauges read from the books on each scrape. The
// transport is built after the registry, because it reports into it; its peer count is attached with
// watchPeers before anything is served.
func newMetrics(backlog func(context.Context) (*kernel.Backlog, error)) *metrics {
	m := &metrics{
		reg:   prometheus.NewRegistry(),
		books: &bookCollector{backlog: backlog},
		calls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "juice_calls_total", Help: "Calls committed, by where they crossed and how they ended.",
		}, []string{"scope", "outcome"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "juice_call_duration_seconds", Help: "Time from a call's start to its settlement.",
			Buckets: callBuckets,
		}, []string{"scope", "outcome"}),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "juice_federation_requests_total", Help: "Federation requests, by protocol, direction and whether they completed.",
		}, []string{"protocol", "direction", "result"}),
		retries: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "juice_federation_retries_total", Help: "Re-dispatches of calls awaiting a peer's receipt.",
		}),
	}
	build := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "juice_build_info", Help: "The build this kernel runs.", ConstLabels: prometheus.Labels{"version": version},
	})
	build.Set(1)
	m.reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		collectors.NewGoCollector(), build, m.calls, m.duration, m.requests, m.retries, m.books)
	return m
}

// watchPeers attaches the transport's connection count. Called once, before serveMetrics.
func (m *metrics) watchPeers(peers func() int) { m.books.peers = peers }

// CallSettled is the kernel's report of one committed call (kernel.Metrics).
func (m *metrics) CallSettled(scope, outcome string, elapsed time.Duration) {
	m.calls.WithLabelValues(scope, outcome).Inc()
	m.duration.WithLabelValues(scope, outcome).Observe(elapsed.Seconds())
}

// observeRequest is the transport's report of one request (fed.Config.Observe).
func (m *metrics) observeRequest(protocol, direction string, ok bool) {
	result := "error"
	if ok {
		result = "ok"
	}
	m.requests.WithLabelValues(protocol, direction, result).Inc()
}

// countRetries wraps the retry worker's one re-dispatch, so every retry is counted where it happens.
func (m *metrics) countRetries(retry func(context.Context, *kernel.Trace) error) func(context.Context, *kernel.Trace) error {
	return func(ctx context.Context, tr *kernel.Trace) error {
		m.retries.Inc()
		return retry(ctx, tr)
	}
}

var (
	descPeers   = prometheus.NewDesc("juice_federation_peers_connected", "Peers this kernel holds a connection to.", nil, nil)
	descPending = prometheus.NewDesc("juice_federation_pending_remote_calls", "Calls dispatched abroad awaiting the peer's signed receipt.", nil, nil)
	descOldest  = prometheus.NewDesc("juice_federation_pending_oldest_age_seconds", "Age of the oldest open item: a call awaiting a receipt, or a served call not yet paid for. Zero when none.", []string{"kind"}, nil)
	descHalted  = prometheus.NewDesc("juice_rail_halted", "1 while outgoing rail work is halted by a blocked payment.", nil, nil)
	descSolvent = prometheus.NewDesc("juice_rail_solvency_ok", "1 while every credit on the books is backed by money that crossed in.", nil, nil)
)

// bookCollector reads the kernel's open work and rail standing at scrape time. A read that fails
// fails the scrape, so a kernel whose books cannot be read never reports itself idle and solvent.
type bookCollector struct {
	backlog func(context.Context) (*kernel.Backlog, error)
	peers   func() int
}

func (c *bookCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{descPeers, descPending, descOldest, descHalted, descSolvent} {
		ch <- d
	}
}

func (c *bookCollector) Collect(ch chan<- prometheus.Metric) {
	peers := 0
	if c.peers != nil {
		peers = c.peers()
	}
	ch <- prometheus.MustNewConstMetric(descPeers, prometheus.GaugeValue, float64(peers))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	b, err := c.backlog(ctx)
	if err == nil && b == nil {
		err = errors.New("no backlog read")
	}
	if err != nil {
		ch <- prometheus.NewInvalidMetric(descPending, fmt.Errorf("reading the books: %w", err))
		return
	}
	now := time.Now()
	age := func(t time.Time) float64 {
		if t.IsZero() {
			return 0
		}
		return now.Sub(t).Seconds()
	}
	ch <- prometheus.MustNewConstMetric(descPending, prometheus.GaugeValue, float64(b.RemoteCalls))
	ch <- prometheus.MustNewConstMetric(descOldest, prometheus.GaugeValue, age(b.OldestRemoteCall), "remote_call")
	ch <- prometheus.MustNewConstMetric(descOldest, prometheus.GaugeValue, age(b.OldestObligation), "obligation")
	ch <- prometheus.MustNewConstMetric(descHalted, prometheus.GaugeValue, boolGauge(b.RailHalted))
	ch <- prometheus.MustNewConstMetric(descSolvent, prometheus.GaugeValue, boolGauge(b.SolvencyGap == 0))
}

func boolGauge(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// serveMetrics binds the metrics address and serves /metrics there and nothing else, returning the
// address it bound. The bind is done here, before serving, so an address that cannot be held refuses
// the boot naming its setting.
func serveMetrics(addr string, reg *prometheus.Registry) (*http.Server, string, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, "", fmt.Errorf("metrics_listen_addr %s: %w", addr, err)
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	return srv, ln.Addr().String(), nil
}
