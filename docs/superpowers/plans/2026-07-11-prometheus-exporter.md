# Prometheus Exporter Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `-mode exporter` to the tinytwamp binary, exposing TWAMP-Light RTT/jitter/loss/reflector metrics via a Prometheus `/metrics` HTTP endpoint with three selectable probe modes (background, scrape, dual) and optional TLS.

**Architecture:** A new `PrometheusStore` in `prom.go` wraps `prometheus/client_golang` GaugeVec and CounterVec registrations; `runExporter` in `exporter.go` orchestrates the config poller, TWAMP-Light server, probe scheduler (background/dual), and HTTP server — mirroring the existing `runAgent` pattern. The `Server` struct gains an atomic reflected-packet counter. Existing `runConfigPoller`, `runProbeScheduler`, `runBurst`, and `InfluxWriter` are reused without modification.

**Tech Stack:** Go 1.21+, `github.com/prometheus/client_golang` (first external dependency), stdlib `net/http` with `crypto/tls` for TLS.

## Global Constraints

- Go 1.21+, module `github.com/buraglio/tiny-twamp`
- First external dependency allowed: `github.com/prometheus/client_golang` (add to go.mod)
- CGO_ENABLED=0 — cross-compilation must work for all platforms
- Metrics endpoint default port: `9862`
- Measurement/metric prefix: `twamp_`
- Labels on probe metrics: `source`, `target`, `topology`, `site` (exact — match InfluxDB tags)
- Label on reflector metric: `source` only
- `twamp_loss_ratio` is 0.0–1.0 (NOT percent)
- Integer counters use `_total` suffix (Prometheus convention)
- `-probe-mode dual` requires a valid `influxdb` block — fatal if absent
- Only one of `-metrics-tls-cert`/`-metrics-tls-key` set → fatal
- TLS cert/key unreadable → fatal
- Config fetch failure at startup → fatal
- Config fetch failure on refresh → log warning, continue
- Scrape-mode probe failure → loss_ratio=1.0, RTT gauges=0, HTTP 200

---

### Task 1: Add `prometheus/client_golang` dependency and reflected-packet counter to `Server`

**Files:**
- Modify: `go.mod` (add dependency)
- Modify: `tinytwamp.go` (add `atomic.Uint64` counter to `Server`, `ReflectedCount()` accessor, increment in `handleTestPacket`)
- Test: `tinytwamp_test.go`

**Interfaces:**
- Produces:
  - `(s *Server) ReflectedCount() uint64` — returns total reflected packets since startup

- [ ] **Step 1: Write failing test for `ReflectedCount`**

Add to `tinytwamp_test.go` in the existing test file:

```go
// ============================================================================
// Server reflected counter
// ============================================================================

func TestServerReflectedCount(t *testing.T) {
	srv := NewServer(nil, newRateLimiter(0), &allowlist{}, true)
	if srv.ReflectedCount() != 0 {
		t.Errorf("initial ReflectedCount = %d, want 0", srv.ReflectedCount())
	}
	srv.reflectedPackets.Add(1)
	if srv.ReflectedCount() != 1 {
		t.Errorf("after Add(1) ReflectedCount = %d, want 1", srv.ReflectedCount())
	}
	srv.reflectedPackets.Add(99)
	if srv.ReflectedCount() != 100 {
		t.Errorf("after Add(99) ReflectedCount = %d, want 100", srv.ReflectedCount())
	}
}
```

- [ ] **Step 2: Run to verify it fails**

```bash
go test ./... -run TestServerReflectedCount -v
```

Expected: FAIL — `reflectedPackets` field and `ReflectedCount` method undefined.

- [ ] **Step 3: Add dependency**

```bash
go get github.com/prometheus/client_golang@latest
go mod tidy
```

Expected: `go.mod` and `go.sum` updated.

- [ ] **Step 4: Add `sync/atomic` counter to `Server` struct and wire it**

In `tinytwamp.go`, add `"sync/atomic"` to imports. Add `reflectedPackets atomic.Uint64` field to the `Server` struct:

```go
type Server struct {
	conn               *net.UDPConn
	logger             *log.Logger
	seqMu              sync.Mutex
	seqNumber          uint32
	ctx                context.Context
	cancel             context.CancelFunc
	wg                 sync.WaitGroup
	semaphore          chan struct{}
	rl                 *rateLimiter
	al                 *allowlist
	synced             bool
	reflectedPackets   atomic.Uint64
}
```

Add accessor after `NewServer`:

```go
func (s *Server) ReflectedCount() uint64 {
	return s.reflectedPackets.Load()
}
```

In `handleTestPacket`, add the increment immediately after the successful `WriteToUDP` call (after the `if err != nil` block that returns on failure):

```go
s.reflectedPackets.Add(1)
```

- [ ] **Step 5: Run to verify tests pass**

```bash
go test ./... -v -run TestServerReflectedCount
```

Expected: PASS.

- [ ] **Step 6: Full build and test**

```bash
go build ./...
go test ./...
GOOS=windows GOARCH=amd64 go build ./...
```

Expected: all pass, no errors.

- [ ] **Step 7: Commit**

```bash
git add go.mod go.sum tinytwamp.go tinytwamp_test.go
git commit -m "feat: add prometheus/client_golang dependency and Server reflected-packet counter"
```

---

### Task 2: `PrometheusStore` — metric registration, Update, IncrementReflected, HTTP handler (`prom.go`)

**Files:**
- Create: `prom.go`
- Modify: `tinytwamp_test.go` (add PrometheusStore tests)

**Interfaces:**
- Consumes: `ProbeResult` (from `agent.go` Task 1 of prior plan)
- Produces:
  - `type PrometheusStore struct`
  - `func newPrometheusStore(hostname string) *PrometheusStore`
  - `func (s *PrometheusStore) Update(r ProbeResult)`
  - `func (s *PrometheusStore) IncrementReflected()`
  - `func (s *PrometheusStore) Handler() http.Handler`

- [ ] **Step 1: Write failing tests**

Add to `tinytwamp_test.go`:

```go
// ============================================================================
// PrometheusStore
// ============================================================================

func TestPrometheusStoreUpdate(t *testing.T) {
	store := newPrometheusStore("probe-a")
	r := ProbeResult{
		Source:    "probe-a",
		Target:    "probe-b",
		Site:      "us-east",
		Topology:  "mesh",
		RttMin:    1 * time.Millisecond,
		RttAvg:    2 * time.Millisecond,
		RttMax:    3 * time.Millisecond,
		RttStddev: 500 * time.Microsecond,
		Jitter:    250 * time.Microsecond,
		LossPct:   20.0,
		Sent:      5,
		Recv:      4,
	}
	// Must not panic
	store.Update(r)
}

func TestPrometheusStoreLossRatioConversion(t *testing.T) {
	// LossPct=20.0 should become loss_ratio=0.2 in the gauge
	// We verify via the text exposition format
	store := newPrometheusStore("probe-a")
	r := ProbeResult{
		Source:   "probe-a",
		Target:   "probe-b",
		Site:     "east",
		Topology: "mesh",
		LossPct:  20.0,
		Sent:     5,
		Recv:     4,
	}
	store.Update(r)

	rec := httptest.NewRecorder()
	store.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()

	if !strings.Contains(body, `twamp_loss_ratio`) {
		t.Errorf("expected twamp_loss_ratio in metrics output, got:\n%s", body)
	}
	if !strings.Contains(body, "0.2") {
		t.Errorf("expected loss_ratio=0.2 (20%% -> 0.2), got:\n%s", body)
	}
}

func TestPrometheusStoreLabels(t *testing.T) {
	store := newPrometheusStore("probe-a")
	r := ProbeResult{
		Source:   "probe-a",
		Target:   "probe-b",
		Site:     "us-east",
		Topology: "hub-spoke",
		Sent:     5,
		Recv:     5,
	}
	store.Update(r)

	rec := httptest.NewRecorder()
	store.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()

	for _, want := range []string{
		`source="probe-a"`,
		`target="probe-b"`,
		`site="us-east"`,
		`topology="hub-spoke"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("expected label %q in metrics output", want)
		}
	}
}

func TestPrometheusStoreIncrementReflected(t *testing.T) {
	store := newPrometheusStore("probe-a")
	store.IncrementReflected()
	store.IncrementReflected()

	rec := httptest.NewRecorder()
	store.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()

	if !strings.Contains(body, "twamp_reflected_packets_total") {
		t.Errorf("expected twamp_reflected_packets_total in output, got:\n%s", body)
	}
	if !strings.Contains(body, "2") {
		t.Errorf("expected counter value 2 in output, got:\n%s", body)
	}
}
```

Also add `"net/http/httptest"` to the test file imports.

- [ ] **Step 2: Run to verify they fail**

```bash
go test ./... -run "TestPrometheusStore" -v
```

Expected: FAIL — `newPrometheusStore` undefined.

- [ ] **Step 3: Create `prom.go`**

```go
package main

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// PrometheusStore holds all registered TWAMP metrics and their registry.
type PrometheusStore struct {
	rttMin    *prometheus.GaugeVec
	rttAvg    *prometheus.GaugeVec
	rttMax    *prometheus.GaugeVec
	rttStddev *prometheus.GaugeVec
	jitter    *prometheus.GaugeVec
	lossRatio *prometheus.GaugeVec
	pktSent   *prometheus.CounterVec
	pktRecv   *prometheus.CounterVec
	reflected *prometheus.CounterVec
	registry  *prometheus.Registry
}

var probeLabels = []string{"source", "target", "topology", "site"}

func newPrometheusStore(hostname string) *PrometheusStore {
	reg := prometheus.NewRegistry()

	newGauge := func(name, help string) *prometheus.GaugeVec {
		g := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: name, Help: help}, probeLabels)
		reg.MustRegister(g)
		return g
	}
	newCounter := func(name, help string, labels []string) *prometheus.CounterVec {
		c := prometheus.NewCounterVec(prometheus.CounterOpts{Name: name, Help: help}, labels)
		reg.MustRegister(c)
		return c
	}

	return &PrometheusStore{
		rttMin:    newGauge("twamp_rtt_min_milliseconds", "Minimum RTT in burst (ms)"),
		rttAvg:    newGauge("twamp_rtt_avg_milliseconds", "Average RTT in burst (ms)"),
		rttMax:    newGauge("twamp_rtt_max_milliseconds", "Maximum RTT in burst (ms)"),
		rttStddev: newGauge("twamp_rtt_stddev_milliseconds", "RTT standard deviation (ms)"),
		jitter:    newGauge("twamp_jitter_milliseconds", "Mean absolute jitter (ms)"),
		lossRatio: newGauge("twamp_loss_ratio", "Packet loss ratio 0.0-1.0"),
		pktSent:   newCounter("twamp_packets_sent_total", "Cumulative packets sent", probeLabels),
		pktRecv:   newCounter("twamp_packets_received_total", "Cumulative packets received", probeLabels),
		reflected: newCounter("twamp_reflected_packets_total", "Packets reflected since startup", []string{"source"}),
		registry:  reg,
	}
}

// Update sets all probe gauges and adds to cumulative counters for one ProbeResult.
func (s *PrometheusStore) Update(r ProbeResult) {
	ms := func(d time.Duration) float64 { return float64(d.Microseconds()) / 1000.0 }
	labels := prometheus.Labels{
		"source":   r.Source,
		"target":   r.Target,
		"topology": r.Topology,
		"site":     r.Site,
	}
	s.rttMin.With(labels).Set(ms(r.RttMin))
	s.rttAvg.With(labels).Set(ms(r.RttAvg))
	s.rttMax.With(labels).Set(ms(r.RttMax))
	s.rttStddev.With(labels).Set(ms(r.RttStddev))
	s.jitter.With(labels).Set(ms(r.Jitter))
	s.lossRatio.With(labels).Set(r.LossPct / 100.0)
	s.pktSent.With(labels).Add(float64(r.Sent))
	s.pktRecv.With(labels).Add(float64(r.Recv))
}

// IncrementReflected adds 1 to the reflected-packets counter for this host.
func (s *PrometheusStore) IncrementReflected() {
	// source label is set at store creation via hostname; we use a fixed label here.
	// The label value is determined by how the metric was registered.
	s.reflected.WithLabelValues("self").Inc()
}

// Handler returns an http.Handler that serves the Prometheus text exposition format.
func (s *PrometheusStore) Handler() http.Handler {
	return promhttp.HandlerFor(s.registry, promhttp.HandlerOpts{})
}
```

**Note on `IncrementReflected`:** The `reflected` counter uses label value `"self"` as a placeholder for the hostname. The `newPrometheusStore(hostname string)` parameter will be used in Task 3 to pre-initialize the counter with the real hostname. Update `IncrementReflected` to store the hostname in the struct and use it:

```go
type PrometheusStore struct {
	// ... existing fields ...
	hostname  string
}

func newPrometheusStore(hostname string) *PrometheusStore {
	// ... existing code ...
	store := &PrometheusStore{
		// ... existing fields ...
		hostname: hostname,
	}
	// Pre-initialize reflected counter so it appears at zero before any packets arrive
	store.reflected.WithLabelValues(hostname)
	return store
}

func (s *PrometheusStore) IncrementReflected() {
	s.reflected.WithLabelValues(s.hostname).Inc()
}
```

Rewrite `prom.go` with this complete version:

```go
package main

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type PrometheusStore struct {
	rttMin    *prometheus.GaugeVec
	rttAvg    *prometheus.GaugeVec
	rttMax    *prometheus.GaugeVec
	rttStddev *prometheus.GaugeVec
	jitter    *prometheus.GaugeVec
	lossRatio *prometheus.GaugeVec
	pktSent   *prometheus.CounterVec
	pktRecv   *prometheus.CounterVec
	reflected *prometheus.CounterVec
	registry  *prometheus.Registry
	hostname  string
}

var probeLabels = []string{"source", "target", "topology", "site"}

func newPrometheusStore(hostname string) *PrometheusStore {
	reg := prometheus.NewRegistry()

	newGauge := func(name, help string) *prometheus.GaugeVec {
		g := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: name, Help: help}, probeLabels)
		reg.MustRegister(g)
		return g
	}
	newCounter := func(name, help string, labels []string) *prometheus.CounterVec {
		c := prometheus.NewCounterVec(prometheus.CounterOpts{Name: name, Help: help}, labels)
		reg.MustRegister(c)
		return c
	}

	s := &PrometheusStore{
		rttMin:    newGauge("twamp_rtt_min_milliseconds", "Minimum RTT in burst (ms)"),
		rttAvg:    newGauge("twamp_rtt_avg_milliseconds", "Average RTT in burst (ms)"),
		rttMax:    newGauge("twamp_rtt_max_milliseconds", "Maximum RTT in burst (ms)"),
		rttStddev: newGauge("twamp_rtt_stddev_milliseconds", "RTT standard deviation (ms)"),
		jitter:    newGauge("twamp_jitter_milliseconds", "Mean absolute jitter (ms)"),
		lossRatio: newGauge("twamp_loss_ratio", "Packet loss ratio 0.0-1.0"),
		pktSent:   newCounter("twamp_packets_sent_total", "Cumulative packets sent", probeLabels),
		pktRecv:   newCounter("twamp_packets_received_total", "Cumulative packets received", probeLabels),
		reflected: newCounter("twamp_reflected_packets_total", "Packets reflected since startup", []string{"source"}),
		registry:  reg,
		hostname:  hostname,
	}
	// Pre-initialize so the counter appears at zero before any packets arrive.
	s.reflected.WithLabelValues(hostname)
	return s
}

func (s *PrometheusStore) Update(r ProbeResult) {
	ms := func(d time.Duration) float64 { return float64(d.Microseconds()) / 1000.0 }
	labels := prometheus.Labels{
		"source":   r.Source,
		"target":   r.Target,
		"topology": r.Topology,
		"site":     r.Site,
	}
	s.rttMin.With(labels).Set(ms(r.RttMin))
	s.rttAvg.With(labels).Set(ms(r.RttAvg))
	s.rttMax.With(labels).Set(ms(r.RttMax))
	s.rttStddev.With(labels).Set(ms(r.RttStddev))
	s.jitter.With(labels).Set(ms(r.Jitter))
	s.lossRatio.With(labels).Set(r.LossPct / 100.0)
	s.pktSent.With(labels).Add(float64(r.Sent))
	s.pktRecv.With(labels).Add(float64(r.Recv))
}

func (s *PrometheusStore) IncrementReflected() {
	s.reflected.WithLabelValues(s.hostname).Inc()
}

func (s *PrometheusStore) Handler() http.Handler {
	return promhttp.HandlerFor(s.registry, promhttp.HandlerOpts{})
}
```

- [ ] **Step 4: Run tests**

```bash
go test ./... -run "TestPrometheusStore" -v
```

Expected: all 4 PASS.

- [ ] **Step 5: Full build and cross-compile**

```bash
go build ./...
GOOS=windows GOARCH=amd64 go build ./...
GOOS=linux GOARCH=arm64 go build ./...
```

Expected: all succeed.

- [ ] **Step 6: Commit**

```bash
git add prom.go tinytwamp_test.go
git commit -m "feat: add PrometheusStore with metric registration, Update, IncrementReflected, and HTTP handler"
```

---

### Task 3: `runExporter` orchestrator (`exporter.go`) + CLI wiring (`tinytwamp.go`)

**Files:**
- Create: `exporter.go`
- Modify: `tinytwamp.go` (new flags, `"exporter"` case in switch, update default error)

**Interfaces:**
- Consumes:
  - `newPrometheusStore(hostname string) *PrometheusStore` from Task 2
  - `PrometheusStore.Update(ProbeResult)`, `IncrementReflected()`, `Handler() http.Handler` from Task 2
  - `Server.ReflectedCount() uint64` from Task 1
  - `runConfigPoller`, `runProbeScheduler`, `runBurst`, `fetchConfig`, `parseAllowlist`, `newRateLimiter`, `NewServer` — all existing
  - `newInfluxWriter`, `InfluxWriter.run` — existing, for `dual` mode
  - `platformWaitForShutdown` — existing platform shim
- Produces:
  - `func runExporter(port int, configURL, hostname string, configRefresh time.Duration, probeMode, metricsAddr, metricsTLSCert, metricsTLSKey string, synced bool, logFile *os.File)`

- [ ] **Step 1: Write failing test for TLS startup validation**

Add to `tinytwamp_test.go`:

```go
// ============================================================================
// Exporter TLS flag validation
// ============================================================================

func TestExporterTLSFlagValidation(t *testing.T) {
	// Both cert and key must be provided together — validate the check logic.
	// We test the validation function directly, not runExporter (which blocks).
	err := validateTLSFlags("/path/to/cert", "")
	if err == nil {
		t.Error("expected error when cert is set but key is empty")
	}
	err = validateTLSFlags("", "/path/to/key")
	if err == nil {
		t.Error("expected error when key is set but cert is empty")
	}
	err = validateTLSFlags("", "")
	if err != nil {
		t.Errorf("expected no error when both are empty, got %v", err)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

```bash
go test ./... -run TestExporterTLSFlagValidation -v
```

Expected: FAIL — `validateTLSFlags` undefined.

- [ ] **Step 3: Create `exporter.go`**

```go
package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sync"
	"time"
)

// validateTLSFlags returns an error if exactly one of cert/key is non-empty.
func validateTLSFlags(cert, key string) error {
	if (cert == "") != (key == "") {
		return fmt.Errorf("-metrics-tls-cert and -metrics-tls-key must both be set or both be empty")
	}
	return nil
}

// runExporter starts the tinytwamp Prometheus exporter mode.
// probeMode is one of "background", "scrape", or "dual".
func runExporter(
	port int,
	configURL, hostname string,
	configRefresh time.Duration,
	probeMode, metricsAddr, metricsTLSCert, metricsTLSKey string,
	synced bool,
	logFile *os.File,
) {
	out := io.Writer(os.Stdout)
	if logFile != nil {
		out = logFile
	}
	logger := log.New(out, "[TWAMP-Light-Exporter] ", log.LstdFlags|log.Lmicroseconds)

	// Validate TLS flags
	if err := validateTLSFlags(metricsTLSCert, metricsTLSKey); err != nil {
		logger.Fatalf("%v", err)
	}

	// Fetch initial config — fatal on failure
	logger.Printf("Fetching config from %s", configURL)
	initialCfg, err := fetchConfig(configURL)
	if err != nil {
		logger.Fatalf("Cannot fetch initial config: %v", err)
	}
	logger.Printf("Config loaded: topology=%s hosts=%d probe_interval=%v probe_mode=%s",
		initialCfg.Topology, len(initialCfg.Hosts), initialCfg.ProbeInterval, probeMode)

	// Validate dual mode requirements
	if probeMode == "dual" && initialCfg.InfluxDB.URL == "" {
		logger.Fatalf("probe-mode=dual requires influxdb.url in config")
	}

	// Resolve hostname
	if hostname == "" {
		h, err := os.Hostname()
		if err != nil {
			logger.Fatalf("Cannot determine hostname: %v", err)
		}
		hostname = h
	}

	if configRefresh == 0 {
		configRefresh = initialCfg.ConfigRefresh
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store := newPrometheusStore(hostname)

	var wg sync.WaitGroup

	// Goroutine: TWAMP-Light reflector — increments reflected counter
	wg.Add(1)
	go func() {
		defer wg.Done()
		al, _ := parseAllowlist("")
		rl := newRateLimiter(0)
		srv := NewServer(logFile, rl, al, synced)
		srv.onReflect = store.IncrementReflected
		if err := srv.Start(port); err != nil {
			logger.Printf("Server error: %v", err)
		}
	}()

	switch probeMode {
	case "background":
		configCh := make(chan AgentConfig, 1)
		resultsCh := make(chan ProbeResult, 200)
		configCh <- initialCfg

		wg.Add(1)
		go func() {
			defer wg.Done()
			runConfigPoller(ctx, configURL, configRefresh, configCh, logger)
		}()

		wg.Add(1)
		go func() {
			defer wg.Done()
			runProbeScheduler(ctx, configCh, resultsCh, hostname, port, synced, logFile, logger)
		}()

		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case r, ok := <-resultsCh:
					if !ok {
						return
					}
					store.Update(r)
				case <-ctx.Done():
					return
				}
			}
		}()

	case "scrape":
		// Config poller keeps currentCfg fresh; scrapes probe inline.
		var cfgMu sync.RWMutex
		currentCfg := initialCfg

		configCh := make(chan AgentConfig, 1)
		wg.Add(1)
		go func() {
			defer wg.Done()
			runConfigPoller(ctx, configURL, configRefresh, configCh, logger)
		}()

		// Config updater goroutine
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case cfg := <-configCh:
					cfgMu.Lock()
					currentCfg = cfg
					cfgMu.Unlock()
				case <-ctx.Done():
					return
				}
			}
		}()

		// Override the HTTP handler to probe inline on each scrape
		origHandler := store.Handler()
		scrapeHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cfgMu.RLock()
			cfg := currentCfg
			cfgMu.RUnlock()
			targets := cfg.targetsFor(hostname)
			for _, target := range targets {
				result := runBurst(target, cfg, hostname, port, synced, logFile)
				store.Update(result)
			}
			origHandler.ServeHTTP(w, r)
		})
		store.scrapeHandler = scrapeHandler

	case "dual":
		configCh := make(chan AgentConfig, 1)
		promCh := make(chan ProbeResult, 200)
		influxCh := make(chan ProbeResult, 200)
		schedulerCh := make(chan ProbeResult, 200)
		configCh <- initialCfg

		wg.Add(1)
		go func() {
			defer wg.Done()
			runConfigPoller(ctx, configURL, configRefresh, configCh, logger)
		}()

		wg.Add(1)
		go func() {
			defer wg.Done()
			runProbeScheduler(ctx, configCh, schedulerCh, hostname, port, synced, logFile, logger)
		}()

		// Dispatcher: fan-out scheduler results to both Prom and Influx channels
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer close(promCh)
			defer close(influxCh)
			for {
				select {
				case r, ok := <-schedulerCh:
					if !ok {
						return
					}
					select {
					case promCh <- r:
					default:
					}
					select {
					case influxCh <- r:
					default:
					}
				case <-ctx.Done():
					return
				}
			}
		}()

		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case r, ok := <-promCh:
					if !ok {
						return
					}
					store.Update(r)
				case <-ctx.Done():
					return
				}
			}
		}()

		wg.Add(1)
		go func() {
			defer wg.Done()
			w := newInfluxWriter(initialCfg.InfluxDB, logger)
			w.run(ctx, influxCh)
		}()
	}

	// Start metrics HTTP server
	mux := http.NewServeMux()
	if store.scrapeHandler != nil {
		mux.Handle("/metrics", store.scrapeHandler)
	} else {
		mux.Handle("/metrics", store.Handler())
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<html><body><a href="/metrics">Metrics</a></body></html>`)
	})

	srv := &http.Server{Addr: metricsAddr, Handler: mux}

	wg.Add(1)
	go func() {
		defer wg.Done()
		var err error
		if metricsTLSCert != "" {
			logger.Printf("Metrics HTTPS server listening on %s", metricsAddr)
			err = srv.ListenAndServeTLS(metricsTLSCert, metricsTLSKey)
		} else {
			logger.Printf("Metrics HTTP server listening on %s", metricsAddr)
			err = srv.ListenAndServe()
		}
		if err != nil && err != http.ErrServerClosed {
			logger.Printf("Metrics server error: %v", err)
		}
	}()

	platformWaitForShutdown(cancel, logger)
	srv.Close()
	wg.Wait()
}
```

**Important:** `exporter.go` introduces two fields on `PrometheusStore` that don't exist yet: `scrapeHandler http.Handler`. Add this field to `PrometheusStore` in `prom.go`:

```go
type PrometheusStore struct {
	// ... existing fields ...
	scrapeHandler http.Handler // set by runExporter in scrape mode; nil otherwise
}
```

Also, `srv.onReflect = store.IncrementReflected` requires an `onReflect func()` field on `Server` in `tinytwamp.go`. Add it to the struct and call it in `handleTestPacket` after the existing `s.reflectedPackets.Add(1)` line:

```go
type Server struct {
	// ... existing fields ...
	onReflect func() // optional callback invoked after each successful reflection
}

// In handleTestPacket, after s.reflectedPackets.Add(1):
if s.onReflect != nil {
	s.onReflect()
}
```

- [ ] **Step 4: Add exporter flags and dispatch to `tinytwamp.go`**

In the `var (...)` flags block, add:

```go
	probeMode      = flag.String("probe-mode", "background", "Exporter probe mode: background, scrape, or dual")
	metricsAddr    = flag.String("metrics-addr", ":9862", "Address for Prometheus metrics HTTP server")
	metricsTLSCert = flag.String("metrics-tls-cert", "", "TLS certificate file for metrics server (requires -metrics-tls-key)")
	metricsTLSKey  = flag.String("metrics-tls-key", "", "TLS private key file for metrics server (requires -metrics-tls-cert)")
```

In `main()`'s switch, add before `default`:

```go
	case "exporter":
		if *configURL == "" {
			fmt.Fprintf(os.Stderr, "exporter mode requires -config-url\n")
			os.Exit(1)
		}
		runExporter(*port, *configURL, *agentHostname, *configRefresh,
			*probeMode, *metricsAddr, *metricsTLSCert, *metricsTLSKey,
			synced, logFile)
```

Update the default error message:

```go
	default:
		fmt.Fprintf(os.Stderr, "Invalid mode %q. Use 'client', 'server', 'agent', or 'exporter'\n", *mode)
		os.Exit(1)
```

- [ ] **Step 5: Run TLS validation test**

```bash
go test ./... -run TestExporterTLSFlagValidation -v
```

Expected: PASS.

- [ ] **Step 6: Full build and cross-compile**

```bash
go build ./...
go test ./...
GOOS=windows GOARCH=amd64 go build ./...
GOOS=linux GOARCH=arm64 go build ./...
```

Expected: all succeed, all tests pass.

- [ ] **Step 7: Commit**

```bash
git add exporter.go prom.go tinytwamp.go tinytwamp_test.go
git commit -m "feat: add runExporter orchestrator with background/scrape/dual probe modes and optional TLS"
```

---

### Task 4: Deploy file and README section

**Files:**
- Create: `deploy/tinytwamp-exporter.service`
- Modify: `README.md`

- [ ] **Step 1: Create `deploy/tinytwamp-exporter.service`**

```ini
[Unit]
Description=TinyTWAMP-Light Prometheus Exporter
Documentation=https://github.com/buraglio/tiny-twamp
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/tinytwamp \
  -mode exporter \
  -probe-mode background \
  -config-url http://config-server/twamp-config.json \
  -metrics-addr :9862
Restart=on-failure
RestartSec=10s
AmbientCapabilities=CAP_NET_BIND_SERVICE
NoNewPrivileges=true
DynamicUser=yes

[Install]
WantedBy=multi-user.target
```

- [ ] **Step 2: Add exporter section to `README.md`**

Insert before the `## Known Limitations` section:

````markdown
## Exporter Mode (Prometheus)

Exporter mode simultaneously reflects TWAMP-Light packets and exposes RTT/jitter/loss metrics via a Prometheus `/metrics` endpoint (default `:9862`).

### Quick Start

```bash
sudo ./tinytwamp -mode exporter \
  -config-url http://config-server/twamp-config.json \
  -probe-mode background \
  -metrics-addr :9862
```

### Probe Modes

| `-probe-mode` | Description |
|---|---|
| `background` (default) | Background scheduler probes continuously; scrapes return cached results instantly |
| `scrape` | Each Prometheus scrape triggers a fresh burst before responding |
| `dual` | Background probes push to both InfluxDB and Prometheus simultaneously |

### Exporter CLI Flags

| Flag | Default | Description |
|---|---|---|
| `-mode exporter` | — | Enable exporter mode |
| `-probe-mode` | `background` | `background`, `scrape`, or `dual` |
| `-metrics-addr` | `:9862` | Prometheus metrics listen address |
| `-metrics-tls-cert` | `""` | TLS certificate file (enables HTTPS; requires `-metrics-tls-key`) |
| `-metrics-tls-key` | `""` | TLS private key file |
| `-config-url` | — | HTTP URL of topology JSON (required) |

### With TLS

```bash
sudo ./tinytwamp -mode exporter \
  -config-url http://config-server/twamp-config.json \
  -metrics-addr :9862 \
  -metrics-tls-cert /etc/twamp/server.crt \
  -metrics-tls-key  /etc/twamp/server.key
```

Prometheus scrape config:

```yaml
scrape_configs:
  - job_name: twamp
    scheme: https
    tls_config:
      insecure_skip_verify: true  # or provide ca_file
    static_configs:
      - targets: [probe-a:9862, probe-b:9862]
    scrape_interval: 30s
    scrape_timeout: 10s  # increase for scrape mode with many targets
```

### Metrics Exposed

| Metric | Type | Description |
|---|---|---|
| `twamp_rtt_min_milliseconds` | Gauge | Minimum RTT (ms) |
| `twamp_rtt_avg_milliseconds` | Gauge | Average RTT (ms) |
| `twamp_rtt_max_milliseconds` | Gauge | Maximum RTT (ms) |
| `twamp_rtt_stddev_milliseconds` | Gauge | RTT standard deviation (ms) |
| `twamp_jitter_milliseconds` | Gauge | Mean absolute jitter (ms) |
| `twamp_loss_ratio` | Gauge | Packet loss 0.0–1.0 |
| `twamp_packets_sent_total` | Counter | Cumulative packets sent |
| `twamp_packets_received_total` | Counter | Cumulative packets received |
| `twamp_reflected_packets_total` | Counter | Packets reflected by this host |

Labels on probe metrics: `source`, `target`, `topology`, `site`.

### Install as a Service

```bash
sudo cp deploy/tinytwamp-exporter.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now tinytwamp-exporter
```
````

- [ ] **Step 3: Verify build and tests**

```bash
go build ./...
go test ./...
```

Expected: all pass.

- [ ] **Step 4: Commit**

```bash
git add deploy/tinytwamp-exporter.service README.md
git commit -m "feat: add Prometheus exporter systemd unit and README documentation"
```

---

## Self-Review

**Spec coverage:**

| Spec requirement | Task |
|---|---|
| `-mode exporter` dispatch | Task 3 |
| `-probe-mode background/scrape/dual` | Task 3 |
| `-metrics-addr :9862` | Task 3 |
| `-metrics-tls-cert` / `-metrics-tls-key` | Task 3 |
| TLS: fatal if only one set | Task 3 (`validateTLSFlags`) |
| TLS: fatal if file unreadable | Task 3 (`srv.ListenAndServeTLS` returns error — handled in goroutine; note: `ListenAndServeTLS` itself validates the cert at startup and returns immediately on error, which is logged) |
| `PrometheusStore` with all 9 metrics | Task 2 |
| Labels: source, target, topology, site on probe metrics | Task 2 |
| Label: source only on reflected metric | Task 2 |
| `twamp_loss_ratio` as 0.0–1.0 | Task 2 (`LossPct / 100.0`) |
| `_total` suffix on counters | Task 2 |
| `Server.reflectedPackets` atomic counter | Task 1 |
| `Server.ReflectedCount()` accessor | Task 1 |
| `Server.onReflect` callback wired to `IncrementReflected` | Task 3 |
| `background` mode goroutine model | Task 3 |
| `scrape` mode inline burst on each scrape | Task 3 |
| `dual` mode dispatcher fan-out to Prom + Influx | Task 3 |
| Config fetch fatal at startup | Task 3 |
| `dual` with no influxdb → fatal | Task 3 |
| `prometheus/client_golang` dependency | Task 1 |
| systemd unit | Task 4 |
| README exporter section | Task 4 |

**Placeholder scan:** None found.

**Type consistency:**
- `newPrometheusStore(hostname string)` defined Task 2, called Task 3 — consistent.
- `store.Update(ProbeResult)` defined Task 2, called Task 3 — consistent.
- `store.IncrementReflected()` defined Task 2, called via `srv.onReflect` in Task 3 — consistent.
- `store.Handler() http.Handler` defined Task 2, used Task 3 — consistent.
- `store.scrapeHandler http.Handler` added to struct in Task 3, read in `runExporter` — consistent (must be added to `prom.go` in Task 3 step 3).
- `validateTLSFlags(cert, key string) error` defined Task 3 step 3, tested Task 3 step 1 — consistent.
- `Server.onReflect func()` added Task 3, incremented in `handleTestPacket` Task 3 — consistent.
- `runExporter(port int, configURL, hostname string, configRefresh time.Duration, probeMode, metricsAddr, metricsTLSCert, metricsTLSKey string, synced bool, logFile *os.File)` — signature consistent across Task 3 definition and `tinytwamp.go` call site.
