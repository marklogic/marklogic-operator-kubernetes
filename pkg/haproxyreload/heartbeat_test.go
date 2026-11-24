// Copyright (c) 2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package haproxyreload

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestHeartbeatIsAliveOnlyWhileTheWatcherMakesProgress(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	if err := CheckHeartbeat(dir, HeartbeatMaxAge, now); err == nil {
		t.Fatal("a watcher that never started must fail the liveness probe")
	}
	if err := WriteHeartbeat(dir, now); err != nil {
		t.Fatal(err)
	}
	if err := CheckHeartbeat(dir, HeartbeatMaxAge, now.Add(time.Minute)); err != nil {
		t.Fatalf("recent heartbeat rejected: %v", err)
	}
	if err := CheckHeartbeat(dir, HeartbeatMaxAge, now.Add(HeartbeatMaxAge+time.Minute)); err == nil {
		t.Fatal("a stalled watcher must fail the liveness probe")
	}
}

func TestHeartbeatToleratesTheLongestSingleWatcherIteration(t *testing.T) {
	// Recovery (2*Timeout) + validation + activation (2*Timeout+5s) + worker check +
	// status patch, with generous margin for lock waits.
	longest := 2*Timeout + Timeout + 2*Timeout + 5*time.Second + Timeout + 10*time.Second
	if HeartbeatMaxAge <= longest {
		t.Fatalf("liveness threshold %s could restart the sidecar mid-reload (longest iteration %s)", HeartbeatMaxAge, longest)
	}
}

func TestHeartbeatFileIsPrivateToTheSidecar(t *testing.T) {
	dir := t.TempDir()
	if err := WriteHeartbeat(dir, time.Now()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, heartbeatFile))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("heartbeat must be owner-only: %v %v", info, err)
	}
}
