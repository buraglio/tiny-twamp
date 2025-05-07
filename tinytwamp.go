package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"time"
)

const (
	serverPort = ":862" // Standard TWAMP uses UDP port 862
)

var (
	mode        = flag.String("mode", "client", "Mode: client or server")
	serverAddr  = flag.String("server", "localhost", "TWAMP server address (client mode only)")
	runAsDaemon = flag.Bool("daemon", false, "Run server as a daemon")
	logFilePath = flag.String("logfile", "", "Log file path (optional)")
	testCount   = flag.Int("c", 1, "Number of tests to run (client mode only)") // Flag for test count
)

func main() {
	flag.Parse()

	// Setup logging for both server and client
	var logFile *os.File
	if *logFilePath != "" {
		var err error
		logFile, err = os.OpenFile(*logFilePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			fmt.Println("Error opening log file:", err)
			return
		}
		defer logFile.Close()

		log.SetOutput(logFile)
		log.Println("Logging started")
	} else {
		log.SetOutput(os.Stdout)
	}

	// Run the server or client based on the mode
	switch *mode {
	case "server":
		if *runAsDaemon {
			runServerAsDaemon(logFile) // Pass the log file path to the daemonized process
		} else {
			runServer(logFile)
		}
	case "client":
		runClient(logFile)
	default:
		fmt.Println("Invalid mode. Use 'client' or 'server'")
		os.Exit(1)
	}
}

// Daemon Mode: Server
func runServerAsDaemon(logFile *os.File) {
	log.Println("Server is running as a daemon...")
	runServer(logFile) // Reuse the original runServer function because I am lazy
}

// Original Server Function
func runServer(logFile *os.File) {
	// Listen on UDP port 862 for incoming requests, using IPv6
	addr := net.UDPAddr{
		Port: 862,
		IP:   net.ParseIP("::"), // Use "::" to listen on all available IPv6 interfaces
	}
	conn, err := net.ListenUDP("udp", &addr)
	if err != nil {
		log.Println("Error setting up server:", err)
		return
	}
	defer conn.Close()
	log.Println("TWAMP server is listening on IPv6 port 862...")

	buf := make([]byte, 1024)
	for {
		// Read the incoming packet
		n, clientAddr, err := conn.ReadFromUDP(buf)
		if err != nil {
			log.Println("Error reading from client:", err)
			continue
		}

		// Log the incoming request with timestamp and client info
		log.Printf("Received test packet from %s: %s\n", clientAddr.String(), string(buf[:n]))

		// Step 1: Parse the received packet to extract the timestamp
		receivedTimeString := string(buf[:n]) // The message sent by the client
		var clientTimestamp time.Time

		// Use time.Parse to parse the timestamp from the received message
		clientTimestamp, err = time.Parse(time.RFC3339, receivedTimeString[11:]) // Skipping "Timestamp: "
		if err != nil {
			log.Printf("Error parsing timestamp from client packet: %v\n", err)
			continue
		}

		// Step 2: Send the timestamp back to the client as part of the response
		responseMessage := fmt.Sprintf("Round-trip time: %s", clientTimestamp.Format(time.RFC3339))

		// Send the result back to the client
		_, err = conn.WriteToUDP([]byte(responseMessage), clientAddr)
		if err != nil {
			log.Println("Error sending to client:", err)
			continue
		}

		// Log the response sent to the client
		log.Printf("Sent response to %s: %s\n", clientAddr.String(), responseMessage)

		// Log the result of the test (round-trip time or any other result)
		log.Printf("Test result for client %s: Sent timestamp: %v\n", clientAddr.String(), clientTimestamp)
	}
}

// Client Function
func runClient(logFile *os.File) {
	// Connect to the TWAMP server over IPv6
	server := fmt.Sprintf("[%s]:862", *serverAddr)
	addr, err := net.ResolveUDPAddr("udp", server)
	if err != nil {
		log.Println("Error resolving address:", err)
		return
	}

	conn, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		log.Println("Error connecting to server:", err)
		return
	}
	defer conn.Close()

	// Loop for the specified number of tests
	for i := 0; i < *testCount; i++ {
		// Get the current timestamp for the test (before sending the packet)
		startTime := time.Now()

		// Send a test message with the timestamp
		message := fmt.Sprintf("Timestamp: %s", startTime.Format(time.RFC3339)) // Format timestamp in RFC3339
		_, err = conn.Write([]byte(message))
		if err != nil {
			log.Println("Error sending message:", err)
			return
		}

		// Log the sent message
		log.Printf("Client sent message: %s\n", message)

		// Wait for the reply (TWAMP response)
		response := make([]byte, 128)
		_, err = conn.Read(response)
		if err != nil {
			log.Println("Error reading reply:", err)
			return
		}

		// Trim the extra null bytes from the response
		trimmedResponse := strings.Trim(string(response), "\x00")

		// The server will send back the timestamp it received
		// We assume the response is in the format: "Round-trip time: <timestamp>"
		log.Printf("Client received response: %s\n", trimmedResponse)

		// Extract the timestamp from the server's response
		// Assuming the server response is in the format: "Round-trip time: <timestamp>"
		var serverTimestamp string
		_, err = fmt.Sscanf(trimmedResponse, "Round-trip time: %s", &serverTimestamp)
		if err != nil {
			log.Println("Error parsing timestamp from response:", err)
			return
		}

		// Calculate RTT by subtracting the client's sent time from the time we received the response
		rtt := time.Since(startTime) // Calculate RTT from when the client sent the message

		// Log the round-trip time
		log.Printf("Client calculated RTT: %v\n", rtt)

		// Optional: Add a delay between tests
		time.Sleep(1 * time.Second)
	}
}
