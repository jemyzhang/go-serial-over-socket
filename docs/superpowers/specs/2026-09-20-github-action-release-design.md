# GitHub Action Automated Release & Versioning Design

## Overview
Implement automated GitHub Release creation on every push to the `main` branch based on Conventional Commits, along with a unified Go version injection mechanism across `sos-server`, `sos-client`, and the WebUI.

## 1. Release Workflow (`.github/workflows/release.yml`)

### Trigger
- `push` to `main` branch.

### Pipeline Steps
1. **Repository Checkout**:
   - `actions/checkout@v4` with `fetch-depth: 0` to enable full history inspection.
2. **Conventional Commits & Semantic Tag Calculation**:
   - Calculate semantic version bump:
     - `feat`: Minor bump
     - `fix`, `perf`, `refactor`: Patch bump
     - `BREAKING CHANGE` or `!`: Major bump
     - Baseline initial version: `v1.0.0` (if no existing git tags).
   - Generate release notes / changelog from commit messages.
3. **Environment Setup**:
   - Setup Go (`actions/setup-go@v5`) using Go 1.24/1.25 or `go-version-file: 'go.mod'`.
   - Install Linux build dependency: `sudo apt-get update && sudo apt-get install -y libudev-dev`.
4. **Compile Binaries (Linux amd64)**:
   - Build `sos-server` and `sos-client` with `-ldflags`:
     ```bash
     -s -w
     -X github.com/jemyzhang/serial-over-socket-go/pkg/version.Version=${TAG}
     -X github.com/jemyzhang/serial-over-socket-go/pkg/version.GitCommit=${GITHUB_SHA}
     -X github.com/jemyzhang/serial-over-socket-go/pkg/version.BuildDate=$(date -u +%Y-%m-%dT%H:%M:%SZ)
     ```
5. **Archive Packaging**:
   - Package `sos-linux-amd64.tar.gz` containing:
     - `sos-server`
     - `sos-client`
     - `sos.service`
     - `README.md`
     - `README_cn.md` (if present)
   - Compute SHA-256 checksum: `sha256sum sos-linux-amd64.tar.gz > checksums.txt`.
6. **Publish Release**:
   - Create Git tag and GitHub Release with generated notes.
   - Upload `sos-linux-amd64.tar.gz` and `checksums.txt` to the release.

## 2. Go Versioning (`pkg/version`)

### Package `pkg/version/version.go`
- `var Version = "v1.0.0"`
- `var GitCommit = "none"`
- `var BuildDate = "unknown"`
- Helper functions:
  - `Get() string` returns formatted version string.
  - `Print(appName string)` prints full banner, version, commit, and build date.

### Binary Integration
- **`cmd/server/main.go`**:
  - Replace hardcoded `const version = "1.0.0"` with `pkg/version.Version`.
  - `-v` flag prints version, commit, and build date via `version.Print("Serial Over Socket Server")`.
  - In `internal/web/web_server.go`, pass `version.Version` in template data `data["Version"] = version.Version`.
- **`web/templates/index.html`**:
  - Replace hardcoded `<span class="footer-version">v1.0.0</span>` with `<span class="footer-version">{{.Version}}</span>`.
- **`cmd/client/main.go`**:
  - Add `-v` / `-version` flag support that prints `version.Print("Serial Over Socket Client")` and exits.

## 3. Systemd Unit (`sos.service`)
- Standardized service definition:
  - Executable path: `/usr/bin/sos-server`
  - Allows configurable environment file (e.g. `/etc/default/sos` or `/etc/sos/config.env`)
  - Configurable execution arguments (e.g. `$SOS_OPTS`)
  - Default user guidance (e.g. running under user with `dialout` group or root).
