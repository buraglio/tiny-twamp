//go:build !windows

package main

import (
	"context"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

func platformHandleShutdown(s *Server, conn *net.UDPConn) {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan
	s.logger.Println("Received shutdown signal")
	s.cancel()
	conn.Close()
}

func platformWaitForShutdown(cancel context.CancelFunc, logger *log.Logger) {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan
	logger.Println("Received shutdown signal")
	cancel()
}

func runServerAsDaemon() {
	var args []string
	for _, a := range os.Args[1:] {
		if a == "-daemon" || a == "--daemon" {
			continue
		}
		args = append(args, a)
	}

	cmd := exec.Command(os.Args[0], args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if *logFilePath != "" {
		lf, err := os.OpenFile(*logFilePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			log.Printf("Error opening log file for daemon: %v", err)
			return
		}
		defer lf.Close()
		cmd.Stdout = lf
		cmd.Stderr = lf
	} else {
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
	}

	if err := cmd.Start(); err != nil {
		log.Printf("Error starting daemon: %v", err)
		return
	}

	log.Printf("TWAMP-Light server started as daemon (PID: %d)", cmd.Process.Pid)
	os.Exit(0)
}
