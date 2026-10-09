// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package mlmanage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const (
	// CredentialsPropertiesPath is the Management API endpoint used to set object storage credentials.
	CredentialsPropertiesPath = "/manage/v2/credentials/properties"
	// bootstrapProbePath is a read-only, authenticated endpoint used to confirm the Management API is serving.
	bootstrapProbePath = "/manage/v2/hosts"

	maxDiscardedResponseBytes = 64 * 1024
	maxProbeResponseBytes     = 1024 * 1024
)

// CredentialClient applies object storage credentials through the Management API.
// It deliberately has no read or delete operations.
type CredentialClient interface {
	// CheckBootstrapReady confirms the authenticated Management API is serving and the named
	// bootstrap host is listed and online. It fails closed on any response it cannot interpret.
	CheckBootstrapReady(ctx context.Context, bootstrapHost string) error
	ApplyAWSCredentials(ctx context.Context, creds AWSCredentials) error
	ApplyAzureCredentials(ctx context.Context, creds AzureCredentials) error
}

// AWSCredentials is trimmed AWS material. SessionToken is omitted from the payload when empty.
type AWSCredentials struct {
	AccessKey    string
	SecretKey    string
	SessionToken string
}

// AzureCredentials is trimmed Azure material.
type AzureCredentials struct {
	StorageAccount string
	StorageKey     string
}

// String and GoString keep credential material out of accidental formatting.
func (AWSCredentials) String() string     { return "AWSCredentials{<redacted>}" }
func (AWSCredentials) GoString() string   { return "AWSCredentials{<redacted>}" }
func (AzureCredentials) String() string   { return "AzureCredentials{<redacted>}" }
func (AzureCredentials) GoString() string { return "AzureCredentials{<redacted>}" }

func (c AWSCredentials) payload() map[string]string {
	payload := map[string]string{
		"type":       "aws",
		"access-key": c.AccessKey,
		"secret-key": c.SecretKey,
	}
	if c.SessionToken != "" {
		payload["session-token"] = c.SessionToken
	}
	return payload
}

func (c AzureCredentials) payload() map[string]string {
	return map[string]string{
		"type":            "azure",
		"storage-account": c.StorageAccount,
		"storage-key":     c.StorageKey,
	}
}

// CredentialError is a secret-safe failure: it never carries request or response content.
type CredentialError struct {
	// Operation is the HTTP method and path, for example "PUT /manage/v2/credentials/properties".
	Operation string
	// StatusCode is zero when no HTTP response was received.
	StatusCode int
	// ResponseIncomplete means the expected status arrived but reading or closing the body failed,
	// so the outcome is unconfirmed.
	ResponseIncomplete bool
}

func (e *CredentialError) Error() string {
	switch {
	case e.StatusCode == 0:
		return fmt.Sprintf("management api %s failed without an HTTP response", e.Operation)
	case e.ResponseIncomplete:
		return fmt.Sprintf("management api %s returned status %d but the response could not be read completely", e.Operation, e.StatusCode)
	default:
		return fmt.Sprintf("management api %s returned status %d", e.Operation, e.StatusCode)
	}
}

// BootstrapNotReadyError reports why the bootstrap host cannot accept credentials yet; it carries no response content.
type BootstrapNotReadyError struct {
	// Reason is a fixed, secret-safe description.
	Reason string
}

func (e *BootstrapNotReadyError) Error() string { return "bootstrap host is not ready: " + e.Reason }

const (
	bootstrapReasonUnparseable = "the host status response could not be interpreted"
	bootstrapReasonNotListed   = "the bootstrap host is not listed in the host status"
	bootstrapReasonOffline     = "the bootstrap host is not online"
)

// IsTransport reports whether no HTTP response was received.
func (e *CredentialError) IsTransport() bool { return e.StatusCode == 0 }

// NewCredentialClient returns a client sharing the Management API authentication and transport settings.
func NewCredentialClient(opts ClientOptions) CredentialClient {
	return &managementClient{
		baseURL:    buildBaseURL(opts.Host, opts.UseTLS),
		username:   opts.Username,
		password:   opts.Password,
		httpClient: buildHTTPClient(opts),
	}
}

func (c *managementClient) CheckBootstrapReady(ctx context.Context, bootstrapHost string) error {
	data, err := c.doSafe(ctx, http.MethodGet, bootstrapProbePath+"?view=status&format=json", nil, http.StatusOK, true)
	if err != nil {
		return err
	}
	return bootstrapHostReady(data, bootstrapHost)
}

// bootstrapHostReady requires the bootstrap host to be listed and online; unknown states are not ready.
func bootstrapHostReady(data []byte, bootstrapHost string) error {
	var payload any
	if err := json.Unmarshal(data, &payload); err != nil {
		return &BootstrapNotReadyError{Reason: bootstrapReasonUnparseable}
	}
	items := extractHostItems(payload)
	if len(items) == 0 {
		return &BootstrapNotReadyError{Reason: bootstrapReasonNotListed}
	}
	for _, item := range items {
		if !sameManagedHost(firstString(item, "nameref", "host-name", "name"), bootstrapHost) {
			continue
		}
		switch status := strings.ToLower(firstString(item, "status", "host-status")); {
		case status == "online":
			return nil
		case status != "":
			return &BootstrapNotReadyError{Reason: bootstrapReasonOffline}
		}
		// Without a per-host status, only an all-hosts-online summary proves the bootstrap host is online.
		if offline, ok := extractTotalHostsOffline(payload); ok && offline == 0 {
			return nil
		}
		return &BootstrapNotReadyError{Reason: bootstrapReasonOffline}
	}
	return &BootstrapNotReadyError{Reason: bootstrapReasonNotListed}
}

// sameManagedHost matches by full name or by the pod name before the first dot.
func sameManagedHost(reported, bootstrapHost string) bool {
	reported = strings.ToLower(strings.TrimSpace(reported))
	bootstrapHost = strings.ToLower(strings.TrimSpace(bootstrapHost))
	if reported == "" || bootstrapHost == "" {
		return false
	}
	if reported == bootstrapHost {
		return true
	}
	return strings.SplitN(reported, ".", 2)[0] == strings.SplitN(bootstrapHost, ".", 2)[0]
}

func (c *managementClient) ApplyAWSCredentials(ctx context.Context, creds AWSCredentials) error {
	return c.putCredentials(ctx, creds.payload())
}

func (c *managementClient) ApplyAzureCredentials(ctx context.Context, creds AzureCredentials) error {
	return c.putCredentials(ctx, creds.payload())
}

func (c *managementClient) putCredentials(ctx context.Context, payload map[string]string) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return &CredentialError{Operation: http.MethodPut + " " + CredentialsPropertiesPath}
	}
	_, err = c.doSafe(ctx, http.MethodPut, CredentialsPropertiesPath, body, http.StatusNoContent, false)
	return err
}

// doSafe performs an authenticated request. The body is returned only when keepBody is set (bounded),
// otherwise it is drained and discarded. Failures never include request or response content, and an
// expected status whose body cannot be read or closed is reported as unconfirmed rather than successful.
func (c *managementClient) doSafe(ctx context.Context, method, pathAndQuery string, body []byte, expected int, keepBody bool) ([]byte, error) {
	path, _, _ := strings.Cut(pathAndQuery, "?")
	operation := method + " " + path

	headers := map[string]string{"Accept": "application/json"}
	if body != nil {
		headers["Content-Type"] = "application/json"
	}
	resp, err := c.doRequestWithAuth(ctx, method, c.baseURL+pathAndQuery, headers, body)
	if err != nil {
		return nil, &CredentialError{Operation: operation}
	}

	var data []byte
	var readErr error
	if keepBody {
		data, readErr = io.ReadAll(io.LimitReader(resp.Body, maxProbeResponseBytes+1))
		if readErr == nil && len(data) > maxProbeResponseBytes {
			readErr = errors.New("response too large")
		}
	} else {
		discarded, err := io.Copy(io.Discard, io.LimitReader(resp.Body, maxDiscardedResponseBytes+1))
		readErr = err
		if readErr == nil && discarded > maxDiscardedResponseBytes {
			readErr = errors.New("response too large")
		}
	}
	closeErr := resp.Body.Close()

	if resp.StatusCode != expected {
		return nil, &CredentialError{Operation: operation, StatusCode: resp.StatusCode}
	}
	if readErr != nil || closeErr != nil {
		return nil, &CredentialError{Operation: operation, StatusCode: resp.StatusCode, ResponseIncomplete: true}
	}
	return data, nil
}
