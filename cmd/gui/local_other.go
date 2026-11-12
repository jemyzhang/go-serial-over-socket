//go:build !linux

package main

import (
	"context"
	"errors"
)

// localServer is the part of the embedded server the native GUI needs.
// Outside Linux there is none: serial enumeration and hot-plug depend
// on udev, so non-Linux builds are remote-only clients.
type localServer interface {
	URL() string
	Shutdown(ctx context.Context) error
}

const localModeSupported = false

func startLocalServer(port int) (localServer, error) {
	return nil, errors.New("local mode (embedded server) is only available on Linux; use -remote to connect to an sos-server")
}
