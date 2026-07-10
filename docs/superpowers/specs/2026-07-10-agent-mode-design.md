# TinyTWAMP Distributed Measurement Service — Design Spec

**Date:** 2026-07-10  
**Status:** Approved  
**Approach:** Option A — Push-to-InfluxDB Agent

---

## 1. Overview

Deploy `tinytwamp` as a distributed TWAMP-Light measurement service across a fleet of 10–50 probe hosts. Each host runs a new `agent` mode that:

1. Pulls its topology configuration from a central HTTP config server.
2. Simultaneously acts as a TWAMP-Light reflector (server) and active prober (client).
3. Pushes aggregated measurement results directly to InfluxDB via the v2 Line Protocol HTTP API.
4. Grafana queries InfluxDB for visualization.

No intermediate collector service is required. InfluxDB is the collector.

---

## 2. Architecture

```
┌─────────────────────────────────────────────────────────┐
│                     Each Probe Host                     │
│                                                         │
│  tinytwamp -mode agent                                  │
│  ├── TWAMP-Light server (listens UDP, reflects packets) │
│  ├── Probe scheduler (fires bursts per config interval) │
│  ├── Config poller (fetches topology JSON periodically) │
│  └── InfluxDB writer (pushes Line Protocol over HTTP)   │
└──────────────┬──────────────────┬───────────────────────┘
               │ pull config      │ push metrics
               ▼                  ▼
      ┌──────────────┐    ┌──────────────┐
      │ Config Server│    │   InfluxDB   │◀─── Grafana
      │ (static JSON │    │  (+ bucket,  │
      │  via HTTP)   │    │   org, token)│
      └──────────────┘    └──────────────┘
```

### Component Responsibilities

| Component | Role |
|---|---|
| Config server | Serves topology JSON over HTTP. Can be a static file behind nginx/caddy. |
| Agent (server goroutine) | Existing TWAMP-Light reflector — reflects packets from all peers unconditionally. |
| Agent (config poller) | HTTP GETs config on startup and every `config_refresh` interval. Falls back to last-known config on failure. |
| Agent (probe scheduler) | Fires a burst of TWAMP-Light packets at each configured target on `probe_interval`. |
| Agent (InfluxDB writer) | Receives `ProbeResult` structs on a channel; batches up to 100 points; flushes every 10 seconds or when batch is full. |
| InfluxDB | Time-series store; receives Line Protocol writes from all probe agents. |
| Grafana | Queries InfluxDB; displays dashboards. |

---

## 3. Concurrency Model

The agent runs four goroutines under a shared `context.Context`:

```
main
├── configPoller()     — polls HTTP config endpoint; sends AgentConfig on channel
├── twampServer()      — existing server code, unmodified
├── probeScheduler()   — driven by ticker; fires bursts; sends ProbeResult on channel
└── influxWriter()     — receives ProbeResult; batches; HTTP POSTs to InfluxDB
```

Config changes are handled gracefully: when the config poller delivers a new `AgentConfig`, the probe scheduler drains any in-flight bursts, then restarts its ticker and target list. The server goroutine is never interrupted.

---

## 4. Topology Config Format

Served as JSON from the config server URL. Each agent uses `os.Hostname()` (overridable with `-hostname`) to find its own entry in `hosts[]` and derive its target list.

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
    "token": "my-influx-token",
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

### Topology Resolution Rules

- `topology = "mesh"`: agent probes every other host in `hosts[]`.
- `topology = "hub-spoke"` with `hub_spoke.enabled = true`:
  - Agent **is** the hub → probes all spoke hosts.
  - Agent **is** a spoke → probes hub only.
- Both modes can coexist: set `topology = "mesh"` and `hub_spoke.enabled = true` to run full mesh among all nodes while also tracking hub→spoke links explicitly via tags.

---

## 5. InfluxDB Data Model

**Measurement:** `twamp_rtt`

| Key | Type | Description |
|---|---|---|
| `source` | tag | Hostname of the sending probe |
| `target` | tag | Hostname or IP being probed |
| `topology` | tag | `mesh` or `hub-spoke` |
| `site` | tag | Optional site label from config |
| `rtt_min_ms` | float field | Minimum RTT in the burst (ms) |
| `rtt_avg_ms` | float field | Average RTT (ms) |
| `rtt_max_ms` | float field | Maximum RTT (ms) |
| `rtt_stddev_ms` | float field | Standard deviation of RTT (ms) |
| `jitter_ms` | float field | Mean absolute inter-packet delay variation (ms) |
| `loss_pct` | float field | Packet loss percentage |
| `packets_sent` | int field | Burst size |
| `packets_recv` | int field | Responses received |

**Write strategy:** batch up to 100 points, flush every 10 seconds or when batch is full. On write failure: exponential backoff, max 3 retries, then drop batch and log error — probes are never paused.

---

## 6. Internal Data Types

```go
// AgentConfig is the parsed topology config fetched from the config server.
type AgentConfig struct {
    Topology      string        // "mesh" or "hub-spoke"
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
```

---

## 7. New CLI Flags (agent mode)

| Flag | Default | Description |
|---|---|---|
| `-mode agent` | — | Enables agent mode |
| `-config-url` | — | HTTP URL of topology JSON (required in agent mode) |
| `-config-refresh` | `5m` | How often to re-fetch config |
| `-hostname` | `""` | Override auto-detected hostname (useful in containers) |

The `-port` and `-logfile` flags always come from the CLI (the server must bind before config is fetched). Probe tuning flags (`-padding`, `-no-sync`) serve as defaults if the corresponding field is absent from the fetched config; the config file takes precedence when present.

---

## 8. Error Handling

| Failure | Behavior |
|---|---|
| Config server unreachable at startup | Fatal — agent cannot determine targets |
| Config server unreachable on refresh | Log warning; continue with last-known config |
| Probe timeout | Counted as lost packet; included in `loss_pct` |
| Probe send failure | Log error; mark all packets in burst as lost |
| InfluxDB write failure | Retry with backoff (max 3×); drop batch; log error; continue probing |
| Self not found in hosts list | Log warning; act as reflector only (no outbound probes) |

---

## 9. Deployment

### systemd Unit

Path: `/etc/systemd/system/tinytwamp-agent.service`

```ini
[Unit]
Description=TinyTWAMP-Light Agent
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
User=twamp
DynamicUser=yes

[Install]
WantedBy=multi-user.target
```

### Docker

```dockerfile
FROM scratch
COPY tinytwamp /tinytwamp
ENTRYPOINT ["/tinytwamp", "-mode", "agent"]
```

> **Important:** Docker's default bridge networking introduces NAT that inflates and distorts RTT measurements. Always use `--network host` on Linux for production deployments. Container mode is best for functional testing only.

### Config Server (minimal)

Any HTTP server can serve the topology file statically:

```nginx
location /twamp-config.json {
    alias /etc/twamp/config.json;
}
```

---

## 10. Grafana Dashboard

A `grafana-dashboard.json` provisioning file is shipped with the repo. Default panels:

| Panel | Type | Query |
|---|---|---|
| RTT avg over time per link | Line graph | `rtt_avg_ms` grouped by `source, target` |
| RTT heatmap (source × target) | Heatmap | `rtt_avg_ms` |
| Packet loss % per link | Bar gauge | `loss_pct` grouped by `source, target` |
| Jitter over time per link | Line graph | `jitter_ms` grouped by `source, target` |

---

## 11. Files to Create / Modify

| Path | Action |
|---|---|
| `tinytwamp.go` | Add `-mode agent` dispatch in `main()` |
| `agent.go` | New file — `AgentConfig`, `ProbeResult`, `runAgent()`, config poller, probe scheduler |
| `influx.go` | New file — InfluxDB Line Protocol writer, batching, retry logic |
| `platform_unix.go` | No change |
| `platform_windows.go` | No change |
| `tinytwamp_test.go` | Add tests for config parsing, target resolution, Line Protocol formatting |
| `deploy/tinytwamp-agent.service` | New file — systemd unit |
| `deploy/Dockerfile` | New file — container image |
| `deploy/grafana-dashboard.json` | New file — Grafana provisioning dashboard |
| `deploy/config-example.json` | New file — example topology config |
| `README.md` | Add agent mode section |

---

## 12. Out of Scope

- TWAMP-Control protocol (TCP negotiation) — TWAMP-Light only
- Authenticated/encrypted TWAMP modes
- Prometheus exporter
- Config server implementation — use any static HTTP server
- TLS for InfluxDB writes (can be added later via `-influx-tls` flag)
- One-way delay measurement (requires OWAMP)
