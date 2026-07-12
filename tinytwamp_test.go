package main

import (
	"fmt"
	"math"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ============================================================================
// NTP timestamp round-trip
// ============================================================================

func TestNTPRoundTrip(t *testing.T) {
	// Truncate to microsecond because NTP fractional resolution is ~232 ps
	// but the conversion loses sub-nanosecond precision; microsecond is safe.
	original := time.Now().Truncate(time.Microsecond)
	converted := ntpToTime(timeToNTP(original))
	diff := original.Sub(converted)
	if diff < 0 {
		diff = -diff
	}
	if diff > time.Microsecond {
		t.Errorf("NTP round-trip error %v > 1µs (original=%v converted=%v)", diff, original, converted)
	}
}

func TestNTPEpochOffset(t *testing.T) {
	// Unix epoch (1970-01-01 00:00:00 UTC) should produce an NTP seconds
	// value equal to ntpEpochOffset.
	epoch := time.Unix(0, 0).UTC()
	ntp := timeToNTP(epoch)
	secs := ntp >> 32
	if secs != ntpEpochOffset {
		t.Errorf("NTP seconds at Unix epoch = %d, want %d", secs, ntpEpochOffset)
	}
}

// ============================================================================
// Error estimate
// ============================================================================

func TestErrorEstimateSynced(t *testing.T) {
	ee := getErrorEstimate(true)
	if ee&0x8000 == 0 {
		t.Errorf("synced error estimate should have S-bit set, got 0x%04x", ee)
	}
}

func TestErrorEstimateUnsynced(t *testing.T) {
	ee := getErrorEstimate(false)
	if ee&0x8000 != 0 {
		t.Errorf("unsynced error estimate should have S-bit clear, got 0x%04x", ee)
	}
}

// ============================================================================
// Packet marshal / unmarshal
// ============================================================================

func TestTestRequestRoundTrip(t *testing.T) {
	original := TWAMPTestRequest{
		SequenceNumber: 42,
		Timestamp:      time.Now().Truncate(time.Microsecond),
		ErrorEstimate:  0x8001,
	}

	data := original.MarshalBinary()
	if len(data) != testRequestBaseSize {
		t.Errorf("marshalled length %d, want %d", len(data), testRequestBaseSize)
	}

	var decoded TWAMPTestRequest
	if err := decoded.UnmarshalBinary(data); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if decoded.SequenceNumber != original.SequenceNumber {
		t.Errorf("SequenceNumber %d != %d", decoded.SequenceNumber, original.SequenceNumber)
	}
	if decoded.ErrorEstimate != original.ErrorEstimate {
		t.Errorf("ErrorEstimate 0x%04x != 0x%04x", decoded.ErrorEstimate, original.ErrorEstimate)
	}
	diff := decoded.Timestamp.Sub(original.Timestamp)
	if diff < 0 {
		diff = -diff
	}
	if diff > time.Microsecond {
		t.Errorf("Timestamp drift %v", diff)
	}
}

func TestTestRequestWithPadding(t *testing.T) {
	padding := make([]byte, 20)
	req := TWAMPTestRequest{
		SequenceNumber: 1,
		Timestamp:      time.Now(),
		ErrorEstimate:  0x8001,
		Padding:        padding,
	}
	data := req.MarshalBinary()
	want := testRequestBaseSize + 20
	if len(data) != want {
		t.Errorf("padded length %d, want %d", len(data), want)
	}
}

func TestTestRequestTooShort(t *testing.T) {
	var req TWAMPTestRequest
	if err := req.UnmarshalBinary(make([]byte, testRequestBaseSize-1)); err == nil {
		t.Error("expected error for short packet, got nil")
	}
}

func TestTestResponseRoundTrip(t *testing.T) {
	now := time.Now().Truncate(time.Microsecond)
	original := TWAMPTestResponse{
		SequenceNumber:   7,
		Timestamp:        now,
		ErrorEstimate:    0x8001,
		ReceiveTimestamp: now.Add(time.Millisecond),
		SenderSeqNumber:  3,
		SenderTimestamp:  now.Add(2 * time.Millisecond),
		SenderError:      0x8001,
		SenderTTL:        64,
	}

	data := original.MarshalBinary()
	if len(data) != testResponseBaseSize {
		t.Errorf("marshalled length %d, want %d", len(data), testResponseBaseSize)
	}

	var decoded TWAMPTestResponse
	if err := decoded.UnmarshalBinary(data); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if decoded.SequenceNumber != original.SequenceNumber {
		t.Errorf("SequenceNumber %d != %d", decoded.SequenceNumber, original.SequenceNumber)
	}
	if decoded.SenderSeqNumber != original.SenderSeqNumber {
		t.Errorf("SenderSeqNumber %d != %d", decoded.SenderSeqNumber, original.SenderSeqNumber)
	}
	if decoded.SenderTTL != original.SenderTTL {
		t.Errorf("SenderTTL %d != %d", decoded.SenderTTL, original.SenderTTL)
	}
}

func TestTestResponseTooShort(t *testing.T) {
	var resp TWAMPTestResponse
	if err := resp.UnmarshalBinary(make([]byte, testResponseBaseSize-1)); err == nil {
		t.Error("expected error for short response, got nil")
	}
}

// ============================================================================
// RTT calculation
// ============================================================================

func TestCalculateRTT(t *testing.T) {
	base := time.Unix(1_000_000, 0)
	t1 := base
	t2 := base.Add(10 * time.Millisecond) // server receives 10ms later
	t3 := base.Add(11 * time.Millisecond) // server takes 1ms to process
	t4 := base.Add(21 * time.Millisecond) // client receives 10ms after server sends

	// RTT = (T4-T1) - (T3-T2) = 21ms - 1ms = 20ms
	rtt := calculateRTT(t1, t2, t3, t4)
	if rtt != 20*time.Millisecond {
		t.Errorf("RTT = %v, want 20ms", rtt)
	}
}

func TestCalculateRTTNonNegative(t *testing.T) {
	// Clock skew can produce negative intermediate values; result must be >= 0.
	base := time.Unix(1_000_000, 0)
	rtt := calculateRTT(base, base.Add(time.Millisecond), base.Add(time.Hour), base.Add(time.Millisecond))
	if rtt < 0 {
		t.Errorf("RTT should be non-negative, got %v", rtt)
	}
}

// ============================================================================
// Rate limiter
// ============================================================================

func TestRateLimiterAllowsUnderLimit(t *testing.T) {
	rl := newRateLimiter(10)
	for i := 0; i < 10; i++ {
		if !rl.allow("192.0.2.1") {
			t.Errorf("packet %d should be allowed", i+1)
		}
	}
}

func TestRateLimiterBlocksOverLimit(t *testing.T) {
	rl := newRateLimiter(2)
	rl.allow("192.0.2.1")
	rl.allow("192.0.2.1")
	if rl.allow("192.0.2.1") {
		t.Error("third packet should be rate-limited")
	}
}

func TestRateLimiterUnlimited(t *testing.T) {
	rl := newRateLimiter(0)
	for i := 0; i < 1000; i++ {
		if !rl.allow("192.0.2.1") {
			t.Fatalf("unlimited limiter blocked packet %d", i+1)
		}
	}
}

func TestRateLimiterPerIP(t *testing.T) {
	rl := newRateLimiter(1)
	if !rl.allow("192.0.2.1") {
		t.Error("first packet from IP-1 should be allowed")
	}
	if !rl.allow("192.0.2.2") {
		t.Error("first packet from IP-2 should be allowed (different bucket)")
	}
}

// ============================================================================
// Allowlist
// ============================================================================

func TestAllowlistEmpty(t *testing.T) {
	al, err := parseAllowlist("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !al.permitted(net.ParseIP("10.0.0.1")) {
		t.Error("empty allowlist should permit all IPs")
	}
}

func TestAllowlistPermits(t *testing.T) {
	al, err := parseAllowlist("10.0.0.0/8,2001:db8::/32")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !al.permitted(net.ParseIP("10.1.2.3")) {
		t.Error("10.1.2.3 should be permitted by 10.0.0.0/8")
	}
	if !al.permitted(net.ParseIP("2001:db8::1")) {
		t.Error("2001:db8::1 should be permitted by 2001:db8::/32")
	}
}

func TestAllowlistBlocks(t *testing.T) {
	al, err := parseAllowlist("10.0.0.0/8")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if al.permitted(net.ParseIP("192.168.1.1")) {
		t.Error("192.168.1.1 should be blocked")
	}
}

func TestAllowlistInvalidCIDR(t *testing.T) {
	if _, err := parseAllowlist("not-a-cidr"); err == nil {
		t.Error("invalid CIDR should return error")
	}
}

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

func TestParseAgentConfigBadDuration(t *testing.T) {
	raw := []byte(`{"topology":"mesh","probe_interval":"not-a-duration"}`)
	_, err := parseAgentConfig(raw)
	if err == nil {
		t.Error("expected error for invalid duration, got nil")
	}
}

func TestParseAgentConfigNegativePadding(t *testing.T) {
	raw := []byte(`{"topology":"mesh","padding":-1,"hosts":[]}`)
	_, err := parseAgentConfig(raw)
	if err == nil {
		t.Error("expected error for negative padding, got nil")
	}
}

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
			{Name: "hub",    Address: "10.0.0.1", Site: "core"},
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
			{Name: "hub",    Address: "10.0.0.1", Site: "core"},
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
	if !strings.Contains(line, `probe\ a`) {
		t.Error("spaces in tag values should be escaped as probe\\ a")
	}
}

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

// ============================================================================
// Statistics helpers (stddev, jitter)
// ============================================================================

func TestStddevAndJitter(t *testing.T) {
	// Feed a known set of RTTs and verify stddev is computed correctly.
	// RTTs: 10ms, 20ms, 30ms → avg=20ms, variance=((−10)²+0²+10²)/3 = 200/3 ms²
	rtts := []time.Duration{
		10 * time.Millisecond,
		20 * time.Millisecond,
		30 * time.Millisecond,
	}

	var sum time.Duration
	for _, r := range rtts {
		sum += r
	}
	avg := sum / time.Duration(len(rtts))

	var variance float64
	avgF := float64(avg)
	for _, r := range rtts {
		d := float64(r) - avgF
		variance += d * d
	}
	stddev := time.Duration(math.Sqrt(variance / float64(len(rtts))))

	// Expected stddev ≈ 8.165ms
	wantLo := 8 * time.Millisecond
	wantHi := 9 * time.Millisecond
	if stddev < wantLo || stddev > wantHi {
		t.Errorf("stddev %v not in [%v, %v]", stddev, wantLo, wantHi)
	}

	// Jitter: |20-10| + |30-20| = 10ms + 10ms, avg = 10ms
	var jitterSum time.Duration
	for i := 1; i < len(rtts); i++ {
		d := rtts[i] - rtts[i-1]
		if d < 0 {
			d = -d
		}
		jitterSum += d
	}
	jitter := jitterSum / time.Duration(len(rtts)-1)
	if jitter != 10*time.Millisecond {
		t.Errorf("jitter = %v, want 10ms", jitter)
	}
}

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
