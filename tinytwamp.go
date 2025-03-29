package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"time"
)

const (
	serverPort = ":862" // TWAMP uses UDP port 862
)

var (
	mode       = flag.String("mode", "client", "Mode: client or server")
	serverAddr = flag.String("server", "localhost", "TWAMP server address (client mode only)")
)

func main() {
	flag.Parse()

	switch *mode {
	case "server":
		runServer()
	case "client":
		runClient()
	default:
		fmt.Println("Invalid mode. Use 'client' or 'server'")
		os.Exit(1)
	}
}

// TWAMP Test Server
func runServer() {
	// Listen on UDP port 862 for incoming requests, using IPv6
	addr := net.UDPAddr{
		Port: 862,
		IP:   net.ParseIP("::"), // Use "::" to listen on all available IPv6 interfaces
	}
	conn, err := net.ListenUDP("udp", &addr)
	if err != nil {
		fmt.Println("Error setting up server:", err)
		return
	}
	defer conn.Close()
	fmt.Println("TWAMP server is listening on IPv6 port 862...")

	buf := make([]byte, 1024)
	for {
		// Read the incoming packet
		_, clientAddr, err := conn.ReadFromUDP(buf)
		if err != nil {
			fmt.Println("Error reading from client:", err)
			continue
		}

		// Here, we can handle the incoming TWAMP request
		// In a real scenario, we would process the TWAMP packet and send a reply
		// For now, we just simulate an echo
		_, err = conn.WriteToUDP(buf, clientAddr)
		if err != nil {
			fmt.Println("Error sending to client:", err)
			continue
		}
	}
}

// TWAMP Client
func runClient() {
	// Connect to the TWAMP server over IPv6, ensuring the address is in square brackets
	server := fmt.Sprintf("[%s]:862", *serverAddr)
	addr, err := net.ResolveUDPAddr("udp", server)
	if err != nil {
		fmt.Println("Error resolving address:", err)
		return
	}

	conn, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		fmt.Println("Error connecting to server:", err)
		return
	}
	defer conn.Close()

	// Send a test message (TWAMP request)
	message := []byte("TWAMP test message")
	startTime := time.Now()
	_, err = conn.Write(message)
	if err != nil {
		fmt.Println("Error sending message:", err)
		return
	}

	// Wait for the reply (TWAMP response)
	_, err = conn.Read(make([]byte, 1024))
	if err != nil {
		fmt.Println("Error reading reply:", err)
		return
	}
	endTime := time.Now()

	// Calculate round-trip time
	roundTripTime := endTime.Sub(startTime)
	fmt.Printf("Round-trip time: %v\n", roundTripTime)
}
