package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jemyzhang/serial-over-socket-go/config"
	"github.com/jemyzhang/serial-over-socket-go/internal/appserver"
	"github.com/jemyzhang/serial-over-socket-go/pkg/version"
)

var (
	address     = flag.String("address", "0.0.0.0", "Server address")
	port        = flag.Int("port", 2025, "Server port")
	showHelp    = flag.Bool("h", false, "Show help")
	showVersion = flag.Bool("v", false, "Show version")
)

func printVersion() {
	version.Print("Serial Over Socket Server (Go Version)")
}

func printHelp(progName string) {
	fmt.Printf("Usage: %s [options]\n\n", progName)
	fmt.Println("Options:")
	fmt.Println("  -address string : Server address (default \"0.0.0.0\")")
	fmt.Println("  -port int       : Server port (default 2025)")
	fmt.Println("  -v              : Show version info")
	fmt.Println("  -h              : Print this help")
}

func main() {
	flag.Usage = func() {
		printHelp(os.Args[0])
	}
	flag.Parse()

	if *showVersion {
		printVersion()
		os.Exit(0)
	}

	if *showHelp {
		printHelp(os.Args[0])
		os.Exit(0)
	}

	// Load the saved configuration (e.g. WebDAV config from the WebUI),
	// then apply explicitly provided command-line flags on top.
	cfg := appserver.LoadConfig()

	flag.Visit(func(f *flag.Flag) {
		if f.Name == "address" {
			cfg.Server.Address = *address
		} else if f.Name == "port" {
			cfg.Server.Port = *port
		}
	})

	// If flags were not visited but config is empty, initialize them
	if cfg.Server.Address == "" {
		cfg.Server.Address = *address
	}
	if cfg.Server.Port == 0 {
		cfg.Server.Port = *port
	}

	// Setup signal handling
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Run server
	runServer(cfg, sigChan)
}

func runServer(cfg *config.Config, sigChan chan os.Signal) {
	log.Println("Starting Serial Over Socket Server...")

	srv, err := appserver.Start(cfg)
	if err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}

	log.Println("Server started successfully")
	log.Printf("  Web Interface: http://%s:%d", cfg.Server.Address, cfg.Server.Port)
	log.Printf("  WebSocket: ws://%s:%d/ws", cfg.Server.Address, cfg.Server.Port)

	// Wait for exit signal
	<-sigChan
	log.Println("Shutting down server...")

	// Graceful shutdown: close serial ports + WS clients, then stop HTTP.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("Shutdown error: %v", err)
	}

	log.Println("Server stopped")
}
