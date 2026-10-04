// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/daios-ai/juice/kernel"
)

// scrape reads the registry the way Prometheus does, returning the status and the exposition.
func scrape(t *testing.T, m *metrics) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{}).ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	return rec.Code, rec.Body.String()
}

func quietBooks(context.Context) (*kernel.Backlog, error) { return &kernel.Backlog{}, nil }

// What the kernel and the transport report is exported under fixed labels alone: nothing a scrape
// returns can name a user, a peer, an action or a record, whatever the kernel served.
func TestMetricsExportOnlyFixedLabels(t *testing.T) {
	m := newMetrics(quietBooks)
	m.watchPeers(func() int { return 2 })
	m.CallSettled("local", "success", 40*time.Millisecond)
	m.CallSettled("outbound", "failure", 2*time.Hour)
	m.observeRequest("/juice/fed/call/1", "out", true)
	m.observeRequest("/juice/fed/gossip/1", "in", false)
	retried := 0
	_ = m.countRetries(func(context.Context, *kernel.Trace) error { retried++; return nil })(context.Background(), &kernel.Trace{})

	code, body := scrape(t, m)
	if code != http.StatusOK {
		t.Fatalf("scrape: %d\n%s", code, body)
	}
	for _, want := range []string{
		`juice_build_info{version="` + version + `"} 1`,
		`juice_calls_total{outcome="success",scope="local"} 1`,
		`juice_calls_total{outcome="failure",scope="outbound"} 1`,
		// A two-hour call lands below the day bucket and above the hour one: the buckets reach as far
		// as a remote call may lawfully wait.
		`juice_call_duration_seconds_bucket{outcome="failure",scope="outbound",le="3600"} 0`,
		`juice_call_duration_seconds_bucket{outcome="failure",scope="outbound",le="86400"} 1`,
		`juice_federation_requests_total{direction="out",protocol="/juice/fed/call/1",result="ok"} 1`,
		`juice_federation_requests_total{direction="in",protocol="/juice/fed/gossip/1",result="error"} 1`,
		`juice_federation_retries_total 1`,
		`juice_federation_peers_connected 2`,
		`process_start_time_seconds`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("scrape lacks %s", want)
		}
	}
	if retried != 1 {
		t.Errorf("the counted retry must still run once, ran %d times", retried)
	}
	allowed := map[string]bool{"scope": true, "outcome": true, "protocol": true, "direction": true,
		"result": true, "kind": true, "version": true, "le": true}
	label := regexp.MustCompile(`([a-z_]+)="`)
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "juice_") {
			continue
		}
		for _, l := range label.FindAllStringSubmatch(line, -1) {
			if !allowed[l[1]] {
				t.Errorf("label %q is outside the fixed vocabulary: %s", l[1], line)
			}
		}
	}
}

// The gauges are the books' own: the age of the oldest open item of each kind, the halt, and
// whether the books balance.
func TestMetricsGaugesReadTheBooks(t *testing.T) {
	m := newMetrics(func(context.Context) (*kernel.Backlog, error) {
		return &kernel.Backlog{RemoteCalls: 3, OldestRemoteCall: time.Now().Add(-time.Hour),
			RailHalted: true, SolvencyGap: 5}, nil
	})
	_, body := scrape(t, m)
	for _, want := range []string{
		`juice_federation_pending_remote_calls 3`,
		`juice_federation_pending_oldest_age_seconds{kind="obligation"} 0`,
		`juice_rail_halted 1`,
		`juice_rail_solvency_ok 0`,
		`juice_federation_peers_connected 0`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("scrape lacks %s\n%s", want, body)
		}
	}
	age := regexp.MustCompile(`juice_federation_pending_oldest_age_seconds\{kind="remote_call"\} ([0-9.e+]+)`).FindStringSubmatch(body)
	if age == nil || !strings.HasPrefix(age[1], "3600") && !strings.HasPrefix(age[1], "3.6") {
		t.Errorf("the oldest parked call is an hour old: %v", age)
	}
}

// Books that cannot be read fail the scrape: a kernel must never report itself idle and solvent
// because it could not look.
func TestMetricsScrapeFailsWhenTheBooksCannotBeRead(t *testing.T) {
	m := newMetrics(func(context.Context) (*kernel.Backlog, error) { return nil, errors.New("database is locked") })
	if code, body := scrape(t, m); code != http.StatusInternalServerError || strings.Contains(body, "juice_rail_solvency_ok 1") {
		t.Errorf("an unreadable book must fail the scrape, got %d\n%s", code, body)
	}
}

// The metrics address answers /metrics and nothing else, and an address that cannot be bound names
// the setting that chose it.
func TestServeMetricsAnswersOnlyMetrics(t *testing.T) {
	m := newMetrics(quietBooks)
	srv, addr, err := serveMetrics("127.0.0.1:0", m.reg)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	get := func(path string) int {
		resp, err := http.Get("http://" + addr + path)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := get("/metrics"); code != http.StatusOK {
		t.Errorf("/metrics: %d", code)
	}
	if code := get("/health"); code != http.StatusNotFound {
		t.Errorf("the metrics address must serve nothing but /metrics, /health answered %d", code)
	}
	if _, _, err := serveMetrics(addr, m.reg); err == nil || !strings.Contains(err.Error(), "metrics_listen_addr") {
		t.Errorf("a held address must refuse naming its setting, got %v", err)
	}
}
