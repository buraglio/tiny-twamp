# TinyTWAMP Prometheus Exporter — Design Spec

**Date:** 2026-07-11
**Status:** Approved
**Approach:** Option B (background cached) with all three probe modes selectable via `-probe-mode` flag

---

## 1. Overview

Add `-mode exporter` to the existing `tinytwamp` binary. The exporter simultaneously acts as a TWAMP-Light reflector and active prober, exposing results as Prometheus metrics on an HTTP `/metrics` endpoint (default port 9862). Three probe modes are available at runtime via `-probe-mode`:

| `-probe-mode` | Behaviour |
|---|---|
| `background` (default) | Background scheduler probes continuously; scrapes return cached results instantly |
| `scrape` | Each Prometheus scrape triggers a fresh burst synchronously before responding |
| `dual` | Background probes feed both InfluxDB (existing agent writer) and Prometheus simultaneously |

The config format is identical to agent mode — same JSON, same topology rules. The `influxdb` block is required only in `dual` mode.

---

## 2. Architecture

```
┌──────────────────────────────────────────────────────────────┐
│              tinytwamp -mode exporter                        │
│                                                              │
│  Config poller    — fetches topology JSON (all modes)        │
│  TWAMP-Light server — reflects peer packets (all modes)      │
│                                                              │
│  ┌─ probe-mode=background ──────────────────────────────┐   │
│  │  Probe scheduler → PrometheusStore.Update() → /metrics│  │
│  └──────────────────────────────────────────────────────┘   │
│  ┌─ probe-mode=scrape ──────────────────────────────────┐   │
│  │  /metrics handler → runBurst() per target → respond  │   │
│  └──────────────────────────────────────────────────────┘   │
│  ┌─ probe-mode=dual ────────────────────────────────────┐   │
│  │  Probe scheduler → InfluxWriter.run() (existing)     │   │
│  │                  → PrometheusStore.Update() → /metrics│  │
│  └──────────────────────────────────────────────────────┘   │
└──────────────────────────────────────────────────────────────┘
```

### Component responsibilities

| Component | Role |
|---|---|
| Config poller | Fetches topology JSON; runs in all probe modes |
| TWAMP-Light server | Reflects packets from peers; increments reflected counter |
| Probe scheduler | Fires bursts per `probe_interval`; used in `background` and `dual` modes |
| PrometheusStore | Holds registered Gauge/Counter metrics; `Update(ProbeResult)` and `IncrementReflected()` |
| HTTP server | Serves `/metrics` on `-metrics-addr`; in `scrape` mode runs bursts inline |
| InfluxWriter | Existing — reused unchanged in `dual` mode only |

---

## 3. Prometheus Metrics

All probe metrics carry labels: `source`, `target`, `topology`, `site` — matching the InfluxDB tag schema.

### Probe metrics (per ProbeResult)

| Metric name | Type | Description |
|---|---|---|
| `twamp_rtt_min_milliseconds` | Gauge | Minimum RTT in burst (ms) |
| `twamp_rtt_avg_milliseconds` | Gauge | Average RTT (ms) |
| `twamp_rtt_max_milliseconds` | Gauge | Maximum RTT (ms) |
| `twamp_rtt_stddev_milliseconds` | Gauge | RTT standard deviation (ms) |
| `twamp_jitter_milliseconds` | Gauge | Mean absolute jitter (ms) |
| `twamp_loss_ratio` | Gauge | Packet loss 0.0–1.0 (not percent — Prometheus convention) |
| `twamp_packets_sent_total` | Counter | Cumulative packets sent to this target |
| `twamp_packets_received_total` | Counter | Cumulative packets received from this target |

### Reflector metric

| Metric name | Type | Labels | Description |
|---|---|---|---|
| `twamp_reflected_packets_total` | Counter | `source` (this host) | Packets reflected since startup |

### Automatic metrics

`prometheus/client_golang` provides standard Go runtime metrics (goroutines, GC, memory) at no extra cost.

### `scrape` mode behaviour

When a target is unreachable during a scrape-triggered probe: `twamp_loss_ratio` = 1.0, all RTT gauges = 0. The scrape always succeeds — it never returns an HTTP error for a probe failure.

---

## 4. New CLI Flags

| Flag | Default | Description |
|---|---|---|
| `-mode exporter` | — | Enable exporter mode |
| `-probe-mode` | `background` | `background`, `scrape`, or `dual` |
| `-metrics-addr` | `:9862` | Address for the Prometheus HTTP server |
| `-metrics-tls-cert` | `""` | Path to TLS certificate file (enables HTTPS when set with `-metrics-tls-key`) |
| `-metrics-tls-key` | `""` | Path to TLS private key file |
| `-config-url` | — | HTTP URL of topology JSON (required) |
| `-config-refresh` | from config | Override config re-fetch interval |
| `-hostname` | auto-detected | Override hostname for topology lookup |
| `-port` | `862` | UDP port for TWAMP-Light |

`-probe-mode dual` additionally requires a valid `influxdb` block in the topology config.

---

## 5. Files to Create / Modify

| Path | Action |
|---|---|
| `prom.go` | New — `PrometheusStore` struct, metric registrations, `Update(ProbeResult)`, `IncrementReflected()`, metrics HTTP handler |
| `exporter.go` | New — `runExporter(...)` orchestrator wiring all goroutines per probe mode |
| `tinytwamp.go` | Modify — add `-mode exporter` dispatch, `-probe-mode`, `-metrics-addr` flags; add `atomic.Uint64` reflected counter to `Server` struct; add `ReflectedCount() uint64` accessor; increment counter in `handleTestPacket` |
| `agent.go` | No change — `runConfigPoller`, `runProbeScheduler`, `runBurst` reused as-is |
| `influx.go` | No change — `InfluxWriter` reused as-is in `dual` mode |
| `go.mod` | Add `github.com/prometheus/client_golang` |
| `tinytwamp_test.go` | Add tests: PrometheusStore.Update correctness, metric label values, scrape mode probe-and-respond, reflected counter increment |
| `deploy/tinytwamp-exporter.service` | New — systemd unit for exporter mode |
| `README.md` | Add exporter mode section |

---

## 6. Internal Data Types

### PrometheusStore

```go
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

func newPrometheusStore(hostname string) *PrometheusStore
func (s *PrometheusStore) Update(r ProbeResult)
func (s *PrometheusStore) IncrementReflected()
func (s *PrometheusStore) Handler() http.Handler
```

### runExporter signature

```go
func runExporter(
    port int,
    configURL string,
    hostname string,
    configRefresh time.Duration,
    probeMode string,        // "background" | "scrape" | "dual"
    metricsAddr string,
    metricsTLSCert string,   // path to cert file; empty = plain HTTP
    metricsTLSKey  string,   // path to key file; empty = plain HTTP
    synced bool,
    logFile *os.File,
)
```

---

## 7. Probe Mode Details

### `background`

Goroutine model (mirrors agent mode exactly):
1. Config poller → `configCh`
2. TWAMP-Light server
3. Probe scheduler → `resultsCh`
4. PrometheusStore updater (reads `resultsCh`, calls `Update`)
5. HTTP server serves `/metrics`

### `scrape`

Goroutine model (minimal):
1. Config poller → keeps `currentConfig *AgentConfig` updated (mutex-protected)
2. TWAMP-Light server
3. HTTP server: on each `/metrics` request, reads `currentConfig`, calls `runBurst` for each target sequentially, calls `store.Update` for each, then serves `/metrics`

Scrape timeout risk: operators must set Prometheus `scrape_timeout` to exceed `N_targets × (burst_size × burst_interval + packet_timeout)`.

### `dual`

Goroutine model (superset of background):
1. Config poller → `configCh`
2. TWAMP-Light server
3. Probe scheduler → `resultsCh` (buffered, fan-out)
4. PrometheusStore updater (reads from resultsCh copy)
5. InfluxWriter (reads from resultsCh copy — channel fan-out via a dispatcher goroutine)
6. HTTP server serves `/metrics`

The dispatcher goroutine reads one `ProbeResult` and sends it to both the InfluxWriter channel and the PrometheusStore updater channel.

---

## 8. Error Handling

| Failure | Behaviour |
|---|---|
| Config server unreachable at startup | Fatal |
| Config server unreachable on refresh | Log warning; continue with last-known config |
| Probe timeout in `background`/`dual` | loss_ratio = 1.0, RTT gauges = 0 |
| Probe timeout in `scrape` | Same — scrape returns 200 with degraded metrics |
| InfluxDB write failure in `dual` | Log, retry, drop — same as agent mode |
| `-probe-mode dual` with no influxdb config | Fatal at startup with clear error message |
| `/metrics` HTTP error | Logged; Prometheus marks scrape as failed |
| Only one of `-metrics-tls-cert`/`-metrics-tls-key` set | Fatal at startup — both must be provided together |
| TLS cert or key file unreadable | Fatal at startup with clear error message |

---

## 9. Deployment

### systemd unit (`deploy/tinytwamp-exporter.service`)

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

### Prometheus scrape config example

```yaml
scrape_configs:
  - job_name: twamp
    static_configs:
      - targets:
          - probe-a:9862
          - probe-b:9862
          - probe-c:9862
    scrape_interval: 30s
    scrape_timeout: 10s   # increase for scrape mode with many targets
```

---

## 10. Out of Scope

- Prometheus push gateway support
- Per-target scrape timeout configuration
- Authenticated scraping (bearer token, mTLS)
- Grafana dashboard for Prometheus datasource (separate follow-on)
