# Agent Mode Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `-mode agent` to tinytwamp so each probe host simultaneously reflects TWAMP-Light packets and pushes RTT/jitter/loss metrics directly to InfluxDB, with topology driven by a central JSON config server.

**Architecture:** A new `runAgent()` function in `agent.go` spins up four goroutines under a shared context: config poller, TWAMP-Light server (existing code), probe scheduler, and InfluxDB batch writer. The topology JSON config is fetched on startup (fatal if unreachable) and refreshed every `config_refresh` interval (non-fatal). Results flow from scheduler → writer via a buffered channel. `influx.go` owns all InfluxDB Line Protocol formatting and HTTP delivery.

**Tech Stack:** Go stdlib only (`net/http`, `encoding/json`, `context`, `sync`, `time`). No external dependencies added. InfluxDB v2 Line Protocol over HTTP. Grafana for visualization.

## Global Constraints

- Go 1.21+ (module: `github.com/buraglio/tiny-twamp`)
- No external dependencies — stdlib only
- CGO_ENABLED=0 (cross-compilation required; see `.goreleaser.yaml`)
- InfluxDB v2 Line Protocol (not v1 query API)
- Measurement name: `twamp_rtt`
- Tags: `source`, `target`, `topology`, `site`
- InfluxDB batch: max 100 points, flush every 10 seconds
- InfluxDB write retry: exponential backoff, max 3 attempts, then drop + log
- Config fetch failure at startup: fatal (`os.Exit(1)`)
- Config fetch failure on refresh: log warning, continue with last-known config
- Self not found in hosts list: log warning, act as reflector only (no outbound probes)

---

### Task 1: Data types and config parsing (`agent.go`)

**Files:**
- Create: `agent.go`
- Modify: `tinytwamp_test.go` (add config parsing and target resolution tests)

**Interfaces:**
- Produces:
  - `type AgentConfig struct` — parsed topology config
  - `type HostEntry struct`
  - `type HubSpokeConfig struct`
  - `type InfluxConfig struct`
  - `type ProbeResult struct` — per-burst measurement result
  - `func parseAgentConfig(data []byte) (AgentConfig, error)` — JSON → AgentConfig
  - `func (c AgentConfig) targetsFor(hostname string) []HostEntry` — topology resolution

- [ ] **Step 1: Write failing tests for `parseAgentConfig`**

Add to `tinytwamp_test.go`:

```go
// ============================================================================
// Agent config parsing
// ============================================================================

func TestParseAgentConfigMesh(t *testing.T) {
	raw := []byte(`{
		"topology": "mesh",
		"probe_interval": "30s",
		"burst_size": 5,
		"burst_interval": "200ms",
		"packet_timeout": "5s",
		"padding": 0,
		"config_refresh": "5m",
		"influxdb": {
			"url": "http://influx:8086",
			"token": "tok",
			"org": "myorg",
			"bucket": "twamp"
		},
		"hosts": [
			{"name": "a", "address": "10.0.0.1", "site": "east"},
			{"name": "b", "address": "10.0.0.2", "site": "west"}
		],
		"hub_spoke": {"enabled": false, "hub": ""}
	}`)
	cfg, err := parseAgentConfig(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Topology != "mesh" {
		t.Errorf("Topology = %q, want mesh", cfg.Topology)
	}
	if cfg.ProbeInterval != 30*time.Second {
		t.Errorf("ProbeInterval = %v, want 30s", cfg.ProbeInterval)
	}
	if cfg.BurstSize != 5 {
		t.Errorf("BurstSize = %d, want 5", cfg.BurstSize)
	}
	if len(cfg.Hosts) != 2 {
		t.Errorf("len(Hosts) = %d, want 2", len(cfg.Hosts))
	}
	if cfg.InfluxDB.URL != "http://influx:8086" {
		t.Errorf("InfluxDB.URL = %q", cfg.InfluxDB.URL)
	}
}

func TestParseAgentConfigInvalid(t *testing.T) {
	_, err := parseAgentConfig([]byte(`not json`))
	if err == nil {
		t.Error("expected error for invalid JSON, got nil")
	}
}

func TestParseAgentConfigMissingTopology(t *testing.T) {
	raw := []byte(`{"hosts": [{"name":"a","address":"1.2.3.4","site":"x"}]}`)
	_, err := parseAgentConfig(raw)
	if err == nil {
		t.Error("expected error for missing topology, got nil")
	}
}
```

- [ ] **Step 2: Write failing tests for `targetsFor`**

Add to `tinytwamp_test.go`:

```go
func TestTargetsForMesh(t *testing.T) {
	cfg := AgentConfig{
		Topology: "mesh",
		Hosts: []HostEntry{
			{Name: "a", Address: "10.0.0.1", Site: "east"},
			{Name: "b", Address: "10.0.0.2", Site: "west"},
			{Name: "c", Address: "10.0.0.3", Site: "eu"},
		},
		HubSpoke: HubSpokeConfig{Enabled: false},
	}
	targets := cfg.targetsFor("a")
	if len(targets) != 2 {
		t.Fatalf("mesh from 'a': want 2 targets, got %d", len(targets))
	}
	for _, tgt := range targets {
		if tgt.Name == "a" {
			t.Error("mesh should not include self")
		}
	}
}

func TestTargetsForHubAsHub(t *testing.T) {
	cfg := AgentConfig{
		Topology: "hub-spoke",
		Hosts: []HostEntry{
			{Name: "hub",   Address: "10.0.0.1", Site: "core"},
			{Name: "spoke1", Address: "10.0.0.2", Site: "east"},
			{Name: "spoke2", Address: "10.0.0.3", Site: "west"},
		},
		HubSpoke: HubSpokeConfig{Enabled: true, Hub: "hub"},
	}
	targets := cfg.targetsFor("hub")
	if len(targets) != 2 {
		t.Fatalf("hub should probe 2 spokes, got %d", len(targets))
	}
}

func TestTargetsForHubAsSpoke(t *testing.T) {
	cfg := AgentConfig{
		Topology: "hub-spoke",
		Hosts: []HostEntry{
			{Name: "hub",   Address: "10.0.0.1", Site: "core"},
			{Name: "spoke1", Address: "10.0.0.2", Site: "east"},
		},
		HubSpoke: HubSpokeConfig{Enabled: true, Hub: "hub"},
	}
	targets := cfg.targetsFor("spoke1")
	if len(targets) != 1 || targets[0].Name != "hub" {
		t.Fatalf("spoke should probe only hub, got %+v", targets)
	}
}

func TestTargetsForUnknownHost(t *testing.T) {
	cfg := AgentConfig{
		Topology: "mesh",
		Hosts: []HostEntry{
			{Name: "a", Address: "10.0.0.1", Site: "east"},
		},
		HubSpoke: HubSpokeConfig{Enabled: false},
	}
	targets := cfg.targetsFor("not-in-list")
	if len(targets) != 0 {
		t.Errorf("unknown host should get 0 targets, got %d", len(targets))
	}
}
```

- [ ] **Step 3: Run tests to verify they fail**

```bash
go test ./... -run "TestParseAgentConfig|TestTargetsFor" -v
```

Expected: `FAIL` — `AgentConfig`, `parseAgentConfig`, `targetsFor` undefined.

- [ ] **Step 4: Create `agent.go` with types and parsing**

```go
package main

import (
	"encoding/json"
	"fmt"
	"time"
)

// AgentConfig is the parsed topology config fetched from the config server.
type AgentConfig struct {
	Topology      string
	ProbeInterval time.Duration
	BurstSize     int
	BurstInterval time.Duration
	PacketTimeout time.Duration
	Padding       int
	ConfigRefresh time.Duration
	InfluxDB      InfluxConfig
	Hosts         []HostEntry
	HubSpoke      HubSpokeConfig
}

type HostEntry struct {
	Name    string
	Address string
	Site    string
}

type HubSpokeConfig struct {
	Enabled bool
	Hub     string
}

type InfluxConfig struct {
	URL    string
	Token  string
	Org    string
	Bucket string
}

// ProbeResult carries aggregated statistics for one burst to one target.
type ProbeResult struct {
	Source    string
	Target    string
	Site      string
	Topology  string
	RttMin    time.Duration
	RttAvg    time.Duration
	RttMax    time.Duration
	RttStddev time.Duration
	Jitter    time.Duration
	LossPct   float64
	SentAt    time.Time
	Sent      int
	Recv      int
}

// rawAgentConfig mirrors AgentConfig with string durations for JSON decoding.
type rawAgentConfig struct {
	Topology      string         `json:"topology"`
	ProbeInterval string         `json:"probe_interval"`
	BurstSize     int            `json:"burst_size"`
	BurstInterval string         `json:"burst_interval"`
	PacketTimeout string         `json:"packet_timeout"`
	Padding       int            `json:"padding"`
	ConfigRefresh string         `json:"config_refresh"`
	InfluxDB      rawInfluxConfig `json:"influxdb"`
	Hosts         []rawHostEntry `json:"hosts"`
	HubSpoke      rawHubSpoke    `json:"hub_spoke"`
}

type rawInfluxConfig struct {
	URL    string `json:"url"`
	Token  string `json:"token"`
	Org    string `json:"org"`
	Bucket string `json:"bucket"`
}

type rawHostEntry struct {
	Name    string `json:"name"`
	Address string `json:"address"`
	Site    string `json:"site"`
}

type rawHubSpoke struct {
	Enabled bool   `json:"enabled"`
	Hub     string `json:"hub"`
}

func parseAgentConfig(data []byte) (AgentConfig, error) {
	var raw rawAgentConfig
	if err := json.Unmarshal(data, &raw); err != nil {
		return AgentConfig{}, fmt.Errorf("JSON parse error: %w", err)
	}
	if raw.Topology == "" {
		return AgentConfig{}, fmt.Errorf("config missing required field: topology")
	}

	parseDur := func(s, name, def string) (time.Duration, error) {
		if s == "" {
			s = def
		}
		d, err := time.ParseDuration(s)
		if err != nil {
			return 0, fmt.Errorf("invalid duration for %s %q: %w", name, s, err)
		}
		return d, nil
	}

	probeInterval, err := parseDur(raw.ProbeInterval, "probe_interval", "60s")
	if err != nil {
		return AgentConfig{}, err
	}
	burstInterval, err := parseDur(raw.BurstInterval, "burst_interval", "200ms")
	if err != nil {
		return AgentConfig{}, err
	}
	packetTimeout, err := parseDur(raw.PacketTimeout, "packet_timeout", "5s")
	if err != nil {
		return AgentConfig{}, err
	}
	configRefresh, err := parseDur(raw.ConfigRefresh, "config_refresh", "5m")
	if err != nil {
		return AgentConfig{}, err
	}

	burstSize := raw.BurstSize
	if burstSize <= 0 {
		burstSize = 5
	}

	hosts := make([]HostEntry, len(raw.Hosts))
	for i, h := range raw.Hosts {
		hosts[i] = HostEntry{Name: h.Name, Address: h.Address, Site: h.Site}
	}

	return AgentConfig{
		Topology:      raw.Topology,
		ProbeInterval: probeInterval,
		BurstSize:     burstSize,
		BurstInterval: burstInterval,
		PacketTimeout: packetTimeout,
		Padding:       raw.Padding,
		ConfigRefresh: configRefresh,
		InfluxDB: InfluxConfig{
			URL:    raw.InfluxDB.URL,
			Token:  raw.InfluxDB.Token,
			Org:    raw.InfluxDB.Org,
			Bucket: raw.InfluxDB.Bucket,
		},
		Hosts: hosts,
		HubSpoke: HubSpokeConfig{
			Enabled: raw.HubSpoke.Enabled,
			Hub:     raw.HubSpoke.Hub,
		},
	}, nil
}

// targetsFor returns the list of hosts this agent should probe given its hostname.
// Returns empty slice if hostname not found in Hosts (agent acts as reflector only).
func (c AgentConfig) targetsFor(hostname string) []HostEntry {
	found := false
	for _, h := range c.Hosts {
		if h.Name == hostname {
			found = true
			break
		}
	}
	if !found {
		return nil
	}

	if c.HubSpoke.Enabled {
		if hostname == c.HubSpoke.Hub {
			// Hub probes all spokes
			var spokes []HostEntry
			for _, h := range c.Hosts {
				if h.Name != c.HubSpoke.Hub {
					spokes = append(spokes, h)
				}
			}
			return spokes
		}
		// Spoke probes hub only
		for _, h := range c.Hosts {
			if h.Name == c.HubSpoke.Hub {
				return []HostEntry{h}
			}
		}
		return nil
	}

	// Mesh: probe everyone except self
	var targets []HostEntry
	for _, h := range c.Hosts {
		if h.Name != hostname {
			targets = append(targets, h)
		}
	}
	return targets
}
```

- [ ] **Step 5: Run tests to verify they pass**

```bash
go test ./... -run "TestParseAgentConfig|TestTargetsFor" -v
```

Expected: all PASS.

- [ ] **Step 6: Commit**

```bash
git add agent.go tinytwamp_test.go
git commit -m "feat: add AgentConfig types, JSON parsing, and topology resolution"
```

---

### Task 2: InfluxDB Line Protocol writer (`influx.go`)

**Files:**
- Create: `influx.go`
- Modify: `tinytwamp_test.go` (add Line Protocol formatting tests)

**Interfaces:**
- Consumes: `ProbeResult` from Task 1
- Produces:
  - `func lineProtocol(r ProbeResult) string` — formats one ProbeResult as an InfluxDB Line Protocol line
  - `type InfluxWriter struct`
  - `func newInfluxWriter(cfg InfluxConfig, logger *log.Logger) *InfluxWriter`
  - `func (w *InfluxWriter) run(ctx context.Context, results <-chan ProbeResult)` — batches and flushes

- [ ] **Step 1: Write failing tests for `lineProtocol`**

Add to `tinytwamp_test.go`:

```go
// ============================================================================
// InfluxDB Line Protocol
// ============================================================================

func TestLineProtocolFormat(t *testing.T) {
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
		LossPct:   0.0,
		SentAt:    time.Unix(1_000_000, 0).UTC(),
		Sent:      5,
		Recv:      5,
	}
	line := lineProtocol(r)

	// Must start with measurement and contain key tags
	if !strings.HasPrefix(line, "twamp_rtt,") {
		t.Errorf("line should start with twamp_rtt,, got: %s", line)
	}
	if !strings.Contains(line, "source=probe-a") {
		t.Errorf("missing source tag: %s", line)
	}
	if !strings.Contains(line, "target=probe-b") {
		t.Errorf("missing target tag: %s", line)
	}
	if !strings.Contains(line, "topology=mesh") {
		t.Errorf("missing topology tag: %s", line)
	}
	if !strings.Contains(line, "site=us-east") {
		t.Errorf("missing site tag: %s", line)
	}
	if !strings.Contains(line, "rtt_avg_ms=2.000") {
		t.Errorf("missing rtt_avg_ms field: %s", line)
	}
	if !strings.Contains(line, "loss_pct=0.000") {
		t.Errorf("missing loss_pct field: %s", line)
	}
	if !strings.Contains(line, "packets_sent=5i") {
		t.Errorf("missing packets_sent integer field: %s", line)
	}
	// Timestamp must be last token and be Unix nanoseconds of SentAt
	wantTs := fmt.Sprintf("%d", time.Unix(1_000_000, 0).UnixNano())
	if !strings.HasSuffix(strings.TrimSpace(line), wantTs) {
		t.Errorf("line should end with timestamp %s, got: %s", wantTs, line)
	}
}

func TestLineProtocolEscapesSpaces(t *testing.T) {
	r := ProbeResult{
		Source:   "probe a",
		Target:   "probe b",
		Site:     "us east",
		Topology: "mesh",
		SentAt:   time.Unix(1, 0),
	}
	line := lineProtocol(r)
	if strings.Contains(line, "probe a") {
		t.Error("spaces in tag values must be escaped as probe\\ a")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

```bash
go test ./... -run "TestLineProtocol" -v
```

Expected: FAIL — `lineProtocol` undefined.

- [ ] **Step 3: Create `influx.go`**

```go
package main

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"math"
	"net/http"
	"strings"
	"time"
)

// lineProtocol formats a ProbeResult as a single InfluxDB v2 Line Protocol line.
// Format: measurement,tag1=v1,tag2=v2 field1=f1,field2=f2 timestamp
func lineProtocol(r ProbeResult) string {
	escape := func(s string) string {
		s = strings.ReplaceAll(s, " ", `\ `)
		s = strings.ReplaceAll(s, ",", `\,`)
		s = strings.ReplaceAll(s, "=", `\=`)
		return s
	}

	ms := func(d time.Duration) float64 { return float64(d.Microseconds()) / 1000.0 }

	tags := fmt.Sprintf("source=%s,target=%s,topology=%s,site=%s",
		escape(r.Source),
		escape(r.Target),
		escape(r.Topology),
		escape(r.Site),
	)

	fields := fmt.Sprintf(
		"rtt_min_ms=%.3f,rtt_avg_ms=%.3f,rtt_max_ms=%.3f,rtt_stddev_ms=%.3f,jitter_ms=%.3f,loss_pct=%.3f,packets_sent=%di,packets_recv=%di",
		ms(r.RttMin),
		ms(r.RttAvg),
		ms(r.RttMax),
		ms(r.RttStddev),
		ms(r.Jitter),
		r.LossPct,
		r.Sent,
		r.Recv,
	)

	return fmt.Sprintf("twamp_rtt,%s %s %d", tags, fields, r.SentAt.UnixNano())
}

// InfluxWriter batches ProbeResults and flushes to InfluxDB.
type InfluxWriter struct {
	cfg    InfluxConfig
	logger *log.Logger
	client *http.Client
}

func newInfluxWriter(cfg InfluxConfig, logger *log.Logger) *InfluxWriter {
	return &InfluxWriter{
		cfg:    cfg,
		logger: logger,
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

// run reads ProbeResults from the channel, batches them, and flushes to InfluxDB.
// Exits when ctx is cancelled and results channel is drained.
func (w *InfluxWriter) run(ctx context.Context, results <-chan ProbeResult) {
	const maxBatch = 100
	flushInterval := 10 * time.Second
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()

	var batch []string

	flush := func() {
		if len(batch) == 0 {
			return
		}
		body := strings.Join(batch, "\n")
		batch = batch[:0]
		if err := w.write(body); err != nil {
			w.logger.Printf("[InfluxWriter] write error: %v", err)
		}
	}

	for {
		select {
		case r, ok := <-results:
			if !ok {
				flush()
				return
			}
			batch = append(batch, lineProtocol(r))
			if len(batch) >= maxBatch {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-ctx.Done():
			flush()
			return
		}
	}
}

// write posts a Line Protocol body to InfluxDB with exponential backoff retry.
func (w *InfluxWriter) write(body string) error {
	url := fmt.Sprintf("%s/api/v2/write?org=%s&bucket=%s&precision=ns",
		w.cfg.URL, w.cfg.Org, w.cfg.Bucket)

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			wait := time.Duration(math.Pow(2, float64(attempt))) * time.Second
			time.Sleep(wait)
		}
		req, err := http.NewRequest(http.MethodPost, url, bytes.NewBufferString(body))
		if err != nil {
			return fmt.Errorf("build request: %w", err)
		}
		req.Header.Set("Authorization", "Token "+w.cfg.Token)
		req.Header.Set("Content-Type", "text/plain; charset=utf-8")

		resp, err := w.client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("HTTP error: %w", err)
			continue
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusNoContent {
			return nil
		}
		lastErr = fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	return fmt.Errorf("after 3 attempts: %w", lastErr)
}
```

- [ ] **Step 4: Run tests to verify they pass**

```bash
go test ./... -run "TestLineProtocol" -v
```

Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add influx.go tinytwamp_test.go
git commit -m "feat: add InfluxDB Line Protocol writer with batching and retry"
```

---

### Task 3: Config poller and probe scheduler (`agent.go` continued)

**Files:**
- Modify: `agent.go` (add `fetchConfig`, `runConfigPoller`, `runProbeScheduler`, `runBurst`)

**Interfaces:**
- Consumes:
  - `AgentConfig` and `parseAgentConfig` from Task 1
  - `ProbeResult` from Task 1
  - `Client`, `NewClient` from `tinytwamp.go` (existing)
- Produces:
  - `func fetchConfig(url string) (AgentConfig, error)` — HTTP GET → parse
  - `func runConfigPoller(ctx context.Context, url string, refresh time.Duration, out chan<- AgentConfig, logger *log.Logger)`
  - `func runProbeScheduler(ctx context.Context, configs <-chan AgentConfig, results chan<- ProbeResult, hostname string, port int, synced bool, logFile *os.File)`
  - `func runBurst(target HostEntry, cfg AgentConfig, hostname string, port int, synced bool, logFile *os.File) ProbeResult`

- [ ] **Step 1: Add `fetchConfig` and `runConfigPoller` to `agent.go`**

Append to `agent.go`:

```go
import (
	// add at top of file alongside existing imports
	"context"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"time"
)

// fetchConfig fetches and parses the topology JSON from url.
func fetchConfig(url string) (AgentConfig, error) {
	resp, err := http.Get(url) //nolint:gosec // URL is operator-supplied config
	if err != nil {
		return AgentConfig{}, fmt.Errorf("fetch config: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return AgentConfig{}, fmt.Errorf("config server returned %d", resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return AgentConfig{}, fmt.Errorf("read config body: %w", err)
	}
	return parseAgentConfig(data)
}

// runConfigPoller fetches config at startup (sends on out), then re-fetches
// every refresh interval. On refresh failure it logs and continues with the
// last-known config. Exits when ctx is cancelled.
func runConfigPoller(ctx context.Context, url string, refresh time.Duration, out chan<- AgentConfig, logger *log.Logger) {
	ticker := time.NewTicker(refresh)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			cfg, err := fetchConfig(url)
			if err != nil {
				logger.Printf("[Agent] config refresh failed: %v (using last-known config)", err)
				continue
			}
			select {
			case out <- cfg:
			case <-ctx.Done():
				return
			}
		case <-ctx.Done():
			return
		}
	}
}
```

- [ ] **Step 2: Add `runBurst` to `agent.go`**

`runBurst` executes one burst of TWAMP-Light probes to a single target and returns an aggregated `ProbeResult`. It reuses the existing `Client` type from `tinytwamp.go`.

Append to `agent.go`:

```go
// runBurst fires cfg.BurstSize TWAMP-Light packets at target and returns
// aggregated statistics as a ProbeResult.
func runBurst(target HostEntry, cfg AgentConfig, hostname string, port int, synced bool, logFile *os.File) ProbeResult {
	result := ProbeResult{
		Source:   hostname,
		Target:   target.Name,
		Site:     target.Site,
		Topology: cfg.Topology,
		Sent:     cfg.BurstSize,
		SentAt:   time.Now(),
	}

	c := NewClient(
		target.Address,
		logFile,
		cfg.BurstSize,
		cfg.BurstInterval,
		cfg.PacketTimeout,
		port,
		cfg.Padding,
		synced,
	)

	rtts, recv := c.runBurst()
	result.Recv = recv

	if recv == 0 {
		result.LossPct = 100.0
		return result
	}

	// Compute statistics
	var sum time.Duration
	minR, maxR := rtts[0], rtts[0]
	for _, r := range rtts {
		sum += r
		if r < minR {
			minR = r
		}
		if r > maxR {
			maxR = r
		}
	}
	avg := sum / time.Duration(recv)

	var variance float64
	avgF := float64(avg)
	for _, r := range rtts {
		d := float64(r) - avgF
		variance += d * d
	}
	stddev := time.Duration(math.Sqrt(variance / float64(recv)))

	var jitterSum time.Duration
	for i := 1; i < recv; i++ {
		d := rtts[i] - rtts[i-1]
		if d < 0 {
			d = -d
		}
		jitterSum += d
	}
	var jitter time.Duration
	if recv > 1 {
		jitter = jitterSum / time.Duration(recv-1)
	}

	result.RttMin = minR
	result.RttAvg = avg
	result.RttMax = maxR
	result.RttStddev = stddev
	result.Jitter = jitter
	result.LossPct = float64(cfg.BurstSize-recv) / float64(cfg.BurstSize) * 100.0
	return result
}
```

- [ ] **Step 3: Add `runBurst()` method to `Client` in `tinytwamp.go`**

The existing `Client.Run()` method logs and prints stats. We need a lower-level method that returns raw RTT slices for use by the agent. Add after `Client.Run()` in `tinytwamp.go`:

```go
// runBurst sends c.count packets and returns the RTT slice and received count.
// It does not print statistics. Used by agent mode.
func (c *Client) runBurst() (rtts []time.Duration, recv int) {
	addr, err := net.ResolveUDPAddr("udp", net.JoinHostPort(c.serverAddr, fmt.Sprintf("%d", c.port)))
	if err != nil {
		c.logger.Printf("resolve failed: %v", err)
		return nil, 0
	}
	conn, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		c.logger.Printf("dial failed: %v", err)
		return nil, 0
	}
	defer conn.Close()
	conn.SetReadBuffer(1 << 20)
	conn.SetWriteBuffer(1 << 20)

	type result struct {
		rtt time.Duration
		err error
	}
	pending := make(map[uint32]pendingSend)
	var pendingMu sync.Mutex
	results := make(chan result, c.count)

	recvDone := make(chan struct{})
	go func() {
		defer close(recvDone)
		buf := make([]byte, 1024+c.paddingSize)
		for {
			conn.SetReadDeadline(time.Now().Add(c.timeout))
			n, err := conn.Read(buf)
			if err != nil {
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					pendingMu.Lock()
					for seq, ps := range pending {
						if time.Since(ps.sentAt) > 2*c.timeout {
							delete(pending, seq)
							results <- result{err: fmt.Errorf("seq %d timed out", seq)}
						}
					}
					remaining := len(pending)
					pendingMu.Unlock()
					if remaining == 0 {
						return
					}
					continue
				}
				return
			}
			t4 := time.Now()
			var resp TWAMPTestResponse
			if err := resp.UnmarshalBinary(buf[:n]); err != nil {
				continue
			}
			pendingMu.Lock()
			ps, ok := pending[resp.SequenceNumber]
			if ok {
				delete(pending, resp.SequenceNumber)
			}
			pendingMu.Unlock()
			if !ok {
				continue
			}
			rtt := calculateRTT(ps.t1, resp.ReceiveTimestamp, resp.SenderTimestamp, t4)
			results <- result{rtt: rtt}
		}
	}()

	for i := 0; i < c.count; i++ {
		seq := uint32(i + 1)
		t1 := time.Now()
		req := TWAMPTestRequest{
			SequenceNumber: seq,
			Timestamp:      t1,
			ErrorEstimate:  getErrorEstimate(c.synced),
			Padding:        make([]byte, c.paddingSize),
		}
		pendingMu.Lock()
		pending[seq] = pendingSend{t1: t1, sentAt: t1}
		pendingMu.Unlock()
		if _, err := conn.Write(req.MarshalBinary()); err != nil {
			pendingMu.Lock()
			delete(pending, seq)
			pendingMu.Unlock()
			results <- result{err: fmt.Errorf("send failed seq=%d: %v", seq, err)}
		}
		if i < c.count-1 {
			time.Sleep(c.interval)
		}
	}

	<-recvDone
	close(results)
	for r := range results {
		if r.err == nil {
			rtts = append(rtts, r.rtt)
		}
	}
	return rtts, len(rtts)
}
```

- [ ] **Step 4: Add `runProbeScheduler` to `agent.go`**

Append to `agent.go`:

```go
// runProbeScheduler drives periodic probe bursts to all configured targets.
// When a new AgentConfig arrives on configs it drains in-flight work and
// restarts with the new target list. Exits when ctx is cancelled.
func runProbeScheduler(ctx context.Context, configs <-chan AgentConfig, results chan<- ProbeResult, hostname string, port int, synced bool, logFile *os.File, logger *log.Logger) {
	var cfg AgentConfig
	var ticker *time.Ticker

	// Wait for first config before starting ticker
	select {
	case cfg = <-configs:
	case <-ctx.Done():
		return
	}

	ticker = time.NewTicker(cfg.ProbeInterval)
	defer ticker.Stop()

	probe := func() {
		targets := cfg.targetsFor(hostname)
		if len(targets) == 0 {
			logger.Printf("[Agent] %s not in hosts list or no targets — acting as reflector only", hostname)
			return
		}
		for _, target := range targets {
			select {
			case <-ctx.Done():
				return
			default:
			}
			r := runBurst(target, cfg, hostname, port, synced, logFile)
			select {
			case results <- r:
			case <-ctx.Done():
				return
			}
		}
	}

	for {
		select {
		case newCfg := <-configs:
			cfg = newCfg
			ticker.Reset(cfg.ProbeInterval)
			logger.Printf("[Agent] config updated, probing %d hosts every %v", len(cfg.Hosts), cfg.ProbeInterval)
		case <-ticker.C:
			probe()
		case <-ctx.Done():
			return
		}
	}
}
```

- [ ] **Step 5: Verify build**

```bash
go build ./...
```

Expected: no errors.

- [ ] **Step 6: Commit**

```bash
git add agent.go tinytwamp.go
git commit -m "feat: add config poller, probe scheduler, and Client.runBurst"
```

---

### Task 4: `runAgent` orchestrator and CLI wiring (`agent.go`, `tinytwamp.go`)

**Files:**
- Modify: `agent.go` (add `runAgent`)
- Modify: `tinytwamp.go` (add `-mode agent` flags and dispatch)

**Interfaces:**
- Consumes: all functions from Tasks 1–3
- Produces: `func runAgent(port int, configURL, hostname string, configRefresh time.Duration, synced bool, logFile *os.File)`

- [ ] **Step 1: Add `runAgent` to `agent.go`**

Append to `agent.go`:

```go
// runAgent starts the four concurrent goroutines that make up agent mode.
// It blocks until SIGINT/SIGTERM is received.
func runAgent(port int, configURL, hostname string, configRefresh time.Duration, synced bool, logFile *os.File) {
	out := io.Writer(os.Stdout)
	if logFile != nil {
		out = logFile
	}
	logger := log.New(out, "[TWAMP-Light-Agent] ", log.LstdFlags|log.Lmicroseconds)

	// Fetch initial config — fatal on failure
	logger.Printf("Fetching config from %s", configURL)
	initialCfg, err := fetchConfig(configURL)
	if err != nil {
		logger.Fatalf("Cannot fetch initial config: %v", err)
	}
	logger.Printf("Config loaded: topology=%s hosts=%d probe_interval=%v",
		initialCfg.Topology, len(initialCfg.Hosts), initialCfg.ProbeInterval)

	// Resolve hostname
	if hostname == "" {
		h, err := os.Hostname()
		if err != nil {
			logger.Fatalf("Cannot determine hostname: %v", err)
		}
		hostname = h
	}

	// Use config_refresh from fetched config if caller didn't override
	if configRefresh == 0 {
		configRefresh = initialCfg.ConfigRefresh
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Wire channels
	configCh := make(chan AgentConfig, 1)
	resultsCh := make(chan ProbeResult, 200)

	// Seed the config channel with initial config so scheduler starts immediately
	configCh <- initialCfg

	var wg sync.WaitGroup

	// Goroutine 1: Config poller (sends subsequent configs)
	wg.Add(1)
	go func() {
		defer wg.Done()
		runConfigPoller(ctx, configURL, configRefresh, configCh, logger)
	}()

	// Goroutine 2: TWAMP-Light reflector server
	wg.Add(1)
	go func() {
		defer wg.Done()
		al, _ := parseAllowlist("") // agent mode: accept from all peers
		rl := newRateLimiter(0)     // agent mode: no rate limiting (trusted peers)
		srv := NewServer(logFile, rl, al, synced)
		if err := srv.Start(port); err != nil {
			logger.Printf("Server error: %v", err)
		}
	}()

	// Goroutine 3: Probe scheduler
	wg.Add(1)
	go func() {
		defer wg.Done()
		runProbeScheduler(ctx, configCh, resultsCh, hostname, port, synced, logFile, logger)
	}()

	// Goroutine 4: InfluxDB writer
	wg.Add(1)
	go func() {
		defer wg.Done()
		w := newInfluxWriter(initialCfg.InfluxDB, logger)
		w.run(ctx, resultsCh)
	}()

	// Wait for shutdown signal then cancel context
	platformWaitForShutdown(cancel, logger)
	wg.Wait()
}
```

- [ ] **Step 2: Add `platformWaitForShutdown` to platform files**

`runAgent` calls `platformWaitForShutdown(cancel, logger)` instead of relying on the server's internal shutdown. Add to `platform_unix.go`:

```go
func platformWaitForShutdown(cancel context.CancelFunc, logger *log.Logger) {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan
	logger.Println("Received shutdown signal")
	cancel()
}
```

Add to `platform_windows.go`:

```go
func platformWaitForShutdown(cancel context.CancelFunc, logger *log.Logger) {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt)
	<-sigChan
	logger.Println("Received shutdown signal")
	cancel()
}
```

- [ ] **Step 3: Add agent flags to `tinytwamp.go` and wire the dispatch**

In the `var (...)` flags block in `tinytwamp.go`, add:

```go
	configURL     = flag.String("config-url", "", "HTTP URL of topology JSON config (required in agent mode)")
	configRefresh = flag.Duration("config-refresh", 0, "Config re-fetch interval (default: value from config)")
	agentHostname = flag.String("hostname", "", "Override hostname used for topology lookup (agent mode)")
```

In `main()`'s switch statement, add before `default`:

```go
	case "agent":
		if *configURL == "" {
			fmt.Fprintf(os.Stderr, "agent mode requires -config-url\n")
			os.Exit(1)
		}
		runAgent(*port, *configURL, *agentHostname, *configRefresh, synced, logFile)
```

Also update the `default` error message:

```go
	default:
		fmt.Fprintf(os.Stderr, "Invalid mode %q. Use 'client', 'server', or 'agent'\n", *mode)
		os.Exit(1)
```

- [ ] **Step 4: Verify build for all platforms**

```bash
go build ./...
GOOS=windows GOARCH=amd64 go build ./...
GOOS=linux   GOARCH=arm64 go build ./...
```

Expected: no errors on all three.

- [ ] **Step 5: Commit**

```bash
git add agent.go tinytwamp.go platform_unix.go platform_windows.go
git commit -m "feat: add runAgent orchestrator and -mode agent CLI wiring"
```

---

### Task 5: Deploy files

**Files:**
- Create: `deploy/tinytwamp-agent.service`
- Create: `deploy/Dockerfile`
- Create: `deploy/config-example.json`
- Create: `deploy/grafana-dashboard.json`

- [ ] **Step 1: Create `deploy/tinytwamp-agent.service`**

```ini
[Unit]
Description=TinyTWAMP-Light Agent
Documentation=https://github.com/buraglio/tiny-twamp
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/tinytwamp \
  -mode agent \
  -config-url http://config-server/twamp-config.json \
  -config-refresh 5m
Restart=on-failure
RestartSec=10s
AmbientCapabilities=CAP_NET_BIND_SERVICE
NoNewPrivileges=true
DynamicUser=yes

[Install]
WantedBy=multi-user.target
```

- [ ] **Step 2: Create `deploy/Dockerfile`**

```dockerfile
# syntax=docker/dockerfile:1
# NOTE: Use --network host on Linux for accurate RTT measurements.
# Bridge networking introduces NAT that distorts results.
FROM scratch
COPY tinytwamp /tinytwamp
ENTRYPOINT ["/tinytwamp", "-mode", "agent"]
```

- [ ] **Step 3: Create `deploy/config-example.json`**

```json
{
  "topology": "mesh",
  "probe_interval": "60s",
  "burst_size": 5,
  "burst_interval": "200ms",
  "packet_timeout": "5s",
  "padding": 0,
  "config_refresh": "5m",
  "influxdb": {
    "url": "http://influxdb.example.com:8086",
    "token": "replace-with-your-influxdb-token",
    "org": "myorg",
    "bucket": "twamp"
  },
  "hosts": [
    { "name": "probe-a", "address": "10.0.0.1", "site": "us-east" },
    { "name": "probe-b", "address": "10.0.0.2", "site": "us-west" },
    { "name": "probe-c", "address": "10.0.0.3", "site": "eu-west" }
  ],
  "hub_spoke": {
    "enabled": false,
    "hub": "probe-a"
  }
}
```

- [ ] **Step 4: Create `deploy/grafana-dashboard.json`**

```json
{
  "__inputs": [
    {
      "name": "DS_INFLUXDB",
      "label": "InfluxDB",
      "type": "datasource",
      "pluginId": "influxdb"
    }
  ],
  "title": "TinyTWAMP-Light Network Measurements",
  "uid": "tinytwamp-v1",
  "schemaVersion": 38,
  "version": 1,
  "panels": [
    {
      "id": 1,
      "title": "RTT avg over time (ms)",
      "type": "timeseries",
      "gridPos": { "x": 0, "y": 0, "w": 24, "h": 8 },
      "datasource": { "type": "influxdb", "uid": "${DS_INFLUXDB}" },
      "targets": [
        {
          "query": "from(bucket: \"twamp\") |> range(start: v.timeRangeStart, stop: v.timeRangeStop) |> filter(fn: (r) => r._measurement == \"twamp_rtt\" and r._field == \"rtt_avg_ms\") |> group(columns: [\"source\", \"target\"]) |> aggregateWindow(every: v.windowPeriod, fn: mean)",
          "refId": "A"
        }
      ],
      "fieldConfig": {
        "defaults": { "unit": "ms", "custom": { "lineWidth": 1 } }
      }
    },
    {
      "id": 2,
      "title": "Packet Loss % per link",
      "type": "bargauge",
      "gridPos": { "x": 0, "y": 8, "w": 12, "h": 6 },
      "datasource": { "type": "influxdb", "uid": "${DS_INFLUXDB}" },
      "targets": [
        {
          "query": "from(bucket: \"twamp\") |> range(start: v.timeRangeStart, stop: v.timeRangeStop) |> filter(fn: (r) => r._measurement == \"twamp_rtt\" and r._field == \"loss_pct\") |> group(columns: [\"source\", \"target\"]) |> mean()",
          "refId": "A"
        }
      ],
      "fieldConfig": {
        "defaults": { "unit": "percent", "min": 0, "max": 100,
          "thresholds": { "steps": [
            { "color": "green", "value": 0 },
            { "color": "yellow", "value": 1 },
            { "color": "red",    "value": 5 }
          ]}
        }
      }
    },
    {
      "id": 3,
      "title": "Jitter over time (ms)",
      "type": "timeseries",
      "gridPos": { "x": 12, "y": 8, "w": 12, "h": 6 },
      "datasource": { "type": "influxdb", "uid": "${DS_INFLUXDB}" },
      "targets": [
        {
          "query": "from(bucket: \"twamp\") |> range(start: v.timeRangeStart, stop: v.timeRangeStop) |> filter(fn: (r) => r._measurement == \"twamp_rtt\" and r._field == \"jitter_ms\") |> group(columns: [\"source\", \"target\"]) |> aggregateWindow(every: v.windowPeriod, fn: mean)",
          "refId": "A"
        }
      ],
      "fieldConfig": {
        "defaults": { "unit": "ms", "custom": { "lineWidth": 1 } }
      }
    },
    {
      "id": 4,
      "title": "RTT heatmap (avg, source × target)",
      "type": "table",
      "gridPos": { "x": 0, "y": 14, "w": 24, "h": 6 },
      "datasource": { "type": "influxdb", "uid": "${DS_INFLUXDB}" },
      "targets": [
        {
          "query": "from(bucket: \"twamp\") |> range(start: v.timeRangeStart, stop: v.timeRangeStop) |> filter(fn: (r) => r._measurement == \"twamp_rtt\" and r._field == \"rtt_avg_ms\") |> group(columns: [\"source\", \"target\"]) |> mean() |> rename(columns: {_value: \"rtt_avg_ms\"})",
          "refId": "A"
        }
      ],
      "fieldConfig": {
        "defaults": { "unit": "ms",
          "thresholds": { "steps": [
            { "color": "green",  "value": 0 },
            { "color": "yellow", "value": 50 },
            { "color": "red",    "value": 150 }
          ]},
          "custom": { "displayMode": "color-background" }
        }
      }
    }
  ],
  "time": { "from": "now-1h", "to": "now" },
  "refresh": "30s"
}
```

- [ ] **Step 5: Commit**

```bash
git add deploy/
git commit -m "feat: add systemd unit, Dockerfile, example config, and Grafana dashboard"
```

---

### Task 6: README agent mode section

**Files:**
- Modify: `README.md`

- [ ] **Step 1: Add agent mode section to README**

Insert before the "Known Limitations" section in `README.md`:

````markdown
## Agent Mode (Distributed Measurement Service)

Agent mode runs on each probe host simultaneously as a TWAMP-Light reflector
and active prober, pushing results directly to InfluxDB for visualization in
Grafana.

### Quick Start

**1. Deploy a config file** (served by any HTTP server):

```bash
cp deploy/config-example.json /etc/twamp/config.json
# Edit hosts[], influxdb{}, and topology as needed
```

**2. Install the agent** on each probe host:

```bash
sudo cp tinytwamp /usr/local/bin/
sudo cp deploy/tinytwamp-agent.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now tinytwamp-agent
```

**3. Import the Grafana dashboard**: In Grafana → Dashboards → Import,
upload `deploy/grafana-dashboard.json` and select your InfluxDB datasource.

### Topology Modes

**Full mesh** — every host probes every other host:
```json
{ "topology": "mesh", "hub_spoke": { "enabled": false } }
```

**Hub-and-spoke** — spokes probe only the hub; hub probes all spokes:
```json
{ "topology": "hub-spoke", "hub_spoke": { "enabled": true, "hub": "probe-a" } }
```

### Agent CLI Flags

| Flag | Default | Description |
|---|---|---|
| `-mode agent` | — | Enable agent mode |
| `-config-url` | — | HTTP URL of topology JSON (required) |
| `-config-refresh` | from config | Override config re-fetch interval |
| `-hostname` | auto-detected | Override hostname used for topology lookup |
| `-port` | `862` | UDP port for TWAMP-Light (both reflector and prober) |

### Docker (testing only)

```bash
# Build
docker build -f deploy/Dockerfile -t tinytwamp .

# Run — MUST use --network host for accurate RTT on Linux
docker run --network host tinytwamp \
  -mode agent \
  -config-url http://config-server/twamp-config.json
```

> **Warning:** Docker bridge networking introduces NAT that distorts RTT
> measurements. Use `--network host` on Linux, or run bare-metal/VM for
> production.
````

- [ ] **Step 2: Verify build and all tests still pass**

```bash
go build ./...
go test ./...
```

Expected: build succeeds, all tests PASS.

- [ ] **Step 3: Commit**

```bash
git add README.md
git commit -m "docs: add agent mode section to README"
```

---

## Self-Review

**Spec coverage check:**

| Spec section | Covered by task |
|---|---|
| Agent mode: config poller | Task 3 (`runConfigPoller`) |
| Agent mode: TWAMP-Light server goroutine | Task 4 (`runAgent` wires existing `Server`) |
| Agent mode: probe scheduler | Task 3 (`runProbeScheduler`) |
| Agent mode: InfluxDB writer | Task 2 (`InfluxWriter.run`) |
| AgentConfig / ProbeResult types | Task 1 |
| JSON config parsing | Task 1 (`parseAgentConfig`) |
| Topology resolution (mesh + hub-spoke) | Task 1 (`targetsFor`) |
| InfluxDB Line Protocol format | Task 2 (`lineProtocol`) |
| InfluxDB batch (100 pts, 10s flush) | Task 2 (`InfluxWriter.run`) |
| InfluxDB retry (backoff, max 3) | Task 2 (`InfluxWriter.write`) |
| Config fetch failure at startup → fatal | Task 4 (`runAgent`) |
| Config fetch failure on refresh → warn + continue | Task 3 (`runConfigPoller`) |
| Self not in hosts → reflector only | Task 3 (`runProbeScheduler` via `targetsFor`) |
| InfluxDB write failure → log + continue | Task 2 (`InfluxWriter.run`) |
| `-mode agent` CLI flags | Task 4 |
| systemd unit | Task 5 |
| Dockerfile | Task 5 |
| config-example.json | Task 5 |
| Grafana dashboard | Task 5 |
| README agent section | Task 6 |

**Placeholder scan:** None found.

**Type consistency:**
- `ProbeResult` defined in Task 1, consumed in Tasks 2, 3 — field names consistent.
- `AgentConfig` defined in Task 1, consumed in Tasks 3, 4 — field names consistent.
- `InfluxConfig` defined in Task 1, used in Task 2 `newInfluxWriter(cfg InfluxConfig, ...)` — consistent.
- `targetsFor` defined in Task 1, called in Task 3 `runProbeScheduler` — consistent.
- `fetchConfig` defined in Task 3, called in Task 4 `runAgent` — consistent.
- `platformWaitForShutdown` defined in Task 4, called in `runAgent` — consistent across both platform files.
- `Client.runBurst()` defined in Task 3, called in Task 3 `runBurst()` — consistent.
