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
