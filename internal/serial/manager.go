package serial

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jemyzhang/serial-over-socket-go/config"
	"github.com/tarm/serial"
)

type PortStatus int32

const (
	StatusDisconnected PortStatus = iota
	StatusConnecting
	StatusConnected
	StatusError
)

type SerialPort struct {
	ID        string
	Config    *config.SerialPortConfig
	Status    PortStatus // Will be accessed atomically
	LastError error
	DataCount int64
	StartTime time.Time
	PortInfo  *PortInfo // cached hardware info (vendor, product, by-id name)

	// 内部状态
	port      io.ReadWriteCloser
	reader    *bufio.Reader
	mu        sync.RWMutex
	closeChan chan struct{}

	// 引用管理器以便调用处理器
	manager *Manager
}

type DataHandler func(portID string, data []byte)
type StatusHandler func(portID string, status PortStatus, err error)

type Manager struct {
	ports          map[string]*SerialPort
	mu             sync.RWMutex
	dataHandlers   []DataHandler
	statusHandlers []StatusHandler
	hotplugMonitor *HotplugMonitor
	autoDiscovery  bool
	// portProfiles mirrors config.PortProfiles: keyed by device description or portID.
	// Used to restore saved serial settings when a device is (re-)plugged.
	portProfiles map[string]*config.PortProfile
}

func NewManager() *Manager {
	return &Manager{
		ports:          make(map[string]*SerialPort),
		dataHandlers:   make([]DataHandler, 0),
		statusHandlers: make([]StatusHandler, 0),
		hotplugMonitor: nil,
		autoDiscovery:  false,
		portProfiles:   nil,
	}
}

// portDescription builds a human-readable description for a port, used as a
// fallback profile key when ByIDName is not available.
// Format: "<Vendor> <Product> <Serial>" or just "<Product>" when vendor is unknown.
func portDescription(info *PortInfo) string {
	if info == nil {
		return ""
	}
	desc := info.Product
	if info.Vendor != "" && info.Vendor != "Unknown" {
		desc = info.Vendor + " " + info.Product
	}
	if info.Serial != "" {
		desc += " " + info.Serial
	}
	return desc
}

// profileKeys returns the ordered list of keys to try when looking up a
// PortProfile. Priority:
//  1. ByIDName  — raw /dev/serial/by-id/ symlink name (most stable)
//  2. description — "<Vendor> <Product> <Serial>" string
//  3. portID    — "ttyUSB0" etc. (last resort)
func profileKeys(info *PortInfo, portID string) []string {
	var keys []string
	if info != nil && info.ByIDName != "" {
		keys = append(keys, info.ByIDName)
	}
	if desc := portDescription(info); desc != "" {
		keys = append(keys, desc)
	}
	if portID != "" {
		keys = append(keys, portID)
	}
	return keys
}

// resolvePortCfg returns the best serial config for a newly discovered port.
// Priority:
//  1. Saved PortProfile keyed by ByIDName (e.g. "usb-FTDI_FT232R_USB_UART_AK08MDAM-if00-port0")
//  2. Saved PortProfile keyed by device description (e.g. "Silicon Labs CP2102 0001")
//  3. Saved PortProfile keyed by portID (e.g. "ttyUSB0")
//  4. defaultConfig (server-wide default from config.json)
func (m *Manager) resolvePortCfg(portInfo *PortInfo, portID string, defaultConfig *config.SerialPortConfig) *config.SerialPortConfig {
	m.mu.RLock()
	profiles := m.portProfiles
	m.mu.RUnlock()

	if profiles != nil {
		for _, key := range profileKeys(portInfo, portID) {
			if p, ok := profiles[key]; ok && p != nil {
				log.Printf("[resolvePortCfg] %s: using saved profile for key %q (baud=%d)", portID, key, p.Baudrate)
				return &config.SerialPortConfig{
					Device:   defaultConfig.Device,
					Baudrate: p.Baudrate,
					Databits: p.Databits,
					Stopbit:  p.Stopbit,
					Parity:   p.Parity,
					Timeout:  defaultConfig.Timeout,
				}
			}
		}
	}

	// Fall back to server default
	log.Printf("[resolvePortCfg] %s: no saved profile, using default (baud=%d)", portID, defaultConfig.Baudrate)
	return defaultConfig
}

// validatePortID rejects IDs that could escape the intended device path
// (the ID is interpolated into "/dev/<id>" in several places).
func validatePortID(id string) error {
	if id == "" {
		return fmt.Errorf("port id must not be empty")
	}
	if strings.ContainsAny(id, `/\`) || strings.Contains(id, "..") {
		return fmt.Errorf("invalid port id %q", id)
	}
	return nil
}

// validateSerialConfig normalises a serial config, filling sane defaults for
// out-of-range values instead of passing garbage to the serial driver.
func validateSerialConfig(cfg *config.SerialPortConfig) (*config.SerialPortConfig, error) {
	if cfg == nil {
		return nil, fmt.Errorf("nil serial config")
	}
	if cfg.Device == "" {
		return nil, fmt.Errorf("device path must not be empty")
	}
	out := *cfg
	switch out.Databits {
	case 5, 6, 7, 8:
	default:
		log.Printf("Invalid databits %d, defaulting to 8", out.Databits)
		out.Databits = 8
	}
	switch out.Stopbit {
	case 1, 2:
	default:
		log.Printf("Invalid stopbit %d, defaulting to 1", out.Stopbit)
		out.Stopbit = 1
	}
	if len(out.Parity) == 0 {
		log.Printf("Empty parity, defaulting to N")
		out.Parity = "N"
	} else {
		switch p := out.Parity[0]; p {
		case 'N', 'n', 'O', 'o', 'E', 'e', 'M', 'm', 'S', 's':
		default:
			return nil, fmt.Errorf("invalid parity %q (want one of N/O/E/M/S)", out.Parity)
		}
	}
	if out.Baudrate <= 0 {
		return nil, fmt.Errorf("invalid baudrate %d", out.Baudrate)
	}
	return &out, nil
}

// Helper methods for atomic status operations.
// Status MUST only be accessed through these two helpers; direct field
// access is a data race (writers: readLoop goroutine, connect/disconnect).
func (p *SerialPort) getStatus() PortStatus {
	return PortStatus(atomic.LoadInt32((*int32)(&p.Status)))
}

func (p *SerialPort) setStatus(status PortStatus) {
	atomic.StoreInt32((*int32)(&p.Status), int32(status))
}

func (m *Manager) AddPort(id string, cfg *config.SerialPortConfig) error {
	return m.AddPortWithInfo(id, cfg, nil)
}

// AddPortWithInfo adds a port with optional hardware info (used by auto-discovery).
func (m *Manager) AddPortWithInfo(id string, cfg *config.SerialPortConfig, portInfo *PortInfo) error {
	if err := validatePortID(id); err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.ports[id]; exists {
		return fmt.Errorf("port %s already exists", id)
	}

	port := &SerialPort{
		ID:        id,
		Config:    cfg,
		Status:    StatusDisconnected,
		closeChan: make(chan struct{}),
		manager:   m,
		PortInfo:  portInfo,
	}

	m.ports[id] = port
	return nil
}

func (m *Manager) RemovePort(id string) error {
	m.mu.Lock()
	port, exists := m.ports[id]
	if !exists {
		m.mu.Unlock()
		return fmt.Errorf("port %s not found", id)
	}

	// Remove from map while holding lock to prevent other operations
	delete(m.ports, id)
	m.mu.Unlock()

	// Tear down the port regardless of its state. A connect() that is
	// still in flight will notice the state change and abort instead of
	// leaking a readLoop for a port that's no longer managed.
	// (port.shutdownRemoved calls notifyStatus() which acquires m.mu.RLock(),
	// so this must run after the manager lock is released.)
	port.shutdownRemoved()

	return nil
}

func (m *Manager) ConnectPort(id string) error {
	m.mu.RLock()
	port, exists := m.ports[id]
	m.mu.RUnlock()

	if !exists {
		return fmt.Errorf("port %s not found", id)
	}

	return port.connect()
}

func (m *Manager) DisconnectPort(id string) error {
	m.mu.RLock()
	port, exists := m.ports[id]
	m.mu.RUnlock()

	if !exists {
		return fmt.Errorf("port %s not found", id)
	}

	return port.disconnect()
}

func (m *Manager) WriteData(id string, data []byte) (int, error) {
	m.mu.RLock()
	port, exists := m.ports[id]
	m.mu.RUnlock()

	if !exists {
		return 0, fmt.Errorf("port %s not found", id)
	}

	return port.write(data)
}

func (m *Manager) GetPortStatus(id string) (PortStatus, error) {
	m.mu.RLock()
	port, exists := m.ports[id]
	m.mu.RUnlock()

	if !exists {
		return StatusDisconnected, fmt.Errorf("port %s not found", id)
	}

	// Use atomic load to avoid deadlock with connect()
	status := PortStatus(atomic.LoadInt32((*int32)(&port.Status)))

	return status, nil
}

func (m *Manager) ListPorts() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	ports := make([]string, 0, len(m.ports))
	for id := range m.ports {
		ports = append(ports, id)
	}
	return ports
}

// GetPortConfig returns a copy of the current serial config for the given port.
// Returns nil if the port does not exist.
func (m *Manager) GetPortConfig(id string) *config.SerialPortConfig {
	m.mu.RLock()
	port, exists := m.ports[id]
	m.mu.RUnlock()
	if !exists || port.Config == nil {
		return nil
	}
	// Return a copy so callers can't mutate internal state
	cfg := *port.Config
	return &cfg
}

// GetPortInfo returns a copy of the cached PortInfo for the given port.
// Returns nil if the port does not exist or has no PortInfo.
func (m *Manager) GetPortInfo(id string) *PortInfo {
	m.mu.RLock()
	port, exists := m.ports[id]
	m.mu.RUnlock()
	if !exists || port.PortInfo == nil {
		return nil
	}
	copy := *port.PortInfo
	return &copy
}

// GetPortAlias returns the alias (user-defined display name) for the given port,
// looked up from the portProfiles map using stable hardware keys only.
// The portID key (e.g. "ttyUSB0") is intentionally skipped here: portIDs are
// reassigned to different physical devices across reboots, so an alias stored
// under a portID would leak to whatever device happens to land on that node next.
func (m *Manager) GetPortAlias(id string) string {
	m.mu.RLock()
	port, exists := m.ports[id]
	profiles := m.portProfiles
	m.mu.RUnlock()

	if !exists || profiles == nil {
		return ""
	}

	var info *PortInfo
	if port.PortInfo != nil {
		info = port.PortInfo
	}

	for _, key := range profileKeys(info, id) {
		if key == id {
			continue // skip portID key — not hardware-specific
		}
		if p, ok := profiles[key]; ok && p != nil && p.Alias != "" {
			return p.Alias
		}
	}
	return ""
}

func (m *Manager) UpdatePortConfig(id string, cfg *config.SerialPortConfig) error {
	m.mu.RLock()
	port, exists := m.ports[id]
	m.mu.RUnlock()

	if !exists {
		return fmt.Errorf("port %s not found", id)
	}

	port.mu.Lock()
	defer port.mu.Unlock()

	// If port is connected, we need to reconnect with new config
	wasConnected := port.getStatus() == StatusConnected

	if wasConnected {
		// Disconnect first (without lock since we already have it)
		if port.port != nil {
			close(port.closeChan)
			port.closeChan = make(chan struct{})
			port.port.Close()
			port.port = nil
			port.reader = nil
		}
		port.setStatus(StatusDisconnected)
	}

	// Update configuration
	port.Config = cfg
	log.Printf("Updated configuration for port %s: %+v", id, cfg)

	// If it was connected, reconnect with new config
	if wasConnected {
		port.mu.Unlock()
		err := port.connect()
		port.mu.Lock()
		return err
	}

	return nil
}

func (m *Manager) RegisterDataHandler(handler DataHandler) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dataHandlers = append(m.dataHandlers, handler)
}

func (m *Manager) RegisterStatusHandler(handler StatusHandler) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.statusHandlers = append(m.statusHandlers, handler)
}

// StartAutoDiscovery initializes port scanning and hotplug monitoring.
// portProfiles (from config.PortProfiles) is used to restore saved baud-rate /
// data-bits / stop-bits / parity for recognised devices on plug-in.
func (m *Manager) StartAutoDiscovery(defaultConfig *config.SerialPortConfig, portProfiles map[string]*config.PortProfile) error {
	m.mu.Lock()
	m.autoDiscovery = true
	m.portProfiles = portProfiles // snapshot at startup; updated by UpdatePortProfiles
	m.mu.Unlock()

	log.Println("Starting auto-discovery of USB serial ports...")

	// Scan for existing ports
	ports, err := ScanUSBSerialPorts()
	if err != nil {
		log.Printf("Warning: failed to scan for USB serial ports: %v", err)
	} else {
		for _, portInfo := range ports {
			piCopy := portInfo // capture for pointer
			resolvedCfg := m.resolvePortCfg(&portInfo, portInfo.ID, defaultConfig)
			cfg := &config.SerialPortConfig{
				Device:   portInfo.DevicePath,
				Baudrate: resolvedCfg.Baudrate,
				Databits: resolvedCfg.Databits,
				Stopbit:  resolvedCfg.Stopbit,
				Parity:   resolvedCfg.Parity,
				Timeout:  resolvedCfg.Timeout,
			}

			err := m.AddPortWithInfo(portInfo.ID, cfg, &piCopy)
			if err != nil {
				log.Printf("Warning: failed to add port %s: %v", portInfo.ID, err)
			} else {
				log.Printf("Added port %s (%s - %s %s by-id=%s) baud=%d",
					portInfo.ID, portInfo.DevicePath, portInfo.Vendor, portInfo.Product, portInfo.ByIDName, cfg.Baudrate)
			}
		}
	}

	// Start hotplug monitor
	monitor, err := NewHotplugMonitor()
	if err != nil {
		return fmt.Errorf("failed to create hotplug monitor: %w", err)
	}

	monitor.RegisterHandler(func(event DeviceEvent) {
		switch event.Type {
		case DeviceAdded:
			m.handleDeviceAdded(event, defaultConfig)
		case DeviceRemoved:
			m.handleDeviceRemoved(event)
		}
	})

	err = monitor.Start()
	if err != nil {
		return fmt.Errorf("failed to start hotplug monitor: %w", err)
	}

	m.mu.Lock()
	m.hotplugMonitor = monitor
	m.mu.Unlock()

	log.Println("Auto-discovery started successfully")
	return nil
}

// UpdatePortProfiles replaces the in-memory profile map so that future
// hotplug events immediately pick up any profiles saved via the WebUI.
func (m *Manager) UpdatePortProfiles(profiles map[string]*config.PortProfile) {
	m.mu.Lock()
	m.portProfiles = profiles
	m.mu.Unlock()
}

// StopAutoDiscovery stops the hotplug monitor
func (m *Manager) StopAutoDiscovery() {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.hotplugMonitor != nil {
		m.hotplugMonitor.Stop()
		m.hotplugMonitor = nil
	}

	m.autoDiscovery = false
	log.Println("Auto-discovery stopped")
}

// handleDeviceAdded handles a device add event
func (m *Manager) handleDeviceAdded(event DeviceEvent, defaultConfig *config.SerialPortConfig) {
	log.Printf("Handling device added event: %s", event.PortID)

	// Resolve config: prefer saved PortProfile, fall back to defaultConfig
	resolvedCfg := m.resolvePortCfg(event.PortInfo, event.PortID, defaultConfig)
	cfg := &config.SerialPortConfig{
		Device:   event.DevicePath,
		Baudrate: resolvedCfg.Baudrate,
		Databits: resolvedCfg.Databits,
		Stopbit:  resolvedCfg.Stopbit,
		Parity:   resolvedCfg.Parity,
		Timeout:  resolvedCfg.Timeout,
	}

	// Add the port with cached PortInfo
	err := m.AddPortWithInfo(event.PortID, cfg, event.PortInfo)
	if err != nil {
		log.Printf("Failed to add port %s: %v", event.PortID, err)
		return
	}

	if event.PortInfo != nil {
		log.Printf("Added port %s (%s - %s %s)", event.PortID, event.DevicePath, event.PortInfo.Vendor, event.PortInfo.Product)
	} else {
		log.Printf("Added port %s (%s)", event.PortID, event.DevicePath)
	}

	// Notify status handlers about the new port
	m.mu.RLock()
	handlers := make([]StatusHandler, len(m.statusHandlers))
	copy(handlers, m.statusHandlers)
	m.mu.RUnlock()

	for _, handler := range handlers {
		handler(event.PortID, StatusDisconnected, nil)
	}
}

// handleDeviceRemoved handles a device remove event
func (m *Manager) handleDeviceRemoved(event DeviceEvent) {
	log.Printf("Handling device removed event: %s", event.PortID)

	// Disconnect and remove the port
	err := m.RemovePort(event.PortID)
	if err != nil {
		log.Printf("Failed to remove port %s: %v", event.PortID, err)
		return
	}

	log.Printf("Removed port %s", event.PortID)

	// Notify status handlers about the removed port
	m.mu.RLock()
	handlers := make([]StatusHandler, len(m.statusHandlers))
	copy(handlers, m.statusHandlers)
	m.mu.RUnlock()

	for _, handler := range handlers {
		handler(event.PortID, StatusDisconnected, fmt.Errorf("device removed"))
	}
}

func (p *SerialPort) connect() error {
	// Fast path: already connected or a connect is in flight.
	switch p.getStatus() {
	case StatusConnected, StatusConnecting:
		return nil
	}

	// Validate config BEFORE touching any state (previously an empty
	// Parity string panicked here with an index-out-of-range).
	cfg, err := validateSerialConfig(p.Config)
	if err != nil {
		p.failConnect(err)
		return err
	}

	// Claim the Connecting state under lock, then do all blocking I/O
	// WITHOUT holding p.mu, so concurrent readers/writers never block
	// on a slow open() and status handlers can't deadlock back into us.
	p.mu.Lock()
	if p.getStatus() == StatusConnected || p.getStatus() == StatusConnecting {
		p.mu.Unlock()
		return nil
	}
	p.Config = cfg
	p.setStatus(StatusConnecting)
	p.mu.Unlock()
	p.notifyStatus()

	log.Printf("Opening serial port %s: device=%s baud=%d data=%d stop=%d parity=%s",
		p.ID, cfg.Device, cfg.Baudrate, cfg.Databits, cfg.Stopbit, cfg.Parity)

	readTimeoutMs := cfg.Timeout
	if readTimeoutMs <= 0 {
		readTimeoutMs = 200
	}
	mode := &serial.Config{
		Name:        cfg.Device,
		Baud:        cfg.Baudrate,
		Size:        byte(cfg.Databits),
		StopBits:    serial.StopBits(cfg.Stopbit),
		Parity:      serial.Parity(cfg.Parity[0]),
		ReadTimeout: time.Duration(readTimeoutMs) * time.Millisecond,
	}

	port, err := serial.OpenPort(mode)
	if err != nil {
		_, statErr := os.Stat(cfg.Device)
		log.Printf("Failed to open serial port %s: %v (device exists: %v)", cfg.Device, err, statErr == nil)
		p.failConnect(fmt.Errorf("failed to open serial port %s: %v (check permissions and device status)", cfg.Device, err))
		return err
	}

	// Re-acquire the lock and make sure nobody changed our state while
	// we were blocked in open() (e.g. a concurrent UpdatePortConfig).
	// If the state moved on, close what we just opened and bail out
	// instead of leaking a second readLoop.
	p.mu.Lock()
	if p.getStatus() != StatusConnecting {
		p.mu.Unlock()
		port.Close()
		return fmt.Errorf("port %s state changed during connect, aborting", p.ID)
	}
	p.port = port
	p.reader = bufio.NewReader(port)
	p.setStatus(StatusConnected)
	p.StartTime = time.Now()
	// Capture the channel and reader for THIS session so that the goroutine
	// is bound to exactly these objects and can never race a later session.
	closeChanCopy := p.closeChan
	readerCopy := p.reader
	p.mu.Unlock()

	p.notifyStatus()
	go p.readLoopWith(closeChanCopy, readerCopy)

	log.Printf("Serial port %s connected successfully", p.ID)
	return nil
}

// shutdownRemoved forcefully tears down a port that is being removed from
// the manager, from any state (Connected, Connecting, Error, ...).
func (p *SerialPort) shutdownRemoved() {
	p.mu.Lock()
	// closeChan may already be closed by disconnect(); close at most once.
	select {
	case <-p.closeChan:
	default:
		close(p.closeChan)
	}
	p.closeChan = make(chan struct{})
	port := p.port
	p.port = nil
	p.reader = nil
	p.setStatus(StatusDisconnected)
	p.mu.Unlock()

	if port != nil {
		if err := port.Close(); err != nil {
			log.Printf("Error closing serial port %s: %v", p.ID, err)
		}
	}
	p.notifyStatus()
}

// failConnect records a connection failure. The status notification happens
// outside any lock so handlers can never deadlock back into the port.
func (p *SerialPort) failConnect(err error) {
	p.mu.Lock()
	p.LastError = err
	// Only move to Error from Connecting; a concurrent disconnect() may
	// have already taken us elsewhere.
	if p.getStatus() == StatusConnecting {
		p.setStatus(StatusError)
	}
	p.mu.Unlock()
	p.notifyStatus()
}

func (p *SerialPort) disconnect() error {
	p.mu.Lock()
	if p.getStatus() != StatusConnected {
		p.mu.Unlock()
		return nil
	}

	close(p.closeChan)
	// Recreate channel for potential future reconnections
	p.closeChan = make(chan struct{})

	port := p.port
	p.port = nil
	p.reader = nil
	p.setStatus(StatusDisconnected)
	p.mu.Unlock()

	// Blocking close() and handler notification both happen outside the
	// lock: the readLoop is already signalled via closeChan and will exit
	// on its own; handlers may call back into the manager safely.
	if port != nil {
		if err := port.Close(); err != nil {
			log.Printf("Error closing serial port %s: %v", p.ID, err)
		}
	}
	p.notifyStatus()

	log.Printf("Serial port %s disconnected", p.ID)
	return nil
}

func (p *SerialPort) write(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}

	// For small writes (like normal typing), send immediately without chunking
	if len(data) <= 4 {
		return p.writeChunk(data)
	}

	// For larger writes (like pasting), send in small chunks with short delays
	// to prevent target embedded device's UART buffer from overrunning.
	// Chunk size / delay are tunable per port (config); defaults keep the
	// previous behaviour (16 bytes / 30ms).
	p.mu.RLock()
	chunkSize := p.Config.WriteChunkSize
	delayMs := p.Config.WriteChunkDelayMs
	p.mu.RUnlock()
	if chunkSize <= 0 {
		chunkSize = 16
	}
	if delayMs <= 0 {
		delayMs = 30
	}
	delay := time.Duration(delayMs) * time.Millisecond

	totalWritten := 0
	for i := 0; i < len(data); i += chunkSize {
		end := i + chunkSize
		if end > len(data) {
			end = len(data)
		}

		n, err := p.writeChunk(data[i:end])
		totalWritten += n
		if err != nil {
			return totalWritten, err
		}

		if end < len(data) {
			// Backpressure delay to let the receiving CPU read from its UART FIFO
			time.Sleep(delay)
		}
	}

	return totalWritten, nil
}

func (p *SerialPort) writeChunk(data []byte) (int, error) {
	p.mu.Lock()
	if p.getStatus() != StatusConnected || p.port == nil {
		p.mu.Unlock()
		return 0, fmt.Errorf("port not connected")
	}
	port := p.port
	p.mu.Unlock()

	n, err := port.Write(data)
	if err != nil {
		log.Printf("Error writing to port %s: %v", p.ID, err)
		p.mu.Lock()
		p.LastError = err
		wasError := p.getStatus() == StatusError
		if !wasError {
			p.setStatus(StatusError)
		}
		p.mu.Unlock()
		if !wasError {
			p.notifyStatus()
		}
		return n, err
	}

	return n, nil
}

// readLoopWith reads from reader until closeChan is closed.
// Both arguments are captured at goroutine-launch time so this goroutine
// is bound to a specific connection session and cannot accidentally race
// against a new readLoop goroutine started after a reconnect.
//
// A persistently failing device (e.g. yanked without a hotplug event)
// backs off exponentially and eventually gives up instead of logging and
// broadcasting status every 100ms forever.
func (p *SerialPort) readLoopWith(closeChan <-chan struct{}, reader *bufio.Reader) {
	const maxConsecutiveErrors = 30

	buffer := make([]byte, 1024)
	consecutiveErrors := 0

	for {
		select {
		case <-closeChan:
			// Our session was closed — exit cleanly.
			return
		default:
			n, err := reader.Read(buffer)
			if err != nil {
				// Check stop signal first so we don't log spurious errors on
				// intentional close.
				select {
				case <-closeChan:
					return
				default:
				}
				if err == io.EOF || errors.Is(err, os.ErrDeadlineExceeded) {
					// Normal idle timeout, not a failure.
					consecutiveErrors = 0
					continue
				}
				consecutiveErrors++
				if consecutiveErrors >= maxConsecutiveErrors {
					log.Printf("Port %s: %d consecutive read errors, giving up: %v",
						p.ID, consecutiveErrors, err)
					p.markReadError(err)
					return
				}
				// Exponential backoff: 100ms, 200ms, ... capped at 2s.
				// Only the first error and state transitions are logged /
				// broadcast, so a dying device doesn't spam either channel.
				backoff := 100 * time.Millisecond << (consecutiveErrors - 1)
				if backoff > 2*time.Second {
					backoff = 2 * time.Second
				}
				if consecutiveErrors == 1 {
					log.Printf("Error reading from port %s: %v", p.ID, err)
				}
				p.markReadError(err)
				select {
				case <-closeChan:
					return
				case <-time.After(backoff):
				}
				continue
			}

			consecutiveErrors = 0
			if n > 0 {
				data := make([]byte, n)
				copy(data, buffer[:n])
				p.handleData(data)
			}
		}
	}
}

// markReadError records a read failure and moves the port to StatusError,
// notifying handlers only on the transition (not on every repeated error).
func (p *SerialPort) markReadError(err error) {
	p.mu.Lock()
	p.LastError = err
	wasError := p.getStatus() == StatusError
	if !wasError {
		p.setStatus(StatusError)
	}
	p.mu.Unlock()
	if !wasError {
		p.notifyStatus()
	}
}

func (p *SerialPort) handleData(data []byte) {
	atomic.AddInt64(&p.DataCount, int64(len(data)))

	// 通知数据处理器
	if p.manager != nil {
		p.manager.mu.RLock()
		handlers := make([]DataHandler, len(p.manager.dataHandlers))
		copy(handlers, p.manager.dataHandlers)
		p.manager.mu.RUnlock()

		for _, handler := range handlers {
			handler(p.ID, data)
		}
	}
}

func (p *SerialPort) notifyStatus() {
	// 通知状态处理器
	if p.manager != nil {
		p.manager.mu.RLock()
		handlers := make([]StatusHandler, len(p.manager.statusHandlers))
		copy(handlers, p.manager.statusHandlers)
		p.manager.mu.RUnlock()

		status := p.getStatus()
		p.mu.RLock()
		lastErr := p.LastError
		p.mu.RUnlock()
		for _, handler := range handlers {
			handler(p.ID, status, lastErr)
		}
	}
}
