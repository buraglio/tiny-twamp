package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"syscall"
	"time"
)

const (
	serverPort = ":862" // TWAMP uses UDP port 862
)

var (
	mode        = flag.String("mode", "client", "Mode: client or server")
	serverAddr  = flag.String("server", "localhost", "TWAMP server address (client mode only)")
	runAsDaemon = flag.Bool("daemon", false, "Run server as a daemon")
	logFilePath = flag.String("logfile", "", "Log file path (optional)")
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
			runServerAsDaemon() // Pass the log file path to the daemonized process
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

// TWAMP Test Server (interactive mode)
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

		// Here, we can handle the incoming TWAMP request
		// In a real scenario, we would process the TWAMP packet and send a reply
		// For now, we just simulate an echo
		_, err = conn.WriteToUDP(buf[:n], clientAddr)
		if err != nil {
			log.Println("Error sending to client:", err)
			continue
		}

		// Log the response sent to the client
		log.Printf("Sent response to %s\n", clientAddr.String())

		// Log the result of the test (e.g., round-trip time or any other result)
		// This assumes the round-trip time is sent as a part of the message from the client
		// Simulate that the client sends the result back as a message
		log.Printf("Test result for client %s: Round-trip time simulated\n", clientAddr.String())
	}
}

// TWAMP Test Server (daemon mode)
func runServerAsDaemon() {
	// Fork the process to run it as a daemon
	// Detach the process from the terminal and run it in the background
	attr := syscall.SysProcAttr{
		Setsid: true, // Start a new session and detach from the terminal
	}

	cmd := exec.Command(os.Args[0], "-mode", "server", "-logfile", *logFilePath)
	cmd.SysProcAttr = &attr

	// Set up the log file for the daemon
	if *logFilePath != "" {
		logFile, err := os.OpenFile(*logFilePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			log.Println("Error opening log file:", err)
			return
		}
		defer logFile.Close()
		cmd.Stdout = logFile
		cmd.Stderr = logFile
	} else {
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
	}

	// Start the server as a daemon, redirecting output to the log file
	err := cmd.Start()
	if err != nil {
		log.Println("Error starting daemon:", err)
		return
	}
	log.Println("Server is running as a daemon...")

	// Exit the current process (parent) immediately
	os.Exit(0)
}

// TWAMP Client
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

	// Send a test message (TWAMP request)
	message := []byte("TWAMP test message")
	startTime := time.Now()
	_, err = conn.Write(message)
	if err != nil {
		log.Println("Error sending message:", err)
		return
	}

	// Wait for the reply (TWAMP response)
	_, err = conn.Read(make([]byte, 1024))
	if err != nil {
		log.Println("Error reading reply:", err)
		return
	}
	endTime := time.Now()

	// Calculate round-trip time
	roundTripTime := endTime.Sub(startTime)
	log.Printf("Round-trip time: %v\n", roundTripTime)

	// Send round-trip time back to the server
	// In a real-world scenario, you would send the round-trip time as part of the response
	responseMessage := fmt.Sprintf("Round-trip time: %v", roundTripTime)
	_, err = conn.Write([]byte(responseMessage))
	if err != nil {
		log.Println("Error sending round-trip time back to server:", err)
		return
	}

	// Log the round-trip time for the client
	log.Printf("Client logged round-trip time: %v", roundTripTime)
}
