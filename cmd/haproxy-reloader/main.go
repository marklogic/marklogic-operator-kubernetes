// Copyright (c) 2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/marklogic/marklogic-operator-kubernetes/pkg/haproxyreload"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) != 2 {
		return errors.New("expected install, start, watch, validate, apply, status, or ready")
	}
	r := haproxyreload.Reloader{
		Dir: haproxyreload.RuntimeDir, Binary: "haproxy", Group: os.Getenv("HAPROXY_MASTER_GROUP"),
	}
	if os.Args[1] == "watch" {
		ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
		defer cancel()
		return haproxyreload.Watch(ctx, r)
	}
	ctx, cancel := context.WithTimeout(context.Background(), haproxyreload.Timeout)
	defer cancel()
	switch os.Args[1] {
	case "install":
		path, err := os.Executable()
		if err != nil {
			return err
		}
		source, err := os.Open(path)
		if err != nil {
			return err
		}
		defer source.Close()
		file, err := os.CreateTemp(r.Dir, ".reloader-*")
		if err != nil {
			return err
		}
		defer os.Remove(file.Name())
		if _, err := io.Copy(file, source); err != nil {
			file.Close()
			return err
		}
		if err := file.Chmod(0555); err != nil {
			file.Close()
			return err
		}
		if err := file.Close(); err != nil {
			return err
		}
		return os.Rename(file.Name(), haproxyreload.Executable)
	case "start":
		data, err := os.ReadFile(haproxyreload.Source)
		if err != nil {
			return err
		}
		if err := r.WithLock(ctx, func() error { return r.Initialize(ctx, data) }); err != nil {
			return err
		}
		binary, err := exec.LookPath(r.Binary)
		if err != nil {
			return err
		}
		return syscall.Exec(binary, append([]string{binary}, r.Args()...), os.Environ())
	case "ready":
		_, err := r.Worker(ctx)
		return err
	case "validate", "apply":
		data, err := io.ReadAll(io.LimitReader(os.Stdin, 1024*1024+1))
		if err != nil {
			return err
		}
		if len(data) > 1024*1024 {
			return errors.New("configuration exceeds 1 MiB")
		}
		return r.WithLock(ctx, func() error {
			if os.Args[1] == "validate" {
				return r.Validate(ctx, data)
			}
			return r.Apply(ctx, data)
		})
	case "status":
		return r.WithLock(ctx, func() error {
			status, err := r.Status(ctx)
			if err != nil {
				return err
			}
			return json.NewEncoder(os.Stdout).Encode(status)
		})
	default:
		return fmt.Errorf("unknown operation %q", os.Args[1])
	}
}
