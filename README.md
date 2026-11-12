# Serial Over Socket (Go Version)

A high-performance Serial-over-WebSocket server and client written in Go. This project allows you to access and control multiple serial ports remotely through a modern web interface or a dedicated TUI terminal client, with full synchronized command history and optional WebDAV cloud backup.

## Security

**This server has no authentication.** Anyone who can reach it can list, open and close serial ports, write arbitrary bytes to them, and read all serial output. It is designed for a **trusted local network only**: never expose it directly to the internet, and if you do not need remote access, bind it to localhost with `sos-server -address 127.0.0.1`. See [SECURITY.md](SECURITY.md) for details and how to report a vulnerability.

## Key Features

### Server (`sos-server`)
- **Unified Communication**: All data and control signals flow through a single WebSocket port (default: 2025).
- **USB Serial Auto-Discovery**: Automatically detects and manages USB serial devices on startup.
- **Hotplug Support**: Real-time detection and broadcasting of USB device plug/unplug events.
- **Centralized Command History**: Server is the single source of truth; all clients stay in sync via `history_update` broadcasts.
- **Persistent Storage**: Config and history saved to `~/.config/go-serial-over-socket/` (XDG-compliant).
- **WebDAV Auto-Backup**: Interval-based differential backup of command history to any WebDAV-compatible server (Nextcloud, Seafile, etc.). Only backs up when history actually changes.
- **Embedded Web Interface**: Standalone binary with all web resources (HTML/JS/CSS) embedded.

### Web Interface
- **xterm.js Terminal**: Full-featured terminal with mouse support, search, and font size controls.
- **Command History Manager**: View, use, edit, and delete individual history entries (with unique IDs).
- **WebDAV Sync Panel**: Configure WebDAV credentials, backup interval, and max backup count via the UI. Settings are persisted on the server.
- **Import/Export**: Import history from local JSON files or remote WebDAV backups; export current history.
- **Auto-clipboard**: Selecting text in the terminal automatically copies it to clipboard (with fallback for non-HTTPS contexts).
- **Real-time Sync**: All changes (edits, deletes, imports) are immediately broadcast to all connected TUI and web clients.

### Terminal Client (`sos-client`)
- **Interactive TUI**: Keyboard-driven port selection, baud rate, data bits, stop bits, and parity configuration.
- **Raw Mode**: Character-by-character transmission — no Enter required for interactive shells.
- **In-Session Shortcuts** (while connected to a port):

  | Shortcut | Action |
  |----------|--------|
  | `Ctrl+T` | Open port/settings palette |
  | `Ctrl+V` | Open multi-line command input editor |
  | `Ctrl+F` | Set output filter (regex) |
  | `Ctrl+L` | Start/stop logging to file |
  | `Ctrl+\` | Toggle per-line timestamp (ms precision, highlighted) |

- **Multi-line Command Editor** (`Ctrl+V`):

  | Key | Action |
  |-----|--------|
  | `Tab` | Switch between **Input** box and **History** list |
  | `Alt+Enter` | Send command immediately |
  | `Ctrl+P` / `Ctrl+N` | Navigate filtered history suggestions |
  | `↑`/`↓` or `k`/`j` | Move through history list (previews full command in input box) |
  | `Enter` | Confirm selection from list → back to input |
  | `Esc` | Cancel and close |

- **Smart History Filtering**: Typing in the input box dynamically filters history suggestions in real time.
- **Live Preview**: When browsing the history list, the full command is previewed in the input box so you can see long commands at a glance.
- **Output Filter**: Press `Ctrl+F` to set a regex pattern — only matching lines are displayed. Press again to clear.
- **Log to File**: Press `Ctrl+L` to start logging all output to a file; supports `Tab`-completion for paths. Press again to stop.
- **Timestamps**: Press `Ctrl+\` to toggle per-line timestamps (`HH:MM:SS.mmm`) with a colored background highlight.
- **Server Sync**: Command history is pulled from and pushed to the server on connect, keeping TUI and WebUI always in sync.

## Architecture

```
┌─────────────────┐    ┌────────────────────────────┐    ┌─────────────────┐
│   Web Browser   │◄──►│                            │◄──►│ Serial Manager  │
│  (Port 2025/ws) │    │     Go Server (Port 2025)  │    │     (UDEV)      │
└─────────────────┘    │                            │    └─────────────────┘
                       │  • Command History (SSOT)  │
┌─────────────────┐    │  • WebDAV Auto-Backup      │
│   sos-client    │◄──►│  • Config Persistence      │
│  (Port 2025/ws) │    │  • WebSocket Broadcast     │
└─────────────────┘    └────────────────────────────┘
                                    ▲
                                    │ (optional)
                              ┌─────┴─────┐
                              │  WebDAV   │
                              │  Server   │
                              └───────────┘
```

## Installation

### Prerequisites
- Go 1.21 or higher
- Linux system (for USB auto-discovery via udev)

### Build
```bash
git clone https://github.com/jemyzhang/go-serial-over-socket.git
cd go-serial-over-socket

# Build server and client
go build -o sos-server ./cmd/server
go build -o sos-client ./cmd/client
```

## Usage

### 1. Start the Server
```bash
# Standard start (listens on 0.0.0.0:2025)
./sos-server

# Custom address and port
./sos-server -address 127.0.0.1 -port 8080
```

Config and history are auto-saved to `~/.config/go-serial-over-socket/`.

### 2. Connect with Terminal Client
```bash
# Connect to server (interactive port selection)
./sos-client -server 127.0.0.1:2025
```

### 3. Access the Web Interface
Open your browser and navigate to: `http://localhost:2025`

- Select a port and click **Connect**
- Use the **History** panel to manage saved commands
- Use the **Sync** tab to configure WebDAV backup

## WebDAV Backup

Configure in the web interface under the **Sync** tab:

| Setting | Description |
|---------|-------------|
| URL | WebDAV server URL (e.g. `https://cloud.example.com/remote.php/dav/files/user/sos/`) |
| Username | WebDAV username |
| Password | WebDAV password |
| Interval (s) | Auto-backup interval in seconds |
| Max Backups | Maximum number of remote backup files to keep |

Backup files are named `history_backup_YYYYMMDDHHMMSS.mmm.json`. On server startup, if no local history exists, the latest WebDAV backup is automatically loaded.

## CLI Options

### Server
```
-address string   Bind address (default "0.0.0.0")
-port int         Bind port (default 2025)
```

### Client
```
-server string    Server address host:port (default "127.0.0.1:8080")
```

## Changelog

### v4.0 (April 2026)
- **Server-Side SSOT**: Server is now the single source of truth for command history and configuration.
- **WebDAV Auto-Backup**: Differential, interval-based backups from the server. Only uploads when history changes.
- **TUI Overhaul**:
  - Multi-line command editor with Tab-switchable input/history list.
  - Live history preview in input box while browsing list.
  - Real-time regex output filter (`Ctrl+F`).
  - Log to file with Tab-completion (`Ctrl+L`).
  - Per-line millisecond timestamp toggle (`Ctrl+\`).
- **WebUI Improvements**:
  - Full CRUD for command history entries (add, edit, delete, use).
  - Clipboard copy with `execCommand` fallback for non-HTTPS contexts.
  - All changes broadcast in real-time to all connected clients.

### v3.0 (February 2026)
- Architecture simplification: unified all communication over WebSocket on a single port.
- Removed `config.json` requirement in favor of persistent server-side config.
- Added Raw Mode for character-level interaction in the terminal client.

## License

MIT License — see [LICENSE](LICENSE) file for details.

## Acknowledgments

- Original SerialOverSocket C++ project by jemyzhang.
- Go community for `gorilla/websocket` and `tarm/serial` libraries.
- [Charm](https://charm.sh/) for BubbleTea, Lipgloss, and Bubbles.
