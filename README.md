
# TinyTWAMP — RFC 5357 TWAMP-Light Implementation

A lightweight, RFC 5357 §5 **TWAMP-Light** server and client written in Go.
TWAMP-Light omits the TCP-based TWAMP-Control negotiation phase; both endpoints
speak TWAMP-Test UDP packets directly. This makes it simpler to deploy and
interoperable with other TWAMP-Light implementations, but it is **not**
compatible with full TWAMP implementations (Cisco, Juniper, IXIA) that require
the Control protocol handshake.

## Features

### RFC 5357 Compliance

- **TWAMP-Light profile** (RFC 5357 §5) — correct on-wire binary packet format
- **NTP 64-bit timestamps** per RFC 4656 §4.1.2
- **Four-timestamp RTT calculation** removes reflector processing delay
- **Error Estimate field** with S-bit (NTP-sync flag) per RFC 4656 §3.7.1
- **Sequence number tracking** and out-of-order response handling
- **Optional packet padding** to negotiate test packet sizes

### Security

- **Per-source-IP token bucket rate limiter** (`-rate-limit`) prevents amplification abuse
- **CIDR allowlist** (`-allowed`) restricts reflector to known senders

### Performance

- **Dual explicit sockets** — `udp4` + `udp6` avoid platform-dependent dual-stack behaviour
- **`sync.Pool` buffer reuse** — zero per-packet allocations on the server
- **1 MB socket buffers** for high packet rates
- **Semaphore-limited goroutine pool** (max 100 concurrent)

### Statistics

- Min / avg / max RTT
- **Standard deviation**
- **Mean absolute jitter** (inter-packet delay variation)
- Packet loss percentage

### Operational

- Graceful shutdown (SIGINT / SIGTERM)
- Daemon mode with full flag forwarding
- Configurable port, timeout, and padding
- Log to file or stdout

## How It Works

### Four-Timestamp RTT

```
RTT = (T4 − T1) − (T3 − T2)

T1 = Client send timestamp
T2 = Reflector receive timestamp
T3 = Reflector send timestamp
T4 = Client receive timestamp
```

Subtracting `(T3 − T2)` removes variable reflector processing time, giving
accurate one-way-trip × 2.

### Packet Format

**Test Request (Client → Reflector):**
```
 0                   1                   2                   3
 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|                        Sequence Number                        |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|                         Timestamp (NTP)                       |
|                            64 bits                            |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|         Error Estimate        |         Padding (opt)  ...    |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
Minimum: 14 bytes + N padding bytes
```

**Test Response (Reflector → Client):**
```
 0                   1                   2                   3
 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|                   Sender Sequence Number                      |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|                    Sender Timestamp (NTP)                     |
|                            64 bits                            |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|       Sender Error Estimate   |              MBZ              |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|                  Receive Timestamp (NTP, T2)                  |
|                            64 bits                            |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|                  Reflector Sequence Number                    |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|                    Send Timestamp (NTP, T3)                   |
|                            64 bits                            |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|     Reflector Error Estimate  |  MBZ2 |     Sender TTL        |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
Total: 40 bytes
```

## Requirements

- Go 1.21 or higher
- UDP port 862 access (or use `-port` for a high port without root)
- NTP-synchronized clocks for accurate absolute measurements

## Installation

```bash
git clone https://github.com/buraglio/tiny-twamp.git
cd tiny-twamp
go build -o tinytwamp .
# Optional system-wide install:
sudo cp tinytwamp /usr/local/bin/
```

## Usage

### Command-Line Flags

| Flag | Default | Description |
|------|---------|-------------|
| `-mode` | `client` | `client` or `server` |
| `-server` | `localhost` | Server address (client mode) |
| `-port` | `862` | UDP port (both modes) |
| `-count` | `10` | Packets to send (client mode) |
| `-interval` | `1s` | Interval between packets (client mode) |
| `-timeout` | `5s` | Per-packet receive timeout (client mode) |
| `-padding` | `0` | Zero-padding bytes appended to test packets |
| `-no-sync` | `false` | Assert clock is NOT NTP-synchronized (clears S-bit) |
| `-daemon` | `false` | Run server as background daemon |
| `-logfile` | `""` | Log file path (stdout if empty) |
| `-rate-limit` | `0` | Max packets/sec per source IP, server (0 = unlimited) |
| `-allowed` | `""` | Comma-separated CIDR allowlist, server (empty = all) |

### Server Mode

```bash
# Foreground, port 862 (requires root or CAP_NET_BIND_SERVICE)
sudo ./tinytwamp -mode server

# High port, no root required
./tinytwamp -mode server -port 8620

# With rate limiting and allowlist
sudo ./tinytwamp -mode server \
    -rate-limit 100 \
    -allowed "10.0.0.0/8,2001:db8::/32" \
    -logfile /var/log/twamp.log

# Daemon mode (all flags forwarded)
sudo ./tinytwamp -mode server -daemon -logfile /var/log/twamp.log
```

### Client Mode

```bash
# Basic test to IPv6 address
./tinytwamp -mode client -server 2001:db8::1

# IPv4 test, 100 packets, 100 ms interval
./tinytwamp -mode client -server 192.168.1.1 -count 100 -interval 100ms

# High port (matching server)
./tinytwamp -mode client -server 192.168.1.1 -port 8620

# With padding (match remote reflector's negotiated size)
./tinytwamp -mode client -server 2001:db8::1 -padding 20

# Clock not synced — assert S=0 in error estimate
./tinytwamp -mode client -server 192.168.1.1 -no-sync
```

**Example output:**
```
[TWAMP-Light-Client] 2025/06/01 12:00:00.000000 Starting TWAMP-Light test to 2001:db8::1 (count=10 interval=1s timeout=5s padding=0)
[TWAMP-Light-Client] 2025/06/01 12:00:00.012345 seq=1 RTT=0.823ms
[TWAMP-Light-Client] 2025/06/01 12:00:01.012678 seq=2 RTT=0.791ms
...
[TWAMP-Light-Client] 2025/06/01 12:00:09.015432 === TWAMP-Light Test Statistics ===
[TWAMP-Light-Client] 2025/06/01 12:00:09.015433 Packets sent:     10
[TWAMP-Light-Client] 2025/06/01 12:00:09.015434 Packets received: 10
[TWAMP-Light-Client] 2025/06/01 12:00:09.015435 Packet loss:      0.0%
[TWAMP-Light-Client] 2025/06/01 12:00:09.015436 RTT min/avg/max:  0.778 / 0.812 / 0.857 ms
[TWAMP-Light-Client] 2025/06/01 12:00:09.015437 Std deviation:    0.024 ms
[TWAMP-Light-Client] 2025/06/01 12:00:09.015438 Mean jitter:      0.018 ms
```

### Running from Cron

```bash
# Every 5 minutes, 5 samples
*/5 * * * * /usr/local/bin/tinytwamp -mode client -server 2001:db8::1 \
    -count 5 -logfile /var/log/twamp-monitor.log
```

## Testing

```bash
go test -v ./...
```

Unit tests cover NTP conversion, error estimates, packet marshal/unmarshal,
RTT calculation, rate limiter, allowlist parsing, and statistics math.

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

## Known Limitations

1. **No TWAMP-Control protocol** — TWAMP-Light only; not interoperable with
   full TWAMP implementations requiring the TCP control session.
2. **Unauthenticated mode only** — authenticated and encrypted modes
   (RFC 5357 §6) are not implemented.
3. **TTL not extracted** — reflector reports TTL=64 (default); extracting
   the actual received TTL requires a raw socket.
4. **No one-way delay** — TWAMP-Light measures RTT; one-way delay requires
   OWAMP (RFC 4656) and tightly synchronized clocks.

## Production Considerations

- **Clock sync**: Use NTP/PTP on both endpoints for accurate timestamps.
- **Firewall**: Open UDP port 862 (or your `-port`) bidirectionally.
- **Amplification**: Enable `-rate-limit` and `-allowed` on public-facing servers.
- **Log rotation**: Use `logrotate` or systemd's `StandardOutput=file:`.
- **Privileges**: Use `setcap cap_net_bind_service+ep ./tinytwamp` instead of
  running as root for port 862.

## References

- [RFC 5357](https://www.rfc-editor.org/rfc/rfc5357.html) — TWAMP
- [RFC 4656](https://www.rfc-editor.org/rfc/rfc4656.html) — OWAMP
- [RFC 6038](https://www.rfc-editor.org/rfc/rfc6038.html) — TWAMP Reflect Octets

## License

[LICENSE](LICENSE.md)

## AI-Assisted Development Notice

This software contains components generated by Large Language Model (LLM) or machine intelligence platforms. This generated code may consist of entire code blocks, code reviews, or in some cases, entire applications.
The purpose of this document is to provide transparency regarding the use of artificial intelligence in the creation and maintenance of this project.
Scope of usage
AI tools have been used to assist in various stages of the development lifecycle. 
Human oversight and verification
AI tools were employed to accelerate development and identify potential issues, and a human reviewed AI-generated content. 
Reliability
Code generated by LLMs may occasionally contain subtle logical errors or inefficiencies that were not detected during review.
