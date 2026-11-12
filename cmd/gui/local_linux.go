//go:build linux

package main

import (
	"context"

	"github.com/jemyzhang/serial-over-socket-go/internal/appserver"
)

// localServer is the part of the embedded server the native GUI needs.
// The embedded server needs udev, so local mode exists on Linux only.
type localServer interface {
	URL() string
	Shutdown(ctx context.Context) error
}

const localModeSupported = true

// startLocalServer starts the embedded sos server, bound to loopback
// only: it exists to serve this app's own window, not the network.
func startLocalServer(port int) (localServer, error) {
	cfg := appserver.LoadConfig()
	cfg.Server.Address = "127.0.0.1"
	if port > 0 {
		cfg.Server.Port = port
	}
	if cfg.Server.Port == 0 {
		cfg.Server.Port = 2025
	}
	return appserver.Start(cfg)
}
