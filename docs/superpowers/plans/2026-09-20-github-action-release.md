# GitHub Action Automated Release & Versioning Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Configure GitHub Actions for automatic release generation on push to `main` with Conventional Commits semantic versioning, and implement runtime Go ldflags version injection across `sos-server`, `sos-client`, and the WebUI.

**Architecture:** A unified Go package `pkg/version` holds build metadata (Version, GitCommit, BuildDate) injected via `-ldflags` during compilation. `sos-server` serves this version to the WebUI template, and `sos-client` exposes it via a `-v` flag. A GitHub Action workflow runs on pushes to `main`, detects Conventional Commits to calculate the next semantic tag (starting from `v1.0.0`), compiles Linux amd64 binaries with `libudev-dev`, packages `sos-linux-amd64.tar.gz` and `checksums.txt`, and creates a GitHub Release.

**Tech Stack:** Go (1.24+ / standard library), GitHub Actions, `mathieudutour/github-tag-action` or Conventional Commits tagger, `softprops/action-gh-release`, systemd.

## Global Constraints
- Target platform: Linux amd64
- Server compilation requires CGO and `libudev-dev`
- ExecStart path in `sos.service`: `/usr/bin/sos-server`
- Do NOT use the author's personal username (`jemyzhang`) anywhere in `sos.service`, configs, code comments, or commit messages. Use generic service account or commented `# User=sos` / `# User=root`.
- Baseline version: `v1.0.0`
- Conventional Commits: `feat` bumps minor, `fix` bumps patch, `BREAKING CHANGE` bumps major.

---

### Task 1: Create `pkg/version` Package with Tests

**Files:**
- Create: `pkg/version/version.go`
- Create: `pkg/version/version_test.go`

**Interfaces:**
- Produces:
  - `var Version string`
  - `var GitCommit string`
  - `var BuildDate string`
  - `func Get() string`
  - `func Info() string`
  - `func Print(appName string)`

- [ ] **Step 1: Write the failing unit test**
Create `pkg/version/version_test.go`:
```go
package version

import (
	"strings"
	"testing"
)

func TestVersionInfo(t *testing.T) {
	Version = "v1.2.3"
	GitCommit = "abcdef1"
	BuildDate = "2026-09-20T10:00:00Z"

	info := Info()
	if !strings.Contains(info, "v1.2.3") {
		t.Errorf("expected info to contain version v1.2.3, got %s", info)
	}
	if !strings.Contains(info, "abcdef1") {
		t.Errorf("expected info to contain commit abcdef1, got %s", info)
	}
	if !strings.Contains(info, "2026-09-20T10:00:00Z") {
		t.Errorf("expected info to contain build date 2026-09-20T10:00:00Z, got %s", info)
	}

	if Get() != "v1.2.3" {
		t.Errorf("expected Get() to return v1.2.3, got %s", Get())
	}
}
```

- [ ] **Step 2: Run test to verify it fails**
Run: `go test ./pkg/version/...`
Expected: FAIL with undefined identifiers.

- [ ] **Step 3: Implement minimal code**
Create `pkg/version/version.go`:
```go
package version

import (
	"fmt"
)

var (
	// Version holds the current semantic version, injected at build time via -ldflags.
	Version = "v1.0.0"
	// GitCommit holds the git commit hash, injected at build time via -ldflags.
	GitCommit = "none"
	// BuildDate holds the build timestamp (UTC), injected at build time via -ldflags.
	BuildDate = "unknown"
)

// Get returns the current version string.
func Get() string {
	return Version
}

// Info returns formatted multi-line version details.
func Info() string {
	return fmt.Sprintf("Version: %s\nCommit: %s\nBuild Date: %s", Version, GitCommit, BuildDate)
}

// Print prints a standard version banner for the given application.
func Print(appName string) {
	fmt.Printf("%s\n", appName)
	fmt.Printf("Version:    %s\n", Version)
	fmt.Printf("Git Commit: %s\n", GitCommit)
	fmt.Printf("Build Date: %s\n", BuildDate)
}
```

- [ ] **Step 4: Run test to verify it passes**
Run: `go test -v ./pkg/version/...`
Expected: PASS

- [ ] **Step 5: Commit**
```bash
git add pkg/version
git commit -m "feat(version): introduce pkg/version with build metadata injection support"
```

---

### Task 2: Integrate `pkg/version` into Server and WebUI

**Files:**
- Modify: `cmd/server/main.go`
- Modify: `internal/web/web_server.go`
- Modify: `web/templates/index.html`

**Interfaces:**
- Consumes: `pkg/version.Version`, `pkg/version.Print`
- Produces: Dynamic version display in `sos-server -v` and in WebUI footer `<span class="footer-version">{{.Version}}</span>`.

- [ ] **Step 1: Update `cmd/server/main.go`**
Import `github.com/jemyzhang/serial-over-socket-go/pkg/version`.
Replace local `const version = "1.0.0"` and update `printVersion()`:
```go
func printVersion() {
	version.Print("Serial Over Socket Server (Go Version)")
}
```

- [ ] **Step 2: Pass version into WebUI template in `internal/web/web_server.go`**
Import `github.com/jemyzhang/serial-over-socket-go/pkg/version`.
In `handleIndex(w http.ResponseWriter, r *http.Request)`:
```go
	data := map[string]interface{}{
		"Title":   "Serial Over Socket - Web Interface",
		"Ports":   ws.serialMgr.ListPorts(),
		"Version": version.Get(),
	}
```

- [ ] **Step 3: Update `web/templates/index.html`**
In `web/templates/index.html` line 1363:
Replace `<span class="footer-version">v1.0.0</span>` with:
```html
<span class="footer-version">{{.Version}}</span>
```

- [ ] **Step 4: Verify build and runtime version**
Run:
```bash
go build -ldflags "-X github.com/jemyzhang/serial-over-socket-go/pkg/version.Version=v1.9.9" -o /tmp/sos-server-test ./cmd/server
/tmp/sos-server-test -v
```
Expected output includes:
`Version: v1.9.9`

- [ ] **Step 5: Commit**
```bash
git add cmd/server/main.go internal/web/web_server.go web/templates/index.html
git commit -m "feat(server): dynamic version display in server CLI and WebUI footer"
```

---

### Task 3: Integrate `pkg/version` into Client

**Files:**
- Modify: `cmd/client/main.go`

**Interfaces:**
- Consumes: `pkg/version.Print`
- Produces: CLI flag `-v` / `--version` for `sos-client`.

- [ ] **Step 1: Add version flag to `cmd/client/main.go`**
Import `github.com/jemyzhang/serial-over-socket-go/pkg/version`.
Add flag in global state or `main()`:
```go
showVersion := flag.Bool("v", false, "Show version info")
```
Parse flags before launching TUI:
```go
if *showVersion {
    version.Print("Serial Over Socket Client (Go Version)")
    os.Exit(0)
}
```

- [ ] **Step 2: Verify build and runtime version**
Run:
```bash
go build -ldflags "-X github.com/jemyzhang/serial-over-socket-go/pkg/version.Version=v1.9.9" -o /tmp/sos-client-test ./cmd/client
/tmp/sos-client-test -v
```
Expected output includes:
`Version: v1.9.9`

- [ ] **Step 3: Commit**
```bash
git add cmd/client/main.go
git commit -m "feat(client): add -v version flag to sos-client CLI"
```

---

### Task 4: Standardize `sos.service`

**Files:**
- Modify: `sos.service`

**Interfaces:**
- Produces: Systemd unit file pointing to `/usr/bin/sos-server` with environment and user guidance.

- [ ] **Step 1: Update `sos.service`**
Write clean systemd service:
```ini
[Unit]
Description=Serial Over Socket Server
Documentation=https://github.com/jemyzhang/serial-over-socket-go
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
# Optional environment file: uncomment and create /etc/default/sos if needed
# EnvironmentFile=-/etc/default/sos
# Default user. Note: user needs read/write permissions for serial devices (e.g. dialout group)
# User=sos
ExecStart=/usr/bin/sos-server $SOS_OPTS
Restart=always
RestartSec=5

# Sandboxing & security hardening (optional, adjust if needed)
# ProtectSystem=full
# ProtectHome=read-only

[Install]
WantedBy=multi-user.target
```

- [ ] **Step 2: Commit**
```bash
git add sos.service
git commit -m "chore(systemd): standardize sos.service with /usr/bin/sos-server and environment guidance"
```

---

### Task 5: Configure GitHub Actions Release Workflow

**Files:**
- Create: `.github/workflows/release.yml`

**Interfaces:**
- Produces: Automated release trigger on push to `main`, Conventional Commits semantic version tag, Linux amd64 compilation, tar.gz packaging, checksums, and GitHub Release.

- [ ] **Step 1: Create `.github/workflows/release.yml`**
```yaml
name: Release

on:
  push:
    branches:
      - main

permissions:
  contents: write

jobs:
  release:
    name: Build and Release
    runs-on: ubuntu-latest
    steps:
      - name: Checkout Repository
        uses: actions/checkout@v4
        with:
          fetch-depth: 0

      - name: Determine Next Semantic Version
        id: tag_version
        uses: mathieudutour/github-tag-action@v6.2
        with:
          github_token: ${{ secrets.GITHUB_TOKEN }}
          default_bump: patch
          initial_version: 1.0.0
          tag_prefix: v

      - name: Setup Go
        uses: actions/setup-go@v5
        with:
          go-version: 'stable'
          cache: true

      - name: Install Linux Build Dependencies
        run: |
          sudo apt-get update
          sudo apt-get install -y libudev-dev

      - name: Build Binaries
        env:
          NEW_TAG: ${{ steps.tag_version.outputs.new_tag }}
        run: |
          VERSION="${NEW_TAG:-v1.0.0}"
          COMMIT="${{ github.sha }}"
          BUILD_DATE=$(date -u +%Y-%m-%dT%H:%M:%SZ)
          PKG="github.com/jemyzhang/serial-over-socket-go/pkg/version"
          LDFLAGS="-s -w -X ${PKG}.Version=${VERSION} -X ${PKG}.GitCommit=${COMMIT} -X ${PKG}.BuildDate=${BUILD_DATE}"

          echo "Building sos-server with version ${VERSION}..."
          go build -ldflags "${LDFLAGS}" -o dist/sos-server ./cmd/server

          echo "Building sos-client with version ${VERSION}..."
          go build -ldflags "${LDFLAGS}" -o dist/sos-client ./cmd/client

      - name: Package Release Assets
        run: |
          mkdir -p package/sos
          cp dist/sos-server package/sos/
          cp dist/sos-client package/sos/
          cp sos.service package/sos/
          cp README.md package/sos/
          [ -f README_cn.md ] && cp README_cn.md package/sos/ || true

          cd package
          tar -czvf ../sos-linux-amd64.tar.gz sos/
          cd ..

          sha256sum sos-linux-amd64.tar.gz > checksums.txt

      - name: Publish GitHub Release
        uses: softprops/action-gh-release@v2
        with:
          tag_name: ${{ steps.tag_version.outputs.new_tag }}
          name: Release ${{ steps.tag_version.outputs.new_tag }}
          body: ${{ steps.tag_version.outputs.changelog }}
          files: |
            sos-linux-amd64.tar.gz
            checksums.txt
```

- [ ] **Step 2: Verify workflow YAML syntax**
Run local linter / yaml validation check.

- [ ] **Step 3: Commit**
```bash
git add .github/workflows/release.yml
git commit -m "ci: add automated release workflow on push to main"
```

---

### Task 6: End-to-End Verification

- [ ] **Step 1: Run all unit tests**
Run: `go test -v ./...`
Expected: All tests pass.

- [ ] **Step 2: Run test builds with ldflags**
Run:
```bash
go build -ldflags "-X github.com/jemyzhang/serial-over-socket-go/pkg/version.Version=v1.0.0-test" -o bin/sos-server ./cmd/server
go build -ldflags "-X github.com/jemyzhang/serial-over-socket-go/pkg/version.Version=v1.0.0-test" -o bin/sos-client ./cmd/client
./bin/sos-server -v
./bin/sos-client -v
```
Expected: Both binaries print `v1.0.0-test`.

- [ ] **Step 3: Verify git status and clean up temporary test binaries**
Run: `rm -rf bin/ dist/ package/`
