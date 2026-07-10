package main

import (
	"math"
	"net"
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
