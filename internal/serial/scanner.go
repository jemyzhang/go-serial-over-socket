package serial

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// PortInfo contains metadata about a discovered serial port
type PortInfo struct {
	ID          string // Port identifier (e.g., "ttyUSB0")
	DevicePath  string // Full device path (e.g., "/dev/ttyUSB0")
	SymlinkPath string // Symlink path from /dev/serial/by-id/ if available
	ByIDName    string // Raw symlink filename from /dev/serial/by-id/ (stable unique key)
	Vendor      string // USB vendor information
	Product     string // USB product information
	Serial      string // USB serial number if available
}

// ScanUSBSerialPorts scans for all available USB serial ports
func ScanUSBSerialPorts() ([]PortInfo, error) {
	ports := make([]PortInfo, 0)

	// First, try to scan /dev/serial/by-id/ for detailed information
	byIDPorts, err := scanByID()
	if err == nil && len(byIDPorts) > 0 {
		ports = append(ports, byIDPorts...)
	}

	// Also scan /dev directly for any ports that might not have symlinks
	devPorts, err := scanDevDirectory()
	if err == nil {
		// Add ports that aren't already in the list
		for _, devPort := range devPorts {
			found := false
			for _, existingPort := range ports {
				if existingPort.ID == devPort.ID {
					found = true
					break
				}
			}
			if !found {
				ports = append(ports, devPort)
			}
		}
	}

	log.Printf("Discovered %d USB serial port(s)", len(ports))
	for _, port := range ports {
		log.Printf("  - %s: %s (vendor: %s, product: %s, serial: %s, by-id: %s)",
			port.ID, port.DevicePath, port.Vendor, port.Product, port.Serial, port.ByIDName)
	}

	return ports, nil
}

// scanByID scans /dev/serial/by-id/ for USB serial devices
func scanByID() ([]PortInfo, error) {
	byIDPath := "/dev/serial/by-id"

	entries, err := os.ReadDir(byIDPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read %s: %w", byIDPath, err)
	}

	ports := make([]PortInfo, 0)
	for _, entry := range entries {
		symlinkName := entry.Name()
		symlinkPath := filepath.Join(byIDPath, symlinkName)

		// Resolve the symlink to get the actual device path
		devicePath, err := filepath.EvalSymlinks(symlinkPath)
		if err != nil {
			log.Printf("Warning: failed to resolve symlink %s: %v", symlinkPath, err)
			continue
		}

		// Extract port ID from device path (e.g., "ttyUSB0" from "/dev/ttyUSB0")
		portID := filepath.Base(devicePath)

		// Parse vendor, product and serial info from the symlink name
		vendor, product, serial := parseUSBInfoFull(symlinkName)

		ports = append(ports, PortInfo{
			ID:          portID,
			DevicePath:  devicePath,
			SymlinkPath: symlinkPath,
			ByIDName:    symlinkName, // raw symlink filename — stable unique hardware key
			Vendor:      vendor,
			Product:     product,
			Serial:      serial,
		})
	}

	return ports, nil
}

// scanDevDirectory scans /dev for USB serial devices
func scanDevDirectory() ([]PortInfo, error) {
	devPath := "/dev"

	entries, err := os.ReadDir(devPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read %s: %w", devPath, err)
	}

	ports := make([]PortInfo, 0)
	for _, entry := range entries {
		name := entry.Name()

		// Filter for common USB serial device names
		if !isUSBSerialDevice(name) {
			continue
		}

		devicePath := filepath.Join(devPath, name)

		// Check if it's a character device
		info, err := entry.Info()
		if err != nil {
			continue
		}

		if info.Mode()&os.ModeCharDevice == 0 {
			continue
		}

		ports = append(ports, PortInfo{
			ID:         name,
			DevicePath: devicePath,
			Vendor:     "Unknown",
			Product:    "USB Serial Device",
		})
	}

	return ports, nil
}

// isUSBSerialDevice checks if a device name matches common USB serial patterns
func isUSBSerialDevice(name string) bool {
	// Common USB serial device prefixes
	prefixes := []string{"ttyUSB", "ttyACM", "ttyAMA"}

	for _, prefix := range prefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}

	return false
}

// parseUSBInfoFull extracts vendor, product and serial number from a by-id symlink name.
//
// Examples of by-id symlink names:
//
//	usb-FTDI_FT232R_USB_UART_AK08MDAM-if00-port0
//	  → vendor="FTDI", product="FT232R USB UART", serial="AK08MDAM"
//
//	usb-Silicon_Labs_CP2102_USB_to_UART_Bridge_Controller_0001-if00-port0
//	  → vendor="Silicon Labs", product="CP2102 USB to UART Bridge Controller", serial="0001"
//
//	usb-1a86_USB_Serial-if00-port0
//	  → vendor="1a86", product="USB Serial", serial=""
//
// The kernel encodes spaces as underscores and appends "-if<N>-port<N>" at the end.
// The serial number (if present) is the last underscore-separated token before the "-if" suffix.
func parseUSBInfoFull(symlinkName string) (vendor, product, serial string) {
	// Remove "usb-" prefix
	if !strings.HasPrefix(symlinkName, "usb-") {
		return "Unknown", symlinkName, ""
	}

	info := strings.TrimPrefix(symlinkName, "usb-")

	// Strip "-if<N>-port<N>" suffix
	if idx := strings.Index(info, "-if"); idx != -1 {
		info = info[:idx]
	}

	// Split by underscore
	parts := strings.Split(info, "_")
	if len(parts) < 2 {
		return "Unknown", info, ""
	}

	// The first token is always the vendor
	vendor = parts[0]

	// Heuristic: detect whether the last token looks like a serial number.
	// A serial number token is typically all uppercase hex / alphanumeric, no spaces.
	// We check: last part is non-empty and contains no lowercase letters (typical for
	// serial numbers like "AK08MDAM", "0001", "A123456"). If it looks like a plain
	// word that is part of the product name (e.g. "Controller", "Bridge") we keep it
	// in the product.
	lastPart := parts[len(parts)-1]
	looksLikeSerial := len(lastPart) >= 4 && isSerialLike(lastPart)

	var productParts []string
	if looksLikeSerial && len(parts) > 2 {
		serial = lastPart
		productParts = parts[1 : len(parts)-1]
	} else {
		productParts = parts[1:]
	}

	product = strings.Join(productParts, " ")
	return vendor, product, serial
}

// isSerialLike returns true if the string looks like a USB serial number
// (all uppercase alphanumeric, no purely lower-case dictionary words).
func isSerialLike(s string) bool {
	hasUpper := false
	hasLower := false
	for _, c := range s {
		if c >= 'A' && c <= 'Z' {
			hasUpper = true
		} else if c >= 'a' && c <= 'z' {
			hasLower = true
		}
	}
	// Serial numbers are typically all-uppercase or all-digits.
	// If there are lowercase letters, it's more likely a product word.
	if hasLower && !hasUpper {
		return false
	}
	return true
}

// parseUSBInfo is kept for backward compatibility; it returns vendor and product only.
func parseUSBInfo(symlinkName string) (vendor, product string) {
	v, p, _ := parseUSBInfoFull(symlinkName)
	return v, p
}

// GetPortInfo retrieves detailed information about a specific port
func GetPortInfo(devicePath string) (*PortInfo, error) {
	// Check if device exists
	if _, err := os.Stat(devicePath); os.IsNotExist(err) {
		return nil, fmt.Errorf("device %s does not exist", devicePath)
	}

	portID := filepath.Base(devicePath)

	// Try to find corresponding symlink in /dev/serial/by-id/
	byIDPorts, _ := scanByID()
	for _, port := range byIDPorts {
		if port.DevicePath == devicePath {
			return &port, nil
		}
	}

	// If not found in by-id, return basic info
	return &PortInfo{
		ID:         portID,
		DevicePath: devicePath,
		Vendor:     "Unknown",
		Product:    "Serial Device",
	}, nil
}
