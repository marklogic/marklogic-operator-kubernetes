// Copyright (c) 2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package haproxyreload

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// HeartbeatMaxAge must exceed the longest single watcher iteration (recovery,
// validation, activation and status reporting, each individually bounded) so
// the liveness probe never restarts the sidecar in the middle of a reload.
const HeartbeatMaxAge = 10 * time.Minute

const heartbeatFile = "heartbeat"

// WriteHeartbeat records that the watcher loop is still making progress.
func WriteHeartbeat(dir string, now time.Time) error {
	return os.WriteFile(filepath.Join(dir, heartbeatFile), []byte(now.UTC().Format(time.RFC3339)), 0600)
}

// CheckHeartbeat fails when the watcher has not recorded progress recently.
func CheckHeartbeat(dir string, maxAge time.Duration, now time.Time) error {
	info, err := os.Stat(filepath.Join(dir, heartbeatFile))
	if err != nil {
		return fmt.Errorf("reloader heartbeat: %w", err)
	}
	if age := now.Sub(info.ModTime()); age > maxAge {
		return fmt.Errorf("reloader heartbeat is stale: last progress %s ago", age.Round(time.Second))
	}
	return nil
}
