package main

import (
	"log"
	"net/http"
	"os"
	"time"
)

func main() {
	// Load configuration
	config := LoadConfig()

	// Create handler
	handler := NewHandler(config)

	// Set up routes
	mux := http.NewServeMux()
	mux.HandleFunc("/teams/webhook", handler.TeamsWebhook)

	// Start server with timeouts
	addr := ":" + config.Port
	server := &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}
	log.Printf("Starting server on %s", addr)
	if err := server.ListenAndServe(); err != nil {
		log.Printf("Server error: %v", err)
		os.Exit(1)
	}
}
