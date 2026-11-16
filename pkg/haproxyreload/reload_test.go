// Copyright (c) 2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package haproxyreload

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func testProxy(t *testing.T) (Reloader, []byte, string, string) {
	t.Helper()
	binary, err := exec.LookPath("haproxy")
	if err != nil {
		t.Skip("integration test requires the haproxy binary")
	}
	// Keep Unix socket paths below the platform limit, including on macOS.
	dir, err := os.MkdirTemp("/tmp", "hpr-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { backend.Close() })
	go func() {
		for {
			conn, err := backend.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				io.Copy(conn, conn)
			}()
		}
	}()
	address := func() string {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := listener.Addr().String()
		listener.Close()
		return addr
	}
	httpAddr, tcpAddr := address(), address()
	data := []byte(fmt.Sprintf(`global
  maxconn 1024
  stats socket %s/worker.sock mode 600 level admin expose-fd listeners
defaults
  timeout connect 2s
  timeout client 60s
  timeout server 60s
frontend http
  mode http
  bind %s
  http-request return status 200 content-type text/plain string version-one
listen tcp
  mode tcp
  bind %s
  server echo %s
`, dir, httpAddr, tcpAddr, backend.Addr()))
	r := Reloader{Dir: dir, Binary: binary}
	if err := r.Initialize(context.Background(), data); err != nil {
		t.Fatal(err)
	}
	logFile, err := os.Create(filepath.Join(dir, "haproxy.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { logFile.Close() })
	command := exec.Command(binary, r.Args()...)
	command.Stdout, command.Stderr = logFile, logFile
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	t.Cleanup(func() {
		command.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			command.Process.Kill()
			<-done
		}
		if t.Failed() {
			logs, err := os.ReadFile(logFile.Name())
			if err == nil {
				t.Logf("HAProxy logs:\n%s", logs)
			}
		}
	})
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := r.Worker(context.Background()); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("HAProxy worker did not become ready")
		}
		time.Sleep(20 * time.Millisecond)
	}
	return r, data, httpAddr, tcpAddr
}

func TestHAProxyHitlessReloadAndRollback(t *testing.T) {
	r, original, httpAddr, tcpAddr := testProxy(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := net.DialTimeout("tcp", tcpAddr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	reader := bufio.NewReader(conn)
	echo := func() {
		t.Helper()
		if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(conn, "connection-preserved\n"); err != nil {
			t.Fatal(err)
		}
		line, err := reader.ReadString('\n')
		if err != nil || line != "connection-preserved\n" {
			t.Fatalf("active TCP connection interrupted: %q, %v", line, err)
		}
	}
	echo()
	client := &http.Client{Timeout: 2 * time.Second}
	defer client.CloseIdleConnections()
	var traffic sync.WaitGroup
	stop := make(chan struct{})
	failures := make(chan error, 1)
	traffic.Add(1)
	go func() {
		defer traffic.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			response, err := client.Get("http://" + httpAddr)
			if err == nil {
				_, err = io.Copy(io.Discard, response.Body)
				response.Body.Close()
				if response.StatusCode != http.StatusOK {
					err = fmt.Errorf("unexpected HTTP status %d", response.StatusCode)
				}
			}
			if err != nil {
				select {
				case failures <- err:
				default:
				}
			}
		}
	}()
	defer func() {
		close(stop)
		traffic.Wait()
		select {
		case err := <-failures:
			t.Errorf("HTTP traffic interrupted during reload: %v", err)
		default:
		}
	}()
	before, err := r.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	desired := bytes.ReplaceAll(original, []byte("version-one"), []byte("version-two"))
	if err := r.Apply(ctx, desired); err != nil {
		t.Fatal(err)
	}
	after, err := r.Status(ctx)
	if err != nil || after.Hash != Hash(desired) || before.WorkerPID == after.WorkerPID {
		t.Fatalf("reload did not activate candidate: before=%+v after=%+v err=%v", before, after, err)
	}
	echo()
	response, err := client.Get("http://" + httpAddr)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || string(body) != "version-two" {
		t.Fatalf("new requests did not use new configuration: %q, %v", body, err)
	}
	if err := r.Apply(ctx, desired); err != nil {
		t.Fatal(err)
	}
	unchanged, err := r.Status(ctx)
	if err != nil || unchanged != after {
		t.Fatalf("unchanged configuration triggered a reload: %+v, %v", unchanged, err)
	}
	if err := r.Apply(ctx, []byte("this is not valid HAProxy syntax\n")); err == nil {
		t.Fatal("invalid configuration was accepted")
	}
	rejected, err := r.Status(ctx)
	if err != nil || rejected != after {
		t.Fatalf("invalid syntax changed active configuration: %+v, %v", rejected, err)
	}
	echo()
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	unbindable := bytes.ReplaceAll(desired, []byte("bind "+httpAddr), []byte("bind "+occupied.Addr().String()))
	if err := r.Validate(ctx, unbindable); err != nil {
		t.Fatalf("test must exercise activation failure after valid syntax: %v", err)
	}
	if err := r.Apply(ctx, unbindable); err == nil {
		t.Fatal("configuration with an unavailable listener was accepted")
	}
	rolledBack, err := r.Status(ctx)
	if err != nil || rolledBack.Hash != Hash(desired) {
		t.Fatalf("activation failure did not roll back: %+v, %v", rolledBack, err)
	}
	active, err := os.ReadFile(r.path("active.cfg"))
	if err != nil || !bytes.Equal(active, desired) {
		t.Fatalf("rollback did not restore active file: %v", err)
	}
	echo()
	if err := r.Apply(ctx, original); err != nil {
		t.Fatalf("rolling back a successfully activated replica failed: %v", err)
	}
	echo()
}

func TestHAProxyInterruptedApplyRecovery(t *testing.T) {
	r, original, _, _ := testProxy(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	candidate := bytes.ReplaceAll(original, []byte("version-one"), []byte("uncommitted"))
	for _, activate := range []bool{false, true} {
		if err := r.write("active.cfg", candidate); err != nil {
			t.Fatal(err)
		}
		if activate {
			if err := r.reload(ctx); err != nil {
				t.Fatal(err)
			}
		}
		status, err := r.Status(ctx)
		if err != nil || status.Hash != Hash(original) {
			t.Fatalf("interrupted apply was not recovered (activated=%v): %+v %v", activate, status, err)
		}
		active, err := os.ReadFile(r.path("active.cfg"))
		if err != nil || !bytes.Equal(active, original) {
			t.Fatalf("active configuration was not restored: %v", err)
		}
	}
}

func TestWithLockHonorsContext(t *testing.T) {
	r := Reloader{Dir: t.TempDir()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := r.WithLock(ctx, func() error {
		cancel()
		return r.WithLock(ctx, func() error { t.Error("concurrent lock acquired"); return nil })
	})
	if err == nil || !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("lock did not honor cancellation: %v", err)
	}
}
