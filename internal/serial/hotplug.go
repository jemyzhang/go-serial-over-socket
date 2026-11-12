package serial

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"strings"

	"github.com/jochenvg/go-udev"
)

// DeviceEventType represents the type of device event
type DeviceEventType int

const (
	DeviceAdded DeviceEventType = iota
	DeviceRemoved
)

// DeviceEvent represents a hotplug event
type DeviceEvent struct {
	Type       DeviceEventType
	DevicePath string
	PortID     string
	PortInfo   *PortInfo
}

// DeviceEventHandler is a callback function for device events
type DeviceEventHandler func(event DeviceEvent)

// HotplugMonitor monitors USB serial device hotplug events
type HotplugMonitor struct {
	udev     *udev.Udev
	monitor  *udev.Monitor
	handlers []DeviceEventHandler
	stopChan chan struct{}
}

// NewHotplugMonitor creates a new hotplug monitor
func NewHotplugMonitor() (*HotplugMonitor, error) {
	u := udev.Udev{}

	return &HotplugMonitor{
		udev:     &u,
		handlers: make([]DeviceEventHandler, 0),
		stopChan: make(chan struct{}),
	}, nil
}

// RegisterHandler registers a callback for device events
func (hm *HotplugMonitor) RegisterHandler(handler DeviceEventHandler) {
	hm.handlers = append(hm.handlers, handler)
}

// Start begins monitoring for hotplug events
func (hm *HotplugMonitor) Start() error {
	// Create a monitor for the tty subsystem
	monitor := hm.udev.NewMonitorFromNetlink("udev")

	// Filter for tty subsystem events
	err := monitor.FilterAddMatchSubsystem("tty")
	if err != nil {
		return fmt.Errorf("failed to add subsystem filter: %w", err)
	}

	hm.monitor = monitor

	// Start monitoring in a goroutine
	go hm.monitorLoop()

	log.Println("Hotplug monitor started")
	return nil
}

// Stop stops the hotplug monitor
func (hm *HotplugMonitor) Stop() {
	if hm.stopChan != nil {
		close(hm.stopChan)
	}
	log.Println("Hotplug monitor stopped")
}

// monitorLoop is the main event loop for monitoring device events
func (hm *HotplugMonitor) monitorLoop() {
	// Create a context from the stop channel
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Monitor stop channel in a goroutine
	go func() {
		<-hm.stopChan
		cancel()
	}()

	// Create a channel to receive devices
	ch, errs, err := hm.monitor.DeviceChan(ctx)
	if err != nil {
		log.Printf("Failed to create device channel: %v", err)
		return
	}

	log.Println("Monitoring for USB serial device hotplug events...")

	for {
		select {
		case <-ctx.Done():
			return
		case err := <-errs:
			if err != nil {
				log.Printf("Error from device monitor: %v", err)
			}
		case device := <-ch:
			hm.handleDeviceEvent(device)
		}
	}
}

// handleDeviceEvent processes a udev device event
func (hm *HotplugMonitor) handleDeviceEvent(device *udev.Device) {
	if device == nil {
		return
	}

	action := device.Action()
	devNode := device.Devnode()
	subsystem := device.Subsystem()

	// Only process tty subsystem events
	if subsystem != "tty" {
		return
	}

	// Only process events with a device node
	if devNode == "" {
		return
	}

	// Extract port ID from device path
	portID := filepath.Base(devNode)

	// Filter for USB serial devices only
	if !isUSBSerialDevice(portID) {
		return
	}

	// Determine event type
	var eventType DeviceEventType
	switch action {
	case "add":
		eventType = DeviceAdded
		log.Printf("USB serial device added: %s (%s)", portID, devNode)
	case "remove":
		eventType = DeviceRemoved
		log.Printf("USB serial device removed: %s (%s)", portID, devNode)
	default:
		// Ignore other actions (change, bind, unbind, etc.)
		return
	}

	// Get port info for added devices
	var portInfo *PortInfo
	if eventType == DeviceAdded {
		info, err := GetPortInfo(devNode)
		if err != nil {
			log.Printf("Warning: failed to get port info for %s: %v", devNode, err)
			// Create basic port info from udev properties
			portInfo = &PortInfo{
				ID:         portID,
				DevicePath: devNode,
				Vendor:     extractVendor(device),
				Product:    extractProduct(device),
				Serial:     extractSerial(device),
				ByIDName:   extractByIDName(device),
			}
		} else {
			// Supplement with udev-derived ByIDName if scanner didn't find one
			if info.ByIDName == "" {
				info.ByIDName = extractByIDName(device)
			}
			if info.Serial == "" {
				info.Serial = extractSerial(device)
			}
			portInfo = info
		}
	}

	// Create event
	event := DeviceEvent{
		Type:       eventType,
		DevicePath: devNode,
		PortID:     portID,
		PortInfo:   portInfo,
	}

	// Notify all registered handlers
	for _, handler := range hm.handlers {
		handler(event)
	}
}

// extractVendor extracts vendor information from udev device
func extractVendor(device *udev.Device) string {
	for _, key := range []string{"ID_VENDOR", "ID_VENDOR_FROM_DATABASE"} {
		if v := device.PropertyValue(key); v != "" {
			return v
		}
	}
	if parent := device.Parent(); parent != nil {
		for _, key := range []string{"ID_VENDOR", "ID_VENDOR_FROM_DATABASE"} {
			if v := parent.PropertyValue(key); v != "" {
				return v
			}
		}
	}
	return "Unknown"
}

// extractProduct extracts product information from udev device
func extractProduct(device *udev.Device) string {
	for _, key := range []string{"ID_MODEL", "ID_MODEL_FROM_DATABASE"} {
		if v := device.PropertyValue(key); v != "" {
			return strings.ReplaceAll(v, "_", " ")
		}
	}
	if parent := device.Parent(); parent != nil {
		for _, key := range []string{"ID_MODEL", "ID_MODEL_FROM_DATABASE"} {
			if v := parent.PropertyValue(key); v != "" {
				return strings.ReplaceAll(v, "_", " ")
			}
		}
	}
	return "USB Serial Device"
}

// extractSerial extracts the USB serial number from udev device properties.
func extractSerial(device *udev.Device) string {
	for _, key := range []string{"ID_SERIAL_SHORT", "ID_USB_SERIAL_SHORT"} {
		if v := device.PropertyValue(key); v != "" {
			return v
		}
	}
	if parent := device.Parent(); parent != nil {
		for _, key := range []string{"ID_SERIAL_SHORT", "ID_USB_SERIAL_SHORT"} {
			if v := parent.PropertyValue(key); v != "" {
				return v
			}
		}
	}
	return ""
}

// extractByIDName reconstructs the /dev/serial/by-id/ symlink filename from udev properties.
// The kernel names these symlinks as:
//
//	usb-<ID_VENDOR_ENC>_<ID_MODEL_ENC>_<ID_SERIAL_SHORT>-if<N>-port<N>
//
// We use ID_ID (the full ID_SERIAL property which encodes vendor_model_serial) when
// available, falling back to building from parts.
func extractByIDName(device *udev.Device) string {
	// ID_ID is set by the udev usb_id builtin and encodes the full
	// "<vendor>_<model>_<serial>" string used in the by-id symlink.
	if id := device.PropertyValue("ID_ID"); id != "" {
		return "usb-" + id
	}

	// Fallback: try to reconstruct from individual properties
	vendorEnc := device.PropertyValue("ID_VENDOR_ENC")
	modelEnc := device.PropertyValue("ID_MODEL_ENC")
	serialShort := device.PropertyValue("ID_SERIAL_SHORT")

	if vendorEnc == "" {
		vendorEnc = strings.ReplaceAll(extractVendor(device), " ", "_")
	}
	if modelEnc == "" {
		modelEnc = strings.ReplaceAll(extractProduct(device), " ", "_")
	}

	if vendorEnc == "" && modelEnc == "" {
		return ""
	}

	name := "usb-" + vendorEnc + "_" + modelEnc
	if serialShort != "" {
		name += "_" + serialShort
	}
	return name
}
