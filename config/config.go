package config

import (
	"encoding/json"
	"os"
	"path/filepath"
)

type ServerConfig struct {
	Address string `json:"address"`
	Port    int    `json:"port"`
}

type SerialPortConfig struct {
	Device   string `json:"device"`
	Baudrate int    `json:"baudrate"`
	Databits int    `json:"databits"`
	Stopbit  int    `json:"stopbit"`
	Parity   string `json:"parity"`
	// Timeout is the serial read timeout in milliseconds. Values <= 0
	// fall back to the built-in default (200ms).
	Timeout int `json:"timeout"`
	// WriteChunkSize / WriteChunkDelayMs pace large writes so a slow
	// embedded UART isn't overrun. Values <= 0 use defaults (16 bytes / 30ms).
	WriteChunkSize    int `json:"write_chunk_size,omitempty"`
	WriteChunkDelayMs int `json:"write_chunk_delay_ms,omitempty"`
}

type WebDAVConfig struct {
	URL        string `json:"url"`
	Username   string `json:"username"`
	Password   string `json:"password"`
	Interval   int    `json:"interval"`
	MaxBackups int    `json:"max_backups"`
}

// PortProfile stores serial port settings keyed by the stable device identifier
// (preferably the /dev/serial/by-id/ symlink name, e.g.
// "usb-FTDI_FT232R_USB_UART_AK08MDAM-if00-port0").
// Using this hardware identifier rather than the device node (ttyUSB0) makes
// the profile portable across reboots where the device number may change.
type PortProfile struct {
	Baudrate int    `json:"baudrate"`
	Databits int    `json:"databits"`
	Stopbit  int    `json:"stopbit"`
	Parity   string `json:"parity"`
	Alias    string `json:"alias,omitempty"` // user-defined display name for this port
}

type Config struct {
	Server       ServerConfig            `json:"server"`
	SerialPort   SerialPortConfig        `json:"serialport"`
	WebDAV       WebDAVConfig            `json:"webdav"`
	PortProfiles map[string]*PortProfile `json:"port_profiles,omitempty"` // keyed by device description
}

func GetConfigPath() string {
	// 1. If currently existing in working directory, use it
	if _, err := os.Stat("config.json"); err == nil {
		return "config.json"
	}

	// 2. Otherwise default to XDG config home
	if home, err := os.UserHomeDir(); err == nil {
		configDir := filepath.Join(home, ".config", "go-serial-over-socket")
		os.MkdirAll(configDir, 0755)
		return filepath.Join(configDir, "config.json")
	}

	// 3. Fallback
	return "config.json"
}

func LoadConfig(path string) (*Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var config Config
	decoder := json.NewDecoder(file)
	err = decoder.Decode(&config)
	if err != nil {
		return nil, err
	}

	return &config, nil
}

func SaveConfig(path string, config *Config) error {
	// Write to a temp file and rename so a crash or a concurrent writer
	// can never leave a truncated config behind.
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// Best-effort cleanup on failure; a successful rename removes it anyway.
	defer func() {
		tmp.Close()
		os.Remove(tmpName)
	}()

	encoder := json.NewEncoder(tmp)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(config); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// 0600: config.json holds the WebDAV password in plain text.
	if err := os.Chmod(tmpName, 0600); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func DefaultConfig() *Config {
	return &Config{
		Server: ServerConfig{
			Address: "0.0.0.0",
			Port:    2025,
		},
		SerialPort: SerialPortConfig{
			Device:   "/dev/ttyUSB0",
			Baudrate: 115200,
			Databits: 8,
			Stopbit:  1,
			Parity:   "N",
			Timeout:  200, // milliseconds
		},
	}
}
