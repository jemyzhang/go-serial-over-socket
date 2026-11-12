//go:build !linux

package main

// Desktop entries are a freedesktop (Linux) concept; elsewhere the
// install prompt does not exist.
func (a *nativeApp) maybePromptDesktopInstall() {}
