package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
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

	// Set up logging if a log file path is provided
	if *logFilePath != "" {
		logFile, err := os.OpenFile(*logFilePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
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

	switch *mode {
	case "server":
		if *runAsDaemon {
			runServerAsDaemon()
		} else {
			runServer()
		}
	case "client":
		runClient()
	default:
		fmt.Println("Invalid mode. Use 'client' or 'server'")
		os.Exit(1)
	}
}

// TWAMP Test Server (interactive mode)
func runServer() {
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
		_, clientAddr, err := conn.ReadFromUDP(buf)
		if err != nil {
			log.Println("Error reading from client:", err)
			continue
		}

		// Here, we can handle the incoming TWAMP request
		// In a real scenario, we would process the TWAMP packet and send a reply
		// For now, we just simulate an echo
		_, err = conn.WriteToUDP(buf, clientAddr)
		if err != nil {
			log.Println("Error sending to client:", err)
			continue
		}
	}
}

// TWAMP Test Server (daemon mode)
func runServerAsDaemon() {
	// Create a new process to run the server in the background (daemon)
	cmd := exec.Command(os.Args[0], "-mode", "server")
	cmd.Stdout = nil
	cmd.Stderr = nil
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
func runClient() {
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
}
