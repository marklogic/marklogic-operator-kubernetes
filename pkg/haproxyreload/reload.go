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
	return r.clearRecovery()
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
	file, err := os.OpenFile(r.path("reload.lock"), os.O_CREATE|os.O_RDWR, 0660)
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
