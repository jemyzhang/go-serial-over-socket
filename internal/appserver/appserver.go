// Package appserver owns the lifecycle of the whole sos server stack
// (serial manager + web server) so it can be run by the standalone
// sos-server binary as well as embedded inside the GUI app, where the
// same process also hosts the client window.
package appserver

import (
	"context"
	"fmt"
	"log"
	"net/http"

	resources "github.com/jemyzhang/serial-over-socket-go"
	"github.com/jemyzhang/serial-over-socket-go/config"
	"github.com/jemyzhang/serial-over-socket-go/internal/serial"
	"github.com/jemyzhang/serial-over-socket-go/internal/web"
)

// Server is a running sos server stack. Create one with Start and stop
// it with Shutdown.
type Server struct {
	cfg       *config.Config
	serialMgr *serial.Manager
	webServer *web.WebServer
}

// LoadConfig returns the effective configuration the same way the
// standalone server builds it: defaults overlaid with the saved config
// file (WebUI settings such as WebDAV survive restarts that way).
// Callers may adjust fields (e.g. bind address) before calling Start.
func LoadConfig() *config.Config {
	cfg := config.DefaultConfig()
	if loaded, err := config.LoadConfig(config.GetConfigPath()); err == nil {
		cfg = loaded
	}
	return cfg
}

// Start brings up serial auto-discovery and the web server. The web
// server runs in the background; use Shutdown to stop everything.
func Start(cfg *config.Config) (*Server, error) {
	serialMgr := serial.NewManager()

	// USB auto-discovery (PortProfiles apply saved baud rates on plug-in).
	if err := serialMgr.StartAutoDiscovery(&cfg.SerialPort, cfg.PortProfiles); err != nil {
		log.Printf("Warning: failed to start auto-discovery: %v", err)
	}

	webServer, err := web.NewWebServer(cfg, serialMgr, resources.TemplatesFS, resources.StaticFS)
	if err != nil {
		serialMgr.StopAutoDiscovery()
		return nil, fmt.Errorf("create web server: %w", err)
	}

	serialMgr.RegisterDataHandler(webServer.HandleSerialData)
	webServer.StartCleanup()

	go func() {
		if err := webServer.Start(); err != nil && err != http.ErrServerClosed {
			log.Printf("Web server error: %v", err)
		}
	}()

	return &Server{cfg: cfg, serialMgr: serialMgr, webServer: webServer}, nil
}

// URL is the WebUI address of the running server.
func (s *Server) URL() string {
	return fmt.Sprintf("http://%s:%d", s.cfg.Server.Address, s.cfg.Server.Port)
}

// Shutdown gracefully stops the server: serial ports and WebSocket
// clients are closed first, then HTTP, then auto-discovery.
func (s *Server) Shutdown(ctx context.Context) error {
	err := s.webServer.Shutdown(ctx)
	s.serialMgr.StopAutoDiscovery()
	return err
}
