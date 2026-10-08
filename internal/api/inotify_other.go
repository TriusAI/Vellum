//go:build !linux

package api

// newFSWatcher reports that this platform has no filesystem-notification
// support, so the watcher falls back to periodic polling.
func newFSWatcher(dirs []string) fsWatcher { return nil }
