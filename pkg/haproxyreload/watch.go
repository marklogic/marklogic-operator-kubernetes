// Copyright (c) 2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package haproxyreload

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	CandidateDir = "/etc/marklogic-haproxy-candidate"
	APITokenDir  = "/var/run/marklogic-haproxy-api"
	StatusMap    = "marklogic-haproxy-reload-status"
)

type candidate struct {
	phase       string
	transaction string
	hash        string
	config      []byte
	previous    []byte
}

type PodStatus struct {
	Transaction string `json:"transaction"`
	Phase       string `json:"phase"`
	Hash        string `json:"hash"`
	WorkerPID   int    `json:"workerPID,omitempty"`
	Message     string `json:"message,omitempty"`
}

func readCandidate(dir string) (candidate, error) {
	revision, err := filepath.EvalSymlinks(filepath.Join(dir, "..data"))
	if err != nil {
		return candidate{}, err
	}
	read := func(name string) ([]byte, error) {
		return os.ReadFile(filepath.Join(revision, name))
	}
	phase, err := read("phase")
	if err != nil {
		return candidate{}, err
	}
	transaction, err := read("transaction")
	if err != nil {
		return candidate{}, err
	}
	hash, err := read("hash")
	if err != nil {
		return candidate{}, err
	}
	config, err := read("haproxy.cfg")
	if err != nil {
		return candidate{}, err
	}
	previous, err := read("previous.cfg")
	if err != nil {
		return candidate{}, err
	}
	c := candidate{
		phase:       strings.TrimSpace(string(phase)),
		transaction: strings.TrimSpace(string(transaction)),
		hash:        strings.TrimSpace(string(hash)),
		config:      config,
		previous:    previous,
	}
	if c.transaction == "" || c.hash == "" || Hash(c.config) != c.hash {
		return candidate{}, errors.New("candidate configuration metadata is inconsistent")
	}
	return c, nil
}

func Watch(ctx context.Context, r Reloader) error {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	namespace, pod := os.Getenv("POD_NAMESPACE"), os.Getenv("POD_NAME")
	if host == "" || port == "" || namespace == "" || pod == "" {
		return errors.New("Kubernetes service address and pod identity are required")
	}
	ca, err := os.ReadFile(filepath.Join(APITokenDir, "ca.crt"))
	if err != nil {
		return fmt.Errorf("reading Kubernetes API CA: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return errors.New("Kubernetes API CA bundle contains no certificates")
	}
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
	}}
	statusURL := "https://" + netJoinHostPort(host, port) + "/api/v1/namespaces/" +
		url.PathEscape(namespace) + "/configmaps/" + StatusMap
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	lastReported := ""
	lastReadError := ""
	lastReportError := ""
	retryPhase := ""
	retryTransaction := ""
	var pendingReport *PodStatus
	pendingFingerprint := ""
	lastWorkerPID := currentWorker(ctx, r)
	for {
		if workerPID := currentWorker(ctx, r); workerPID != 0 {
			if lastWorkerPID != 0 && workerPID != lastWorkerPID {
				retryPhase = ""
				lastReported = ""
				pendingReport = nil
				pendingFingerprint = ""
			}
			lastWorkerPID = workerPID
		}
		candidate, err := readCandidate(CandidateDir)
		if err != nil {
			if err.Error() != lastReadError {
				fmt.Fprintf(os.Stderr, "haproxy-reloader: reading candidate: %v\n", err)
				lastReadError = err.Error()
			}
		} else {
			lastReadError = ""
			phaseKey := candidate.transaction + ":" + candidate.phase
			if phaseKey == retryPhase {
				if pendingReport != nil {
					if err := patchPodStatus(ctx, client, statusURL, filepath.Join(APITokenDir, "token"), pod, *pendingReport); err != nil {
						if err.Error() != lastReportError {
							fmt.Fprintf(os.Stderr, "haproxy-reloader: reporting reload status: %v\n", err)
							lastReportError = err.Error()
						}
					} else {
						lastReported = pendingFingerprint
						pendingReport = nil
						pendingFingerprint = ""
						lastReportError = ""
					}
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-ticker.C:
				}
				continue
			}
			if pendingReport != nil {
				pendingReport = nil
				pendingFingerprint = ""
			}
			report, done := processCandidate(ctx, r, candidate)
			reportFailed := false
			if report != nil {
				fingerprint, _ := json.Marshal(report)
				if string(fingerprint) != lastReported {
					if err := patchPodStatus(ctx, client, statusURL, filepath.Join(APITokenDir, "token"), pod, *report); err != nil {
						if err.Error() != lastReportError {
							fmt.Fprintf(os.Stderr, "haproxy-reloader: reporting reload status: %v\n", err)
							lastReportError = err.Error()
						}
						reportFailed = true
						if report.Phase != "rollback-failed" {
							copy := *report
							pendingReport = &copy
							pendingFingerprint = string(fingerprint)
						}
					} else {
						lastReported = string(fingerprint)
						lastReportError = ""
					}
				}
			}
			if done {
				lastReported = ""
				retryPhase = ""
				retryTransaction = ""
			} else if report != nil && report.Phase != "rollback-failed" && !reportFailed {
				retryPhase = phaseKey
				retryTransaction = candidate.transaction
			} else if reportFailed && pendingReport != nil {
				retryPhase = phaseKey
				retryTransaction = candidate.transaction
			} else if reportFailed {
				retryPhase = ""
			} else if candidate.transaction != retryTransaction {
				retryPhase = ""
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func netJoinHostPort(host, port string) string {
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		host = "[" + host + "]"
	}
	return host + ":" + port
}

func processCandidate(ctx context.Context, r Reloader, c candidate) (*PodStatus, bool) {
	status := &PodStatus{Transaction: c.transaction, Hash: c.hash}
	switch c.phase {
	case "prepare":
		status.Phase = "prepared"
		opCtx, cancel := context.WithTimeout(ctx, Timeout)
		err := r.Validate(opCtx, c.config)
		cancel()
		if err != nil {
			status.Phase, status.Message = "failed", err.Error()
		}
		return status, false
	case "commit":
		status.Phase = "applied"
		opCtx, cancel := context.WithTimeout(ctx, Timeout)
		err := r.Validate(opCtx, c.config)
		cancel()
		if err != nil {
			status.Phase, status.Message = "failed", err.Error()
			return status, false
		}
		opCtx, cancel = context.WithTimeout(ctx, 2*Timeout+5*time.Second)
		err = r.Apply(opCtx, c.config)
		cancel()
		if err != nil {
			status.Phase, status.Message = "failed", err.Error()
			return status, false
		}
		opCtx, cancel = context.WithTimeout(ctx, Timeout)
		pid, err := r.Worker(opCtx)
		cancel()
		if err != nil {
			status.Phase, status.Message = "failed", err.Error()
			return status, false
		}
		status.WorkerPID = pid
		return status, false
	case "rollback":
		status.Phase = "rolledback"
		opCtx, cancel := context.WithTimeout(ctx, 2*Timeout+5*time.Second)
		err := r.Apply(opCtx, c.previous)
		cancel()
		if err != nil {
			status.Phase, status.Message = "rollback-failed", err.Error()
			return status, false
		}
		status.Hash = Hash(c.previous)
		opCtx, cancel = context.WithTimeout(ctx, Timeout)
		pid, err := r.Worker(opCtx)
		cancel()
		if err != nil {
			status.Phase, status.Message = "rollback-failed", err.Error()
			return status, false
		}
		status.WorkerPID = pid
		return status, false
	case "abort":
		status.Phase = "aborted"
		return status, true
	default:
		return nil, false
	}
}

func currentWorker(ctx context.Context, r Reloader) int {
	workerCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	pid, err := r.Worker(workerCtx)
	if err != nil {
		return 0
	}
	return pid
}

func patchPodStatus(ctx context.Context, client *http.Client, endpoint, tokenPath, pod string, status PodStatus) error {
	token, err := os.ReadFile(tokenPath)
	if err != nil {
		return fmt.Errorf("reading projected Kubernetes token: %w", err)
	}
	value, err := json.Marshal(status)
	if err != nil {
		return err
	}
	patch, err := json.Marshal(struct {
		Data map[string]string `json:"data"`
	}{Data: map[string]string{pod: string(value)}})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPatch, endpoint, bytes.NewReader(patch))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	request.Header.Set("Content-Type", "application/merge-patch+json")
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return fmt.Errorf("Kubernetes API returned %s: %s", response.Status, body)
	}
	return nil
}
