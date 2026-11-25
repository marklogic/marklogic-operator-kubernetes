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

func testDir(t *testing.T) string {
	t.Helper()
	// Keep Unix socket paths below the platform limit, including on macOS.
	dir, err := os.MkdirTemp("/tmp", "hpr-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

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

func TestHAProxyInterruptedRestorationRecovery(t *testing.T) {
	r, original, httpAddr, _ := testProxy(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	candidate := bytes.ReplaceAll(original, []byte("version-one"), []byte("uncommitted"))
	for _, statusRequest := range []bool{false, true} {
		if err := r.write("active.cfg", candidate); err != nil {
			t.Fatal(err)
		}
		if err := r.reload(ctx); err != nil {
			t.Fatal(err)
		}
		candidatePID, err := r.Worker(ctx)
		if err != nil {
			t.Fatal(err)
		}
		// Recovery and rollback share this interruption point: the files match,
		// but the candidate worker is still serving requests.
		if err := r.write("recovery.pending", nil); err != nil {
			t.Fatal(err)
		}
		if err := r.write("active.cfg", original); err != nil {
			t.Fatal(err)
		}
		if statusRequest {
			status, err := r.Status(ctx)
			if err != nil || status.Hash != Hash(original) {
				t.Fatalf("status did not recover pending restoration: %+v, %v", status, err)
			}
		} else if err := r.Recover(ctx); err != nil {
			t.Fatal(err)
		}
		pid, err := r.Worker(ctx)
		if err != nil || pid == candidatePID {
			t.Fatalf("pending restoration did not replace candidate worker: %d, %v", pid, err)
		}
		client := &http.Client{Timeout: 2 * time.Second}
		response, err := client.Get("http://" + httpAddr)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		client.CloseIdleConnections()
		if err != nil || string(body) != "version-one" {
			t.Fatalf("candidate configuration still serving: %q, %v", body, err)
		}
		if _, err := os.Stat(r.path("recovery.pending")); !os.IsNotExist(err) {
			t.Fatalf("confirmed restoration retained pending marker: %v", err)
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

func TestInterruptedRestorationRetainsPendingReload(t *testing.T) {
	for _, rollback := range []bool{false, true} {
		t.Run(fmt.Sprintf("rollback=%v", rollback), func(t *testing.T) {
			r := Reloader{Dir: testDir(t), Binary: "true"}
			original, candidate := []byte("committed"), []byte("candidate")
			if err := r.Initialize(context.Background(), original); err != nil {
				t.Fatal(err)
			}
			if !rollback {
				if err := r.write("active.cfg", candidate); err != nil {
					t.Fatal(err)
				}
			}
			listener, err := net.Listen("unix", r.path("master.sock"))
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			type exchange struct {
				command, response string
				restoring         bool
			}
			exchanges := []exchange{
				{"reload", "interrupted", true},
				{"reload", "retry failed", true},
				{"reload", "Success=1\n", true},
				{"show proc", "123 worker\n", true},
				{"show proc", "123 worker\n", false},
			}
			if rollback {
				exchanges = append([]exchange{{"reload", "activation failed", false}}, exchanges...)
			}
			done := make(chan error, 1)
			go func() {
				for _, step := range exchanges {
					conn, err := listener.Accept()
					if err != nil {
						done <- err
						return
					}
					command, err := io.ReadAll(conn)
					if err != nil {
						conn.Close()
						done <- err
						return
					}
					if strings.TrimSpace(string(command)) != step.command {
						t.Errorf("command = %q, want %q", command, step.command)
					}
					if step.restoring {
						// Observe the interruption window, before reload is confirmed.
						if _, err := os.Stat(r.path("recovery.pending")); err != nil {
							t.Errorf("restoration was not journaled: %v", err)
						}
						active, err := os.ReadFile(r.path("active.cfg"))
						if err != nil || !bytes.Equal(active, original) {
							t.Errorf("restored active file = %q, err=%v", active, err)
						}
					}
					_, err = io.WriteString(conn, step.response)
					conn.Close()
					if err != nil {
						done <- err
						return
					}
				}
				done <- nil
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if rollback {
				err = r.Apply(ctx, candidate)
			} else {
				err = r.Recover(ctx)
			}
			if err == nil {
				t.Fatal("interrupted restoration succeeded")
			}
			// A fresh request must retry even though the files are already equal.
			retry := Reloader{Dir: r.Dir, Binary: r.Binary}
			if err := retry.Recover(ctx); err == nil {
				t.Fatal("failed retry succeeded")
			}
			if _, err := os.Stat(r.path("recovery.pending")); err != nil {
				t.Fatalf("failed retry lost pending marker: %v", err)
			}
			status, err := retry.Status(ctx)
			if err != nil || status.Hash != Hash(original) || status.WorkerPID != 123 {
				t.Fatalf("status did not finish restoration: %+v, %v", status, err)
			}
			if _, err := os.Stat(r.path("recovery.pending")); !os.IsNotExist(err) {
				t.Fatalf("confirmed restoration retained pending marker: %v", err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		})
	}
}

func TestInitializeClearsPendingOnlyAfterSuccess(t *testing.T) {
	for _, failure := range []string{"", "validation", "committed-write"} {
		t.Run("failure="+failure, func(t *testing.T) {
			r := Reloader{Dir: testDir(t), Binary: "true"}
			if err := r.write("recovery.pending", nil); err != nil {
				t.Fatal(err)
			}
			if failure == "validation" {
				r.Binary = "false"
			}
			if failure == "committed-write" {
				if err := os.Mkdir(r.path("committed.cfg"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			err := r.Initialize(context.Background(), []byte("startup configuration"))
			if (err != nil) != (failure != "") {
				t.Fatalf("Initialize error = %v, failure = %q", err, failure)
			}
			_, err = os.Stat(r.path("recovery.pending"))
			if failure == "" && !os.IsNotExist(err) {
				t.Fatalf("successful startup retained stale marker: %v", err)
			}
			if failure != "" && err != nil {
				t.Fatalf("failed startup lost pending marker: %v", err)
			}
		})
	}
}

func TestValidationFailureMessageIsStableAcrossRetries(t *testing.T) {
	script := filepath.Join(t.TempDir(), "fake-haproxy")
	body := "#!/bin/sh\n" +
		"echo \"[NOTICE]   ($$) : haproxy version is 3.4.7\"\n" +
		"echo \"[ALERT]    ($$) : config : parsing [$3:3] : unknown keyword 'bogus' in 'frontend' section\"\n" +
		"echo \"[ALERT]    ($$) : config : Fatal errors found in configuration.\"\n" +
		"exit 1\n"
	if err := os.WriteFile(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	r := Reloader{Dir: t.TempDir(), Binary: script}
	var messages []string
	for i := 0; i < 2; i++ {
		err := r.Validate(context.Background(), []byte("frontend f\n"))
		if err == nil {
			t.Fatal("invalid configuration was accepted")
		}
		messages = append(messages, err.Error())
	}
	if messages[0] != messages[1] {
		t.Fatalf("message must not vary between attempts:\n%q\n%q", messages[0], messages[1])
	}
	for _, volatile := range []string{"NOTICE", "candidate-", r.Dir} {
		if strings.Contains(messages[0], volatile) {
			t.Fatalf("message contains run-specific text %q: %s", volatile, messages[0])
		}
	}
	if !strings.Contains(messages[0], "unknown keyword 'bogus'") || !strings.Contains(messages[0], "candidate configuration:3") {
		t.Fatalf("diagnostics were lost: %s", messages[0])
	}
}

func waitForOldWorkers(t *testing.T, r Reloader, want int, within time.Duration) time.Duration {
	t.Helper()
	start := time.Now()
	for {
		_, old, err := r.Processes(context.Background())
		if err == nil && old == want {
			return time.Since(start)
		}
		if time.Since(start) > within {
			t.Fatalf("expected %d old workers within %s, have %d (err %v)", want, within, old, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestHardStopAfterBoundsHowLongAnOldWorkerDrains(t *testing.T) {
	r, original, _, tcpAddr := testProxy(t)
	ctx := context.Background()
	echoes := func(conn net.Conn) bool {
		_ = conn.SetDeadline(time.Now().Add(time.Second))
		if _, err := io.WriteString(conn, "ping\n"); err != nil {
			return false
		}
		line, err := bufio.NewReader(conn).ReadString('\n')
		return err == nil && line == "ping\n"
	}

	// Without a bound, the old worker stays for as long as a connection is open ...
	held, err := net.Dial("tcp", tcpAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if !echoes(held) {
		t.Fatal("test setup: connection does not work")
	}
	if err := r.Apply(ctx, bytes.ReplaceAll(original, []byte("version-one"), []byte("version-two"))); err != nil {
		t.Fatal(err)
	}
	waitForOldWorkers(t, r, 1, 3*time.Second)
	time.Sleep(4 * time.Second)
	if _, old, _ := r.Processes(ctx); old != 1 || !echoes(held) {
		t.Fatalf("without hard-stop-after the old worker must keep serving its open connection (old=%d)", old)
	}
	// ... and drains once the connection ends, which is what the counter must show.
	held.Close()
	waitForOldWorkers(t, r, 0, 5*time.Second)

	// With a bound, it is removed on schedule even though a connection is still open.
	bounded := bytes.Replace(original, []byte("global\n"), []byte("global\n  hard-stop-after 3s\n"), 1)
	if err := r.Apply(ctx, bounded); err != nil {
		t.Fatal(err)
	}
	waitForOldWorkers(t, r, 0, 5*time.Second) // the previous config had no bound: nothing draining
	stuck, err := net.Dial("tcp", tcpAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer stuck.Close()
	if !echoes(stuck) {
		t.Fatal("test setup: connection to the bounded configuration does not work")
	}
	if err := r.Apply(ctx, bytes.ReplaceAll(bounded, []byte("version-one"), []byte("version-three"))); err != nil {
		t.Fatal(err)
	}
	waitForOldWorkers(t, r, 1, 3*time.Second)
	took := waitForOldWorkers(t, r, 0, 8*time.Second)
	if took > 6*time.Second {
		t.Fatalf("the old worker outlived hard-stop-after 3s by too much: %s", took)
	}
	if echoes(stuck) {
		t.Fatal("hard-stop-after must end connections still open when the bound expires")
	}
}
