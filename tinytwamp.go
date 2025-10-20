package main

import (
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// ============================================================================
// RFC 5357 TWAMP Constants
// ============================================================================

const (
	// TWAMP uses UDP port 862 for both control and test packets
	TWAMPPort = 862

	// Packet sizes for unauthenticated mode
	TestRequestMinSize  = 14 // Minimum TWAMP-Test request packet
	TestResponseMinSize = 40 // Minimum TWAMP-Test response packet

	// NTP epoch offset (seconds between 1900-01-01 and 1970-01-01)
	ntpEpochOffset = 2208988800

	// Buffer pool sizes
	maxConcurrentPackets = 100
)

// ============================================================================
// NTP Timestamp Functions (RFC 5357 Section 4.1.2)
// ============================================================================

// timeToNTP converts a Go time.Time to a 64-bit NTP timestamp
// Format: 32 bits for seconds since 1900-01-01, 32 bits for fractional seconds
func timeToNTP(t time.Time) uint64 {
	secs := uint64(t.Unix() + ntpEpochOffset)
	nanos := uint64(t.Nanosecond())
	// Convert nanoseconds to NTP fractional seconds (2^32 units per second)
	frac := (nanos << 32) / 1e9
	return (secs << 32) | frac
}

// ntpToTime converts a 64-bit NTP timestamp to Go time.Time
func ntpToTime(ntp uint64) time.Time {
	secs := (ntp >> 32) - ntpEpochOffset
	frac := ntp & 0xFFFFFFFF
	// Convert NTP fractional seconds to nanoseconds
	nanos := (frac * 1e9) >> 32
	return time.Unix(int64(secs), int64(nanos))
}

// getErrorEstimate returns a basic error estimate for timestamp accuracy
// Format per RFC 4656 Section 3.7.1: S | Z | Scale | Multiplier
// For simplicity: S=0 (synchronized), Z=0 (accurate), Scale=0, Multiplier=1
func getErrorEstimate() uint16 {
	return 0x0001 // Minimal error estimate
}

// ============================================================================
// TWAMP Packet Structures (RFC 5357)
// ============================================================================

// TWAMPTestRequest represents a TWAMP-Test request packet (sender to reflector)
// RFC 5357 Section 4.1.2 - Unauthenticated mode
type TWAMPTestRequest struct {
	SequenceNumber uint32
	Timestamp      time.Time
	ErrorEstimate  uint16
}

// MarshalBinary encodes the test request packet
func (p *TWAMPTestRequest) MarshalBinary() []byte {
	buf := make([]byte, TestRequestMinSize)
	binary.BigEndian.PutUint32(buf[0:4], p.SequenceNumber)
	binary.BigEndian.PutUint64(buf[4:12], timeToNTP(p.Timestamp))
	binary.BigEndian.PutUint16(buf[12:14], p.ErrorEstimate)
	return buf
}

// UnmarshalBinary decodes the test request packet
func (p *TWAMPTestRequest) UnmarshalBinary(data []byte) error {
	if len(data) < TestRequestMinSize {
		return fmt.Errorf("packet too short: %d bytes (minimum %d)", len(data), TestRequestMinSize)
	}
	p.SequenceNumber = binary.BigEndian.Uint32(data[0:4])
	p.Timestamp = ntpToTime(binary.BigEndian.Uint64(data[4:12]))
	p.ErrorEstimate = binary.BigEndian.Uint16(data[12:14])
	return nil
}

// TWAMPTestResponse represents a TWAMP-Test response packet (reflector to sender)
// RFC 5357 Section 4.2.1 - Unauthenticated mode
type TWAMPTestResponse struct {
	// Sender's original data (copied from request)
	SequenceNumber uint32
	Timestamp      time.Time
	ErrorEstimate  uint16
	MBZ            uint16 // Must Be Zero

	// Reflector's timestamps
	ReceiveTimestamp time.Time // T2: When reflector received packet
	SenderSeqNumber  uint32    // Reflector's own sequence number
	SenderTimestamp  time.Time // T3: When reflector sent response
	SenderError      uint16    // Reflector's error estimate
	MBZ2             uint8     // Must Be Zero
	SenderTTL        uint8     // TTL from received packet
}

// MarshalBinary encodes the test response packet
func (p *TWAMPTestResponse) MarshalBinary() []byte {
	buf := make([]byte, TestResponseMinSize)
	offset := 0

	// Copy sender's original data
	binary.BigEndian.PutUint32(buf[offset:offset+4], p.SequenceNumber)
	offset += 4
	binary.BigEndian.PutUint64(buf[offset:offset+8], timeToNTP(p.Timestamp))
	offset += 8
	binary.BigEndian.PutUint16(buf[offset:offset+2], p.ErrorEstimate)
	offset += 2
	binary.BigEndian.PutUint16(buf[offset:offset+2], 0) // MBZ
	offset += 2

	// Add reflector timestamps
	binary.BigEndian.PutUint64(buf[offset:offset+8], timeToNTP(p.ReceiveTimestamp))
	offset += 8
	binary.BigEndian.PutUint32(buf[offset:offset+4], p.SenderSeqNumber)
	offset += 4
	binary.BigEndian.PutUint64(buf[offset:offset+8], timeToNTP(p.SenderTimestamp))
	offset += 8
	binary.BigEndian.PutUint16(buf[offset:offset+2], p.SenderError)
	offset += 2
	buf[offset] = 0 // MBZ2
	offset++
	buf[offset] = p.SenderTTL

	return buf
}

// UnmarshalBinary decodes the test response packet
func (p *TWAMPTestResponse) UnmarshalBinary(data []byte) error {
	if len(data) < TestResponseMinSize {
		return fmt.Errorf("response too short: %d bytes (minimum %d)", len(data), TestResponseMinSize)
	}

	offset := 0
	p.SequenceNumber = binary.BigEndian.Uint32(data[offset : offset+4])
	offset += 4
	p.Timestamp = ntpToTime(binary.BigEndian.Uint64(data[offset : offset+8]))
	offset += 8
	p.ErrorEstimate = binary.BigEndian.Uint16(data[offset : offset+2])
	offset += 2
	p.MBZ = binary.BigEndian.Uint16(data[offset : offset+2])
	offset += 2

	p.ReceiveTimestamp = ntpToTime(binary.BigEndian.Uint64(data[offset : offset+8]))
	offset += 8
	p.SenderSeqNumber = binary.BigEndian.Uint32(data[offset : offset+4])
	offset += 4
	p.SenderTimestamp = ntpToTime(binary.BigEndian.Uint64(data[offset : offset+8]))
	offset += 8
	p.SenderError = binary.BigEndian.Uint16(data[offset : offset+2])
	offset += 2
	p.MBZ2 = data[offset]
	offset++
	p.SenderTTL = data[offset]

	return nil
}

// ============================================================================
// RTT Calculation (RFC 5357 Section 4.2.1)
// ============================================================================

// CalculateRTT computes round-trip time using the four-timestamp method
// This removes server processing delay from the calculation
// RTT = (T4 - T1) - (T3 - T2)
// Where:
//   T1 = Client send time
//   T2 = Server receive time
//   T3 = Server send time
//   T4 = Client receive time
func calculateRTT(t1, t2, t3, t4 time.Time) time.Duration {
	totalTime := t4.Sub(t1)      // Client's perspective of total time
	serverDelay := t3.Sub(t2)     // Server processing delay
	rtt := totalTime - serverDelay

	// Ensure non-negative RTT (clock sync issues can cause negative values)
	if rtt < 0 {
		return 0
	}
	return rtt
}

// ============================================================================
// Buffer Pool for Performance Optimization
// ============================================================================

var bufferPool = sync.Pool{
	New: func() any {
		return make([]byte, 1024) // Support larger packets with padding
	},
}

// ============================================================================
// TWAMP Server (Session-Reflector)
// ============================================================================

// Server represents a TWAMP reflector server
type Server struct {
	conn          *net.UDPConn
	logFile       *os.File
	logger        *log.Logger
	seqNumber     uint32
	seqMutex      sync.Mutex
	ctx           context.Context
	cancel        context.CancelFunc
	wg            sync.WaitGroup
	semaphore     chan struct{} // Limit concurrent goroutines
}

// NewServer creates a new TWAMP server instance
func NewServer(logFile *os.File) *Server {
	logger := log.New(os.Stdout, "[TWAMP-Server] ", log.LstdFlags|log.Lmicroseconds)
	if logFile != nil {
		logger.SetOutput(logFile)
	}

	ctx, cancel := context.WithCancel(context.Background())

	return &Server{
		logFile:   logFile,
		logger:    logger,
		seqNumber: 0,
		ctx:       ctx,
		cancel:    cancel,
		semaphore: make(chan struct{}, maxConcurrentPackets),
	}
}

// Start begins listening for TWAMP test packets
func (s *Server) Start() error {
	addr := net.UDPAddr{
		Port: TWAMPPort,
		IP:   net.ParseIP("::"), // Listen on all IPv6 interfaces (includes IPv4-mapped)
	}

	conn, err := net.ListenUDP("udp", &addr)
	if err != nil {
		return fmt.Errorf("failed to start server: %v", err)
	}
	s.conn = conn

	// Set socket buffer sizes for better performance
	s.conn.SetReadBuffer(1024 * 1024)  // 1MB read buffer
	s.conn.SetWriteBuffer(1024 * 1024) // 1MB write buffer

	s.logger.Printf("TWAMP Reflector listening on port %d (IPv4/IPv6)", TWAMPPort)

	// Handle graceful shutdown
	go s.handleShutdown()

	// Main packet processing loop
	for {
		select {
		case <-s.ctx.Done():
			s.logger.Println("Server shutting down...")
			s.wg.Wait() // Wait for all goroutines to finish
			return nil
		default:
			// Get buffer from pool
			buf := bufferPool.Get().([]byte)

			// Set read deadline to allow periodic context checking
			s.conn.SetReadDeadline(time.Now().Add(1 * time.Second))

			n, clientAddr, err := s.conn.ReadFromUDP(buf)
			if err != nil {
				bufferPool.Put(buf)
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					continue // Timeout is expected, continue loop
				}
				if s.ctx.Err() != nil {
					return nil // Context cancelled
				}
				s.logger.Printf("Read error: %v", err)
				continue
			}

			// Acquire semaphore to limit concurrent goroutines
			s.semaphore <- struct{}{}
			s.wg.Add(1)

			// Process packet in goroutine for better performance
			go s.handleTestPacket(buf[:n], clientAddr)
		}
	}
}

// handleTestPacket processes a TWAMP test request and sends response
func (s *Server) handleTestPacket(data []byte, clientAddr *net.UDPAddr) {
	defer func() {
		bufferPool.Put(data[:cap(data)]) // Return full buffer to pool
		<-s.semaphore                     // Release semaphore
		s.wg.Done()
	}()

	// T2: Record receive timestamp immediately
	receiveTime := time.Now()

	// Parse request packet
	var req TWAMPTestRequest
	if err := req.UnmarshalBinary(data); err != nil {
		s.logger.Printf("Invalid packet from %s: %v", clientAddr, err)
		return
	}

	s.logger.Printf("Received test packet from %s: seq=%d", clientAddr, req.SequenceNumber)

	// Get TTL from received packet (requires socket options - simplified here)
	ttl := uint8(64) // Default, actual implementation would extract from IP header

	// Increment server's own sequence number
	s.seqMutex.Lock()
	s.seqNumber++
	currentSeq := s.seqNumber
	s.seqMutex.Unlock()

	// Create response packet
	response := TWAMPTestResponse{
		// Copy sender's original data
		SequenceNumber: req.SequenceNumber,
		Timestamp:      req.Timestamp,
		ErrorEstimate:  req.ErrorEstimate,
		MBZ:            0,

		// Add reflector data
		ReceiveTimestamp: receiveTime,
		SenderSeqNumber:  currentSeq,
		SenderTimestamp:  time.Now(), // T3: Send timestamp
		SenderError:      getErrorEstimate(),
		MBZ2:             0,
		SenderTTL:        ttl,
	}

	// Marshal response
	responseData := response.MarshalBinary()

	// Send response
	_, err := s.conn.WriteToUDP(responseData, clientAddr)
	if err != nil {
		s.logger.Printf("Failed to send response to %s: %v", clientAddr, err)
		return
	}

	s.logger.Printf("Sent response to %s: seq=%d, recv_time=%v, send_time=%v",
		clientAddr, req.SequenceNumber, receiveTime.Format(time.RFC3339Nano),
		response.SenderTimestamp.Format(time.RFC3339Nano))
}

// handleShutdown listens for shutdown signals
func (s *Server) handleShutdown() {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	<-sigChan
	s.logger.Println("Received shutdown signal")
	s.cancel()
	if s.conn != nil {
		s.conn.Close()
	}
}

// ============================================================================
// TWAMP Client (Session-Sender)
// ============================================================================

// Client represents a TWAMP test client
type Client struct {
	serverAddr string
	logFile    *os.File
	logger     *log.Logger
	count      int
	interval   time.Duration
}

// NewClient creates a new TWAMP client instance
func NewClient(serverAddr string, logFile *os.File, count int, interval time.Duration) *Client {
	logger := log.New(os.Stdout, "[TWAMP-Client] ", log.LstdFlags|log.Lmicroseconds)
	if logFile != nil {
		logger.SetOutput(logFile)
	}

	return &Client{
		serverAddr: serverAddr,
		logFile:    logFile,
		logger:     logger,
		count:      count,
		interval:   interval,
	}
}

// Run executes the TWAMP test session
func (c *Client) Run() error {
	// Resolve server address
	server := fmt.Sprintf("[%s]:%d", c.serverAddr, TWAMPPort)
	addr, err := net.ResolveUDPAddr("udp", server)
	if err != nil {
		return fmt.Errorf("failed to resolve address: %v", err)
	}

	// Connect to server
	conn, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		return fmt.Errorf("failed to connect: %v", err)
	}
	defer conn.Close()

	// Set socket buffer sizes
	conn.SetReadBuffer(1024 * 1024)
	conn.SetWriteBuffer(1024 * 1024)

	c.logger.Printf("Starting TWAMP test to %s", c.serverAddr)

	var stats []time.Duration

	for i := 0; i < c.count; i++ {
		rtt, err := c.sendTestPacket(conn, uint32(i+1))
		if err != nil {
			c.logger.Printf("Test %d failed: %v", i+1, err)
			continue
		}

		stats = append(stats, rtt)
		c.logger.Printf("Test %d: RTT = %v (%.3f ms)", i+1, rtt, float64(rtt.Microseconds())/1000.0)

		if i < c.count-1 {
			time.Sleep(c.interval)
		}
	}

	// Print statistics
	c.printStats(stats)

	return nil
}

// sendTestPacket sends a single test packet and calculates RTT
func (c *Client) sendTestPacket(conn *net.UDPConn, seqNum uint32) (time.Duration, error) {
	// T1: Create and send test request
	t1 := time.Now()
	request := TWAMPTestRequest{
		SequenceNumber: seqNum,
		Timestamp:      t1,
		ErrorEstimate:  getErrorEstimate(),
	}

	requestData := request.MarshalBinary()
	_, err := conn.Write(requestData)
	if err != nil {
		return 0, fmt.Errorf("send failed: %v", err)
	}

	// Wait for response with timeout
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	responseBuf := make([]byte, 1024)
	n, err := conn.Read(responseBuf)
	if err != nil {
		return 0, fmt.Errorf("receive failed: %v", err)
	}

	// T4: Record receive time immediately
	t4 := time.Now()

	// Parse response
	var response TWAMPTestResponse
	if err := response.UnmarshalBinary(responseBuf[:n]); err != nil {
		return 0, fmt.Errorf("invalid response: %v", err)
	}

	// Verify sequence number
	if response.SequenceNumber != seqNum {
		return 0, fmt.Errorf("sequence mismatch: expected %d, got %d", seqNum, response.SequenceNumber)
	}

	// Calculate RTT using four timestamps
	// T1 = request.Timestamp (client send)
	// T2 = response.ReceiveTimestamp (server receive)
	// T3 = response.SenderTimestamp (server send)
	// T4 = t4 (client receive)
	rtt := calculateRTT(t1, response.ReceiveTimestamp, response.SenderTimestamp, t4)

	return rtt, nil
}

// printStats displays test statistics
func (c *Client) printStats(rtts []time.Duration) {
	if len(rtts) == 0 {
		c.logger.Println("No successful tests")
		return
	}

	var sum, min, max time.Duration
	min = rtts[0]
	max = rtts[0]

	for _, rtt := range rtts {
		sum += rtt
		if rtt < min {
			min = rtt
		}
		if rtt > max {
			max = rtt
		}
	}

	avg := sum / time.Duration(len(rtts))

	c.logger.Println("=== Test Statistics ===")
	c.logger.Printf("Packets sent: %d", c.count)
	c.logger.Printf("Packets received: %d", len(rtts))
	c.logger.Printf("Loss: %.1f%%", float64(c.count-len(rtts))/float64(c.count)*100)
	c.logger.Printf("RTT min/avg/max: %.3f / %.3f / %.3f ms",
		float64(min.Microseconds())/1000.0,
		float64(avg.Microseconds())/1000.0,
		float64(max.Microseconds())/1000.0)
}

// ============================================================================
// Main and CLI
// ============================================================================

var (
	mode        = flag.String("mode", "client", "Mode: client or server")
	serverAddr  = flag.String("server", "localhost", "TWAMP server address (client mode only)")
	runAsDaemon = flag.Bool("daemon", false, "Run server as a daemon")
	logFilePath = flag.String("logfile", "", "Log file path (optional)")
	count       = flag.Int("count", 10, "Number of test packets to send (client mode)")
	interval    = flag.Duration("interval", 1*time.Second, "Interval between test packets (client mode)")
)

func main() {
	flag.Parse()

	// Setup logging
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

	// Run based on mode
	switch *mode {
	case "server":
		if *runAsDaemon {
			runServerAsDaemon()
		} else {
			server := NewServer(logFile)
			if err := server.Start(); err != nil {
				fmt.Fprintf(os.Stderr, "Server error: %v\n", err)
				os.Exit(1)
			}
		}

	case "client":
		client := NewClient(*serverAddr, logFile, *count, *interval)
		if err := client.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "Client error: %v\n", err)
			os.Exit(1)
		}

	default:
		fmt.Fprintf(os.Stderr, "Invalid mode. Use 'client' or 'server'\n")
		os.Exit(1)
	}
}

// runServerAsDaemon forks the process to run as a background daemon
func runServerAsDaemon() {
	attr := syscall.SysProcAttr{
		Setsid: true, // Start a new session and detach from terminal
	}

	cmd := exec.Command(os.Args[0], "-mode", "server", "-logfile", *logFilePath)
	cmd.SysProcAttr = &attr

	if *logFilePath != "" {
		logFile, err := os.OpenFile(*logFilePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			log.Printf("Error opening log file: %v", err)
			return
		}
		defer logFile.Close()
		cmd.Stdout = logFile
		cmd.Stderr = logFile
	} else {
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
	}

	if err := cmd.Start(); err != nil {
		log.Printf("Error starting daemon: %v", err)
		return
	}

	log.Printf("Server started as daemon (PID: %d)", cmd.Process.Pid)
	os.Exit(0)
}
