package main

import (
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// version is overwritten at build time by goreleaser ldflags.
var version = "dev"

// ============================================================================
// RFC 5357 TWAMP-Light Constants
// ============================================================================

// This implementation conforms to TWAMP-Light (RFC 5357 §5), which omits the
// TWAMP-Control TCP negotiation phase. It is interoperable with other
// TWAMP-Light endpoints but not with full TWAMP implementations that require
// the Control protocol.

const (
	defaultPort = 862

	// Minimum on-wire sizes for unauthenticated mode (RFC 4656 §4.1.2)
	testRequestBaseSize  = 14
	testResponseBaseSize = 40

	// NTP epoch offset: seconds between 1900-01-01 and 1970-01-01
	ntpEpochOffset = 2208988800

	maxConcurrentPackets = 100
)

// ============================================================================
// NTP Timestamp Functions (RFC 5357 §4.1.2)
// ============================================================================

func timeToNTP(t time.Time) uint64 {
	secs := uint64(t.Unix() + ntpEpochOffset)
	frac := (uint64(t.Nanosecond()) << 32) / 1e9
	return (secs << 32) | frac
}

func ntpToTime(ntp uint64) time.Time {
	secs := (ntp >> 32) - ntpEpochOffset
	nanos := ((ntp & 0xFFFFFFFF) * 1e9) >> 32
	return time.Unix(int64(secs), int64(nanos))
}

// getErrorEstimate returns the RFC 4656 §3.7.1 error estimate field.
// Bit layout: S(1) | Z(1) | Scale(6) | Multiplier(8)
// S=1 signals the sender is UTC-synchronized (NTP-disciplined clock).
// We always assert S=1; if your system is not NTP-synced, pass -no-sync.
func getErrorEstimate(synced bool) uint16 {
	if synced {
		return 0x8001 // S=1, Z=0, Scale=0, Multiplier=1
	}
	return 0x0001 // S=0 (unsynchronized)
}

// ============================================================================
// TWAMP Packet Structures (RFC 5357)
// ============================================================================

// TWAMPTestRequest is a TWAMP-Light sender packet (RFC 4656 §4.1.2).
type TWAMPTestRequest struct {
	SequenceNumber uint32
	Timestamp      time.Time
	ErrorEstimate  uint16
	Padding        []byte // Optional zero-padding to reach negotiated packet size
}

func (p *TWAMPTestRequest) MarshalBinary() []byte {
	size := testRequestBaseSize + len(p.Padding)
	buf := make([]byte, size)
	binary.BigEndian.PutUint32(buf[0:4], p.SequenceNumber)
	binary.BigEndian.PutUint64(buf[4:12], timeToNTP(p.Timestamp))
	binary.BigEndian.PutUint16(buf[12:14], p.ErrorEstimate)
	// Padding bytes are already zero from make()
	return buf
}

func (p *TWAMPTestRequest) UnmarshalBinary(data []byte) error {
	if len(data) < testRequestBaseSize {
		return fmt.Errorf("packet too short: %d bytes (minimum %d)", len(data), testRequestBaseSize)
	}
	p.SequenceNumber = binary.BigEndian.Uint32(data[0:4])
	p.Timestamp = ntpToTime(binary.BigEndian.Uint64(data[4:12]))
	p.ErrorEstimate = binary.BigEndian.Uint16(data[12:14])
	if len(data) > testRequestBaseSize {
		p.Padding = data[testRequestBaseSize:]
	}
	return nil
}

// TWAMPTestResponse is a TWAMP-Light reflector packet (RFC 5357 §4.2.1).
type TWAMPTestResponse struct {
	SequenceNumber   uint32
	Timestamp        time.Time
	ErrorEstimate    uint16
	MBZ              uint16
	ReceiveTimestamp time.Time
	SenderSeqNumber  uint32
	SenderTimestamp  time.Time
	SenderError      uint16
	MBZ2             uint8
	SenderTTL        uint8
}

func (p *TWAMPTestResponse) MarshalBinary() []byte {
	buf := make([]byte, testResponseBaseSize)
	off := 0

	binary.BigEndian.PutUint32(buf[off:], p.SequenceNumber)
	off += 4
	binary.BigEndian.PutUint64(buf[off:], timeToNTP(p.Timestamp))
	off += 8
	binary.BigEndian.PutUint16(buf[off:], p.ErrorEstimate)
	off += 2
	binary.BigEndian.PutUint16(buf[off:], 0) // MBZ
	off += 2

	binary.BigEndian.PutUint64(buf[off:], timeToNTP(p.ReceiveTimestamp))
	off += 8
	binary.BigEndian.PutUint32(buf[off:], p.SenderSeqNumber)
	off += 4
	binary.BigEndian.PutUint64(buf[off:], timeToNTP(p.SenderTimestamp))
	off += 8
	binary.BigEndian.PutUint16(buf[off:], p.SenderError)
	off += 2
	buf[off] = 0 // MBZ2
	off++
	buf[off] = p.SenderTTL

	return buf
}

func (p *TWAMPTestResponse) UnmarshalBinary(data []byte) error {
	if len(data) < testResponseBaseSize {
		return fmt.Errorf("response too short: %d bytes (minimum %d)", len(data), testResponseBaseSize)
	}
	off := 0
	p.SequenceNumber = binary.BigEndian.Uint32(data[off:])
	off += 4
	p.Timestamp = ntpToTime(binary.BigEndian.Uint64(data[off:]))
	off += 8
	p.ErrorEstimate = binary.BigEndian.Uint16(data[off:])
	off += 2
	p.MBZ = binary.BigEndian.Uint16(data[off:])
	off += 2

	p.ReceiveTimestamp = ntpToTime(binary.BigEndian.Uint64(data[off:]))
	off += 8
	p.SenderSeqNumber = binary.BigEndian.Uint32(data[off:])
	off += 4
	p.SenderTimestamp = ntpToTime(binary.BigEndian.Uint64(data[off:]))
	off += 8
	p.SenderError = binary.BigEndian.Uint16(data[off:])
	off += 2
	p.MBZ2 = data[off]
	off++
	p.SenderTTL = data[off]

	return nil
}

// ============================================================================
// RTT Calculation (RFC 5357 §4.2.1)
// ============================================================================

// calculateRTT applies the four-timestamp method to remove reflector
// processing delay: RTT = (T4 - T1) - (T3 - T2).
func calculateRTT(t1, t2, t3, t4 time.Time) time.Duration {
	rtt := t4.Sub(t1) - t3.Sub(t2)
	if rtt < 0 {
		return 0
	}
	return rtt
}

// ============================================================================
// Buffer Pool
// ============================================================================

var bufferPool = sync.Pool{
	New: func() any {
		b := make([]byte, 1024)
		return &b
	},
}

func getBuffer() *[]byte {
	return bufferPool.Get().(*[]byte)
}

func putBuffer(b *[]byte) {
	bufferPool.Put(b)
}

// ============================================================================
// Rate Limiter (token bucket per source IP)
// ============================================================================

type rateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*tokenBucket
	rate    int // tokens per second (0 = unlimited)
}

type tokenBucket struct {
	tokens   float64
	lastFill time.Time
}

func newRateLimiter(rate int) *rateLimiter {
	return &rateLimiter{
		buckets: make(map[string]*tokenBucket),
		rate:    rate,
	}
}

// allow returns true if the source IP may send another packet.
func (r *rateLimiter) allow(ip string) bool {
	if r.rate <= 0 {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	b, ok := r.buckets[ip]
	if !ok {
		b = &tokenBucket{tokens: float64(r.rate), lastFill: now}
		r.buckets[ip] = b
	}

	elapsed := now.Sub(b.lastFill).Seconds()
	b.tokens += elapsed * float64(r.rate)
	if b.tokens > float64(r.rate) {
		b.tokens = float64(r.rate)
	}
	b.lastFill = now

	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

// ============================================================================
// CIDR Allowlist
// ============================================================================

type allowlist struct {
	nets []*net.IPNet
}

// parseAllowlist parses a comma-separated list of CIDR prefixes.
// An empty string means allow all.
func parseAllowlist(s string) (*allowlist, error) {
	al := &allowlist{}
	if s == "" {
		return al, nil
	}
	for _, cidr := range strings.Split(s, ",") {
		cidr = strings.TrimSpace(cidr)
		if cidr == "" {
			continue
		}
		_, ipNet, err := net.ParseCIDR(cidr)
		if err != nil {
			return nil, fmt.Errorf("invalid CIDR %q: %v", cidr, err)
		}
		al.nets = append(al.nets, ipNet)
	}
	return al, nil
}

func (al *allowlist) permitted(ip net.IP) bool {
	if len(al.nets) == 0 {
		return true
	}
	for _, n := range al.nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// ============================================================================
// TWAMP-Light Server (Session-Reflector)
// ============================================================================

type Server struct {
	conn             *net.UDPConn
	logger           *log.Logger
	seqMu            sync.Mutex
	seqNumber        uint32
	ctx              context.Context
	cancel           context.CancelFunc
	wg               sync.WaitGroup
	semaphore        chan struct{}
	rl               *rateLimiter
	al               *allowlist
	synced           bool
	reflectedPackets atomic.Uint64
	onReflect        func() // optional callback invoked after each successful reflection
}

func NewServer(logFile *os.File, rl *rateLimiter, al *allowlist, synced bool) *Server {
	out := io.Writer(os.Stdout)
	if logFile != nil {
		out = logFile
	}
	logger := log.New(out, "[TWAMP-Light-Server] ", log.LstdFlags|log.Lmicroseconds)

	ctx, cancel := context.WithCancel(context.Background())
	return &Server{
		logger:    logger,
		ctx:       ctx,
		cancel:    cancel,
		semaphore: make(chan struct{}, maxConcurrentPackets),
		rl:        rl,
		al:        al,
		synced:    synced,
	}
}

func (s *Server) ReflectedCount() uint64 {
	return s.reflectedPackets.Load()
}

func (s *Server) Start(port int) error {
	addr := &net.UDPAddr{Port: port, IP: net.IPv6zero}

	conn4, err4 := net.ListenUDP("udp4", &net.UDPAddr{Port: port, IP: net.IPv4zero})
	conn6, err6 := net.ListenUDP("udp6", addr)

	switch {
	case err4 != nil && err6 != nil:
		return fmt.Errorf("failed to bind: %v / %v", err4, err6)
	case err4 == nil && err6 == nil:
		s.logger.Printf("TWAMP-Light Reflector listening on port %d (IPv4 + IPv6)", port)
		go s.serve(conn4)
		s.serve(conn6)
		return nil
	case err6 == nil:
		s.logger.Printf("TWAMP-Light Reflector listening on port %d (IPv6 only)", port)
		s.serve(conn6)
		return nil
	default:
		s.logger.Printf("TWAMP-Light Reflector listening on port %d (IPv4 only)", port)
		s.serve(conn4)
		return nil
	}
}

func (s *Server) serve(conn *net.UDPConn) {
	conn.SetReadBuffer(1 << 20)
	conn.SetWriteBuffer(1 << 20)

	go platformHandleShutdown(s, conn)

	for {
		select {
		case <-s.ctx.Done():
			s.wg.Wait()
			return
		default:
		}

		bufp := getBuffer()
		conn.SetReadDeadline(time.Now().Add(time.Second))
		n, clientAddr, err := conn.ReadFromUDP(*bufp)
		if err != nil {
			putBuffer(bufp)
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			if s.ctx.Err() != nil {
				return
			}
			s.logger.Printf("Read error: %v", err)
			continue
		}

		clientIP := clientAddr.IP
		if !s.al.permitted(clientIP) {
			s.logger.Printf("Rejected packet from %s (not in allowlist)", clientAddr)
			putBuffer(bufp)
			continue
		}
		if !s.rl.allow(clientIP.String()) {
			s.logger.Printf("Rate-limited packet from %s", clientAddr)
			putBuffer(bufp)
			continue
		}

		data := make([]byte, n)
		copy(data, (*bufp)[:n])
		putBuffer(bufp)

		s.semaphore <- struct{}{}
		s.wg.Add(1)
		go s.handleTestPacket(conn, data, clientAddr)
	}
}

func (s *Server) handleTestPacket(conn *net.UDPConn, data []byte, clientAddr *net.UDPAddr) {
	defer func() {
		<-s.semaphore
		s.wg.Done()
	}()

	receiveTime := time.Now() // T2

	var req TWAMPTestRequest
	if err := req.UnmarshalBinary(data); err != nil {
		s.logger.Printf("Invalid packet from %s: %v", clientAddr, err)
		return
	}

	s.seqMu.Lock()
	s.seqNumber++
	currentSeq := s.seqNumber
	s.seqMu.Unlock()

	response := TWAMPTestResponse{
		SequenceNumber:   req.SequenceNumber,
		Timestamp:        req.Timestamp,
		ErrorEstimate:    req.ErrorEstimate,
		ReceiveTimestamp: receiveTime,
		SenderSeqNumber:  currentSeq,
		SenderTimestamp:  time.Now(), // T3
		SenderError:      getErrorEstimate(s.synced),
		SenderTTL:        64, // actual TTL requires raw socket; 64 is the standard default
	}

	if _, err := conn.WriteToUDP(response.MarshalBinary(), clientAddr); err != nil {
		s.logger.Printf("Failed to send response to %s: %v", clientAddr, err)
		return
	}
	s.reflectedPackets.Add(1)
	if s.onReflect != nil {
		s.onReflect()
	}

	s.logger.Printf("seq=%d src=%s recv=%s send=%s",
		req.SequenceNumber, clientAddr,
		receiveTime.Format(time.RFC3339Nano),
		response.SenderTimestamp.Format(time.RFC3339Nano))
}

// ============================================================================
// TWAMP-Light Client (Session-Sender)
// ============================================================================

// pendingSend tracks the send time for an in-flight packet.
type pendingSend struct {
	t1      time.Time
	sentAt  time.Time // wall time, for expiry
}

// Client represents a TWAMP-Light session sender.
type Client struct {
	serverAddr  string
	port        int
	logger      *log.Logger
	count       int
	interval    time.Duration
	timeout     time.Duration
	paddingSize int
	synced      bool
}

func NewClient(serverAddr string, logFile *os.File, count int, interval, timeout time.Duration, port, paddingSize int, synced bool) *Client {
	out := io.Writer(os.Stdout)
	if logFile != nil {
		out = logFile
	}
	return &Client{
		serverAddr:  serverAddr,
		port:        port,
		logger:      log.New(out, "[TWAMP-Light-Client] ", log.LstdFlags|log.Lmicroseconds),
		count:       count,
		interval:    interval,
		timeout:     timeout,
		paddingSize: paddingSize,
		synced:      synced,
	}
}

func (c *Client) Run() error {
	addr, err := net.ResolveUDPAddr("udp", net.JoinHostPort(c.serverAddr, fmt.Sprintf("%d", c.port)))
	if err != nil {
		return fmt.Errorf("failed to resolve address: %v", err)
	}

	conn, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		return fmt.Errorf("failed to connect: %v", err)
	}
	defer conn.Close()

	conn.SetReadBuffer(1 << 20)
	conn.SetWriteBuffer(1 << 20)

	c.logger.Printf("Starting TWAMP-Light test to %s (count=%d interval=%v timeout=%v padding=%d)",
		c.serverAddr, c.count, c.interval, c.timeout, c.paddingSize)

	// pending maps sequence number → send info; recv goroutine writes results here.
	type result struct {
		rtt time.Duration
		err error
	}

	pending := make(map[uint32]pendingSend)
	var pendingMu sync.Mutex
	results := make(chan result, c.count)

	// Receiver goroutine — handles out-of-order responses.
	recvDone := make(chan struct{})
	go func() {
		defer close(recvDone)
		buf := make([]byte, 1024+c.paddingSize)
		for {
			conn.SetReadDeadline(time.Now().Add(c.timeout))
			n, err := conn.Read(buf)
			if err != nil {
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					// Check if all expected responses have arrived.
					pendingMu.Lock()
					remaining := len(pending)
					pendingMu.Unlock()
					if remaining == 0 {
						return
					}
					// Expire stale entries older than 2× timeout.
					pendingMu.Lock()
					for seq, ps := range pending {
						if time.Since(ps.sentAt) > 2*c.timeout {
							delete(pending, seq)
							results <- result{err: fmt.Errorf("seq %d timed out", seq)}
						}
					}
					remaining = len(pending)
					pendingMu.Unlock()
					if remaining == 0 {
						return
					}
					continue
				}
				return
			}

			t4 := time.Now() // Record immediately after read

			var resp TWAMPTestResponse
			if err := resp.UnmarshalBinary(buf[:n]); err != nil {
				results <- result{err: fmt.Errorf("invalid response: %v", err)}
				continue
			}

			pendingMu.Lock()
			ps, ok := pending[resp.SequenceNumber]
			if ok {
				delete(pending, resp.SequenceNumber)
			}
			pendingMu.Unlock()

			if !ok {
				results <- result{err: fmt.Errorf("unexpected seq %d", resp.SequenceNumber)}
				continue
			}

			rtt := calculateRTT(ps.t1, resp.ReceiveTimestamp, resp.SenderTimestamp, t4)
			results <- result{rtt: rtt}
			c.logger.Printf("seq=%d RTT=%.3fms", resp.SequenceNumber, float64(rtt.Microseconds())/1000.0)
		}
	}()

	// Sender loop.
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

	// Wait for receiver to drain.
	<-recvDone

	// Collect results.
	close(results)
	var rtts []time.Duration
	errCount := 0
	for r := range results {
		if r.err != nil {
			c.logger.Printf("Error: %v", r.err)
			errCount++
		} else {
			rtts = append(rtts, r.rtt)
		}
	}

	c.printStats(rtts, c.count)
	return nil
}

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

func (c *Client) printStats(rtts []time.Duration, sent int) {
	received := len(rtts)
	c.logger.Println("=== TWAMP-Light Test Statistics ===")
	c.logger.Printf("Packets sent:     %d", sent)
	c.logger.Printf("Packets received: %d", received)
	if sent > 0 {
		c.logger.Printf("Packet loss:      %.1f%%", float64(sent-received)/float64(sent)*100)
	}
	if received == 0 {
		c.logger.Println("No successful tests")
		return
	}

	var sum time.Duration
	minRTT := rtts[0]
	maxRTT := rtts[0]
	for _, r := range rtts {
		sum += r
		if r < minRTT {
			minRTT = r
		}
		if r > maxRTT {
			maxRTT = r
		}
	}
	avg := sum / time.Duration(received)

	// Standard deviation
	var variance float64
	avgF := float64(avg)
	for _, r := range rtts {
		d := float64(r) - avgF
		variance += d * d
	}
	stddev := time.Duration(math.Sqrt(variance / float64(received)))

	// Mean absolute jitter (inter-packet delay variation)
	var jitterSum time.Duration
	for i := 1; i < received; i++ {
		d := rtts[i] - rtts[i-1]
		if d < 0 {
			d = -d
		}
		jitterSum += d
	}
	var jitter time.Duration
	if received > 1 {
		jitter = jitterSum / time.Duration(received-1)
	}

	ms := func(d time.Duration) float64 { return float64(d.Microseconds()) / 1000.0 }
	c.logger.Printf("RTT min/avg/max:  %.3f / %.3f / %.3f ms", ms(minRTT), ms(avg), ms(maxRTT))
	c.logger.Printf("Std deviation:    %.3f ms", ms(stddev))
	c.logger.Printf("Mean jitter:      %.3f ms", ms(jitter))
}

// ============================================================================
// Main and CLI
// ============================================================================

var (
	printVersion = flag.Bool("version", false, "Print version and exit")
	mode         = flag.String("mode", "client", "Mode: client or server")
	serverAddr   = flag.String("server", "localhost", "TWAMP-Light server address (client mode)")
	port         = flag.Int("port", defaultPort, "UDP port (both modes)")
	runAsDaemon = flag.Bool("daemon", false, "Run server as a daemon (server mode)")
	logFilePath = flag.String("logfile", "", "Log file path (stdout if empty)")
	count       = flag.Int("count", 10, "Number of test packets (client mode)")
	interval    = flag.Duration("interval", time.Second, "Interval between packets (client mode)")
	timeout     = flag.Duration("timeout", 5*time.Second, "Per-packet receive timeout (client mode)")
	paddingSize = flag.Int("padding", 0, "Extra zero-padding bytes appended to test packets")
	noSync      = flag.Bool("no-sync", false, "Assert clock is NOT NTP-synchronized (sets S=0 in error estimate)")
	rateLimit   = flag.Int("rate-limit", 0, "Max packets per second per source IP on server (0 = unlimited)")
	allowed     = flag.String("allowed", "", "Comma-separated CIDR allowlist for server (empty = allow all)")

	configURL     = flag.String("config-url", "", "HTTP URL of topology JSON config (required in agent mode)")
	configRefresh = flag.Duration("config-refresh", 0, "Config re-fetch interval (default: value from config)")
	agentHostname = flag.String("hostname", "", "Override hostname used for topology lookup (agent mode)")

	probeMode      = flag.String("probe-mode", "background", "Exporter probe mode: background, scrape, or dual")
	metricsAddr    = flag.String("metrics-addr", ":9862", "Address for Prometheus metrics HTTP server")
	metricsTLSCert = flag.String("metrics-tls-cert", "", "TLS certificate file for metrics server (requires -metrics-tls-key)")
	metricsTLSKey  = flag.String("metrics-tls-key", "", "TLS private key file for metrics server (requires -metrics-tls-cert)")
)

func main() {
	flag.Parse()

	if *printVersion {
		fmt.Println(version)
		os.Exit(0)
	}

	synced := !*noSync

	var logFile *os.File
	if *logFilePath != "" {
		var err error
		logFile, err = os.OpenFile(*logFilePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error opening log file: %v\n", err)
			os.Exit(1)
		}
		defer logFile.Close()
	}

	switch *mode {
	case "server":
		if *runAsDaemon {
			runServerAsDaemon()
			return
		}
		al, err := parseAllowlist(*allowed)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Invalid allowlist: %v\n", err)
			os.Exit(1)
		}
		rl := newRateLimiter(*rateLimit)
		server := NewServer(logFile, rl, al, synced)
		if err := server.Start(*port); err != nil {
			fmt.Fprintf(os.Stderr, "Server error: %v\n", err)
			os.Exit(1)
		}

	case "client":
		client := NewClient(*serverAddr, logFile, *count, *interval, *timeout, *port, *paddingSize, synced)
		if err := client.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "Client error: %v\n", err)
			os.Exit(1)
		}

	case "agent":
		if *configURL == "" {
			fmt.Fprintf(os.Stderr, "agent mode requires -config-url\n")
			os.Exit(1)
		}
		runAgent(*port, *configURL, *agentHostname, *configRefresh, synced, logFile)

	case "exporter":
		if *configURL == "" {
			fmt.Fprintf(os.Stderr, "exporter mode requires -config-url\n")
			os.Exit(1)
		}
		runExporter(*port, *configURL, *agentHostname, *configRefresh,
			*probeMode, *metricsAddr, *metricsTLSCert, *metricsTLSKey,
			synced, logFile)

	default:
		fmt.Fprintf(os.Stderr, "Invalid mode %q. Use 'client', 'server', 'agent', or 'exporter'\n", *mode)
		os.Exit(1)
	}
}

// runServerAsDaemon and platformHandleShutdown are implemented in
// platform_unix.go and platform_windows.go.
