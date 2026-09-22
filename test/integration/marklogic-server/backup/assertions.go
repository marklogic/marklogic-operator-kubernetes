// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package backup

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/textproto"
	"strings"

	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
)

type forest struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	BackupPath string `json:"backupPath"`
}
type jobStatus struct {
	Status  string   `json:"status"`
	Forests []forest `json:"forests"`
}

// Digest negotiation may produce multiple header blocks. Use the final response
// only, and require exactly one JSON result from our queries.
func decodeEval(headers, body string, result any) error {
	normalized := strings.ReplaceAll(headers, "\r\n", "\n")
	blocks := strings.Split(strings.TrimSpace(normalized), "\n\n")
	last := blocks[len(blocks)-1]
	status, rest, ok := strings.Cut(last, "\n")
	fields := strings.Fields(status)
	if !ok || len(fields) < 2 || !strings.HasPrefix(fields[0], "HTTP/") || fields[1] != "200" {
		return fmt.Errorf("eval did not return HTTP 200")
	}
	h, err := textproto.NewReader(bufio.NewReader(strings.NewReader(rest + "\n\n"))).ReadMIMEHeader()
	if err != nil {
		return fmt.Errorf("invalid eval headers")
	}
	contentType, params, err := mime.ParseMediaType(h.Get("Content-Type"))
	if err != nil || contentType != "multipart/mixed" || params["boundary"] == "" {
		return fmt.Errorf("eval response is not multipart/mixed")
	}
	reader := multipart.NewReader(strings.NewReader(body), params["boundary"])
	part, err := reader.NextPart()
	if err != nil {
		return fmt.Errorf("eval returned no result")
	}
	data, err := io.ReadAll(io.LimitReader(part, 1024*1024+1))
	if err != nil || len(data) > 1024*1024 {
		return fmt.Errorf("invalid or oversized eval result")
	}
	if _, err := reader.NextPart(); err != io.EOF {
		return fmt.Errorf("eval must return exactly one result")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(result); err != nil {
		return fmt.Errorf("eval returned invalid result JSON (response omitted)")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("eval returned trailing data")
	}
	return nil
}

func jobComplete(s jobStatus, expected []forest, backup bool, runURI string) (bool, string, error) {
	switch s.Status {
	case "queued", "in-progress", "finishing":
		return false, "", nil
	case "completed":
	default:
		return false, "", fmt.Errorf("backup/restore job did not succeed (status %q)", s.Status)
	}
	if len(expected) == 0 || len(s.Forests) != len(expected) {
		return false, "", fmt.Errorf("job has missing or unexpected forests")
	}
	remaining := map[string]bool{}
	for _, f := range expected {
		if f.ID == "" || remaining[f.ID] {
			return false, "", fmt.Errorf("expected topology contains missing or duplicate forest IDs")
		}
		remaining[f.ID] = true
	}
	path := ""
	for _, f := range s.Forests {
		if !remaining[f.ID] || f.Status != "completed" {
			return false, "", fmt.Errorf("forest status is incomplete, failed, or does not match this run")
		}
		delete(remaining, f.ID)
		if backup {
			if !backupPathWithinRun(f.BackupPath, runURI) {
				return false, "", fmt.Errorf("backup path escapes this run's S3 prefix")
			}
			if path != "" && path != f.BackupPath {
				return false, "", fmt.Errorf("backup forests have different paths; this scenario requires one full backup")
			}
			path = f.BackupPath
		}
	}
	return true, path, nil
}

func credentialsApplied(cluster marklogicv1.MarklogicCluster, c config) (bool, error) {
	if cluster.Spec.ObjectStorage == nil || cluster.Spec.ObjectStorage.AWS == nil {
		return false, fmt.Errorf("installed CRD pruned spec.objectStorage; install the object-storage CRD and operator before this scenario")
	}
	if cluster.Spec.ObjectStorage.AWS.SecretName != credentialSecret {
		return false, fmt.Errorf("cluster does not reference this run's AWS Secret")
	}
	if cluster.UID == "" {
		return false, fmt.Errorf("cluster has no UID for credential fingerprint verification")
	}
	status := cluster.Status.ObjectStorage
	if status == nil || status.AWS == nil {
		return false, nil
	}
	if status.AWS.Phase == marklogicv1.ObjectStoragePhaseFailed {
		return false, fmt.Errorf("operator could not apply AWS credentials; inspect status.objectStorage.aws.reason (raw status omitted)")
	}
	return status.AWS.Phase == marklogicv1.ObjectStoragePhaseApplied && status.AWS.AppliedFingerprint == c.AWS.Fingerprint(string(cluster.UID)), nil
}
