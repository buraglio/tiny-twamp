//go:build windows

package main

import (
	"fmt"
	"net"
	"os"
	"os/signal"
)

func platformHandleShutdown(s *Server, conn *net.UDPConn) {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt)
	<-sigChan
	s.logger.Println("Received shutdown signal")
	s.cancel()
	conn.Close()
}

func runServerAsDaemon() {
	fmt.Fprintln(os.Stderr, "daemon mode is not supported on Windows")
	os.Exit(1)
}
