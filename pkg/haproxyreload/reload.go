// Copyright (c) 2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

// Package haproxyreload manages a validated, writable configuration independently
// of the projected ConfigMap and communicates with HAProxy's local master CLI.
package haproxyreload

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	RuntimeDir = "/var/run/marklogic-haproxy"
	Executable = RuntimeDir + "/reloader"
	Source     = "/usr/local/etc/haproxy/haproxy.cfg"
	Timeout    = 30 * time.Second
)

type Status struct {
	Hash      string `json:"hash"`
	WorkerPID int    `json:"workerPID"`
}

type Reloader struct {
	Dir    string
	Binary string
}

func Hash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (r Reloader) path(name string) string { return filepath.Join(r.Dir, name) }

func (r Reloader) Args() []string {
	return []string{"-W", "-db", "-S", r.path("master.sock") + ",mode,600,level,admin", "-f", r.path("active.cfg")}
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
	return nil
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
		return fmt.Errorf("HAProxy validation failed: %w: %s", err, output)
	}
	return nil
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

func (r Reloader) Worker(ctx context.Context) (int, error) {
	output, err := r.cli(ctx, "show proc")
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "# old workers") {
			break
		}
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == "worker" {
			pid, err := strconv.Atoi(fields[0])
			if err == nil && pid > 0 {
				return pid, nil
			}
		}
	}
	return 0, fmt.Errorf("HAProxy master reports no current worker: %s", output)
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

// WithLock serializes exec requests, including recovery after an interrupted request.
func (r Reloader) WithLock(ctx context.Context, fn func() error) error {
	file, err := os.OpenFile(r.path("reload.lock"), os.O_CREATE|os.O_RDWR, 0600)
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
// request was interrupted between replacing active.cfg and recording success.
func (r Reloader) Recover(ctx context.Context) error {
	committed, err := os.ReadFile(r.path("committed.cfg"))
	if err != nil {
		return err
	}
	active, err := os.ReadFile(r.path("active.cfg"))
	if err != nil {
		return err
	}
	if bytes.Equal(active, committed) {
		return nil
	}
	if err := r.write("active.cfg", committed); err != nil {
		return err
	}
	if err := r.reload(ctx); err != nil {
		// Leave the mismatch visible so a later request retries recovery.
		return errors.Join(err, r.write("active.cfg", active))
	}
	return nil
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
	if restoreErr := r.write("active.cfg", previous); restoreErr != nil {
		return errors.Join(err, fmt.Errorf("restoring active file: %w", restoreErr))
	}
	if restoreErr := r.reload(rollbackCtx); restoreErr != nil {
		// Preserve the journal mismatch until recovery confirms a running worker.
		journalErr := r.write("active.cfg", data)
		return errors.Join(err, fmt.Errorf("rollback reload failed: %w", restoreErr), journalErr)
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
