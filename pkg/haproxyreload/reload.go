// Copyright (c) 2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

// Package haproxyreload manages a validated, writable configuration independently
// of the projected ConfigMap and communicates with HAProxy's local master CLI.
package haproxyreload

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	// RuntimeDir is shared and writable by both HAProxy and the reloader.
	RuntimeDir = "/var/run/marklogic-haproxy"
	// BinDir holds the reloader executable. Only the init container can write it;
	// the long-running containers mount it read-only so that a compromised HAProxy
	// cannot replace the code that runs in the token-bearing sidecar.
	BinDir     = "/opt/marklogic-haproxy-reloader"
	Executable = BinDir + "/reloader"
	// HealthDir is mounted only into the sidecar and holds its liveness heartbeat.
	HealthDir = "/var/run/marklogic-haproxy-health"
	Source    = "/usr/local/etc/haproxy/haproxy.cfg"
	Timeout   = 30 * time.Second
)

type Status struct {
	Hash      string `json:"hash"`
	WorkerPID int    `json:"workerPID"`
}

type Reloader struct {
	Dir    string
	Binary string
	Group  string
}

func Hash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (r Reloader) path(name string) string { return filepath.Join(r.Dir, name) }

func (r Reloader) Args() []string {
	group := r.Group
	if group == "" {
		group = strconv.Itoa(os.Getgid())
	}
	return []string{"-W", "-db", "-S", r.path("master.sock") + ",gid," + group + ",mode,660,level,admin", "-f", r.path("active.cfg")}
}

// Initialize is used only on container startup, before HAProxy starts.
func (r Reloader) Initialize(ctx context.Context, data []byte) error {
	if err := r.Validate(ctx, data); err != nil {
		return err
	}
	for _, name := range []string{"active.cfg", "committed.cfg"} {
		if err := r.write(name, data); err != nil {
			return err
		}
	}
	// A new boot ID tells the sidecar that HAProxy restarted, which a reload (even a
	// failed one) must never look like.
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return err
	}
	if err := r.write(bootIDFile, []byte(hex.EncodeToString(id))); err != nil {
		return err
	}
	return r.clearRecovery()
}

const bootIDFile = "boot.id"

// BootWatcher reports when the HAProxy container has restarted since it was last asked.
type BootWatcher struct{ last string }

// Restarted is true once per restart. An unreadable or missing ID (for example before
// HAProxy has initialized) is not a restart.
func (b *BootWatcher) Restarted(r Reloader) bool {
	data, err := os.ReadFile(r.path(bootIDFile))
	if err != nil || len(data) == 0 {
		return false
	}
	id := string(data)
	restarted := b.last != "" && id != b.last
	b.last = id
	return restarted
}

func (r Reloader) Validate(ctx context.Context, data []byte) error {
	file, err := os.CreateTemp(r.Dir, "candidate-*.cfg")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	output, err := exec.CommandContext(ctx, r.Binary, "-c", "-f", file.Name()).CombinedOutput()
	if err != nil {
		return fmt.Errorf("HAProxy validation failed: %w: %s", err, summarizeHAProxyOutput(output, file.Name()))
	}
	return nil
}

var haproxyLogPrefix = regexp.MustCompile(`^\[(\w+)\]\s+\(\d+\) : `)

// summarizeHAProxyOutput keeps the diagnostic lines of `haproxy -c` and removes what
// differs on every run (process ID, temporary file name), so repeated failures
// produce identical messages instead of a status update per retry.
func summarizeHAProxyOutput(output []byte, candidatePath string) string {
	var kept []string
	for _, line := range strings.Split(strings.ReplaceAll(string(output), candidatePath, "candidate configuration"), "\n") {
		line = strings.TrimSpace(haproxyLogPrefix.ReplaceAllString(strings.TrimSpace(line), "[$1] "))
		if line != "" && !strings.HasPrefix(line, "[NOTICE]") {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

func (r Reloader) write(name string, data []byte) error {
	file, err := os.CreateTemp(r.Dir, ".config-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err = file.Chmod(0660); err != nil {
		file.Close()
		return err
	}
	if err = file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), r.path(name))
}

func (r Reloader) cli(ctx context.Context, command string) (string, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", r.path("master.sock"))
	if err != nil {
		return "", err
	}
	defer conn.Close()
	deadline := time.Now().Add(Timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return "", err
	}
	if _, err := io.WriteString(conn, command+"\n"); err != nil {
		return "", err
	}
	if unix, ok := conn.(*net.UnixConn); ok {
		if err := unix.CloseWrite(); err != nil {
			return "", err
		}
	}
	output, err := io.ReadAll(io.LimitReader(conn, 1024*1024))
	if err != nil {
		return "", err
	}
	return string(output), nil
}

// parseProcesses reads `show proc`: the current worker's PID, and how many workers of
// earlier configurations are still draining connections.
func parseProcesses(output string) (current, old int) {
	section := ""
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "#") {
			section = strings.TrimSpace(strings.TrimPrefix(line, "#"))
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[1] != "worker" {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil || pid <= 0 {
			continue
		}
		switch section {
		case "", "workers": // a worker listed before any header is the current one
			if current == 0 {
				current = pid
			}
		case "old workers":
			old++
		}
	}
	return current, old
}

// Processes returns the current worker and the number of old workers still draining.
func (r Reloader) Processes(ctx context.Context) (current, old int, err error) {
	output, err := r.cli(ctx, "show proc")
	if err != nil {
		return 0, 0, err
	}
	current, old = parseProcesses(output)
	if current == 0 {
		return 0, old, fmt.Errorf("HAProxy master reports no current worker: %s", output)
	}
	return current, old, nil
}

func (r Reloader) Worker(ctx context.Context) (int, error) {
	pid, _, err := r.Processes(ctx)
	return pid, err
}

func (r Reloader) reload(ctx context.Context) error {
	output, err := r.cli(ctx, "reload")
	if err != nil {
		return fmt.Errorf("master CLI reload: %w", err)
	}
	if !strings.HasPrefix(strings.TrimSpace(output), "Success=1\n") && strings.TrimSpace(output) != "Success=1" {
		return fmt.Errorf("HAProxy rejected reload: %s", output)
	}
	// After a rejected activation HAProxy may list all surviving workers under
	// "old workers". A rollback must still be allowed to create a current worker.
	_, err = r.Worker(ctx)
	return err
}

// WithLock serializes startup, sidecar and CLI operations, including recovery
// after an interrupted request.
func (r Reloader) WithLock(ctx context.Context, fn func() error) error {
	// flock needs only a readable descriptor, so containers sharing the
	// runtime group can lock a file created by the other container.
	file, err := os.OpenFile(r.path("reload.lock"), os.O_CREATE|os.O_RDONLY, 0660)
	if err != nil {
		return err
	}
	defer file.Close()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	defer syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	return fn()
}

// Recover conservatively restores the last confirmed configuration if an exec
// request was interrupted during activation or restoration of active.cfg.
func (r Reloader) Recover(ctx context.Context) error {
	committed, err := os.ReadFile(r.path("committed.cfg"))
	if err != nil {
		return err
	}
	active, err := os.ReadFile(r.path("active.cfg"))
	if err != nil {
		return err
	}
	_, pendingErr := os.Stat(r.path("recovery.pending"))
	if pendingErr != nil && !errors.Is(pendingErr, os.ErrNotExist) {
		return pendingErr
	}
	if bytes.Equal(active, committed) && errors.Is(pendingErr, os.ErrNotExist) {
		return nil
	}
	return r.restore(ctx, committed)
}

func (r Reloader) clearRecovery() error {
	err := os.Remove(r.path("recovery.pending"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (r Reloader) restore(ctx context.Context, committed []byte) error {
	// File equality cannot prove that the running worker has been restored.
	// Journal the pending reload before replacing active.cfg, including on retries.
	if err := r.write("recovery.pending", nil); err != nil {
		return err
	}
	if err := r.write("active.cfg", committed); err != nil {
		return err
	}
	if err := r.reload(ctx); err != nil {
		return err
	}
	return r.clearRecovery()
}

// Apply validates again immediately before swapping the active file. A successful
// CLI response and a new current worker are both required before committing.
func (r Reloader) Apply(ctx context.Context, data []byte) error {
	if err := r.Recover(ctx); err != nil {
		return fmt.Errorf("recovering interrupted reload: %w", err)
	}
	if err := r.Validate(ctx, data); err != nil {
		return err
	}
	previous, err := os.ReadFile(r.path("committed.cfg"))
	if err != nil {
		return err
	}
	if bytes.Equal(previous, data) {
		_, err := r.Worker(ctx)
		return err
	}
	if err := r.write("active.cfg", data); err != nil {
		return err
	}
	err = r.reload(ctx)
	if err == nil {
		err = r.write("committed.cfg", data)
	}
	if err == nil {
		return nil
	}
	rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), Timeout)
	defer cancel()
	if restoreErr := r.restore(rollbackCtx, previous); restoreErr != nil {
		return errors.Join(err, fmt.Errorf("rollback failed: %w", restoreErr))
	}
	return err
}

func (r Reloader) Status(ctx context.Context) (Status, error) {
	if err := r.Recover(ctx); err != nil {
		return Status{}, err
	}
	pid, err := r.Worker(ctx)
	if err != nil {
		return Status{}, err
	}
	data, err := os.ReadFile(r.path("committed.cfg"))
	if err != nil {
		return Status{}, err
	}
	return Status{Hash: Hash(data), WorkerPID: pid}, nil
}
