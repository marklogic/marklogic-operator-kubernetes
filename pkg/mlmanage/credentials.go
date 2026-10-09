// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package mlmanage

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

const (
	// CredentialsPropertiesPath is the Management API endpoint used to set object storage credentials.
	CredentialsPropertiesPath = "/manage/v2/credentials/properties"
	// bootstrapProbePath is a read-only, authenticated endpoint used to confirm the Management API is serving.
	bootstrapProbePath = "/manage/v2/hosts"

	maxDiscardedResponseBytes = 64 * 1024
)

// CredentialClient applies object storage credentials through the Management API.
// It deliberately has no read or delete operations.
type CredentialClient interface {
	// CheckBootstrapReady confirms the authenticated Management API is serving requests.
	CheckBootstrapReady(ctx context.Context) error
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
}

func (e *CredentialError) Error() string {
	if e.StatusCode == 0 {
		return fmt.Sprintf("management api %s failed without an HTTP response", e.Operation)
	}
	return fmt.Sprintf("management api %s returned status %d", e.Operation, e.StatusCode)
}

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

func (c *managementClient) CheckBootstrapReady(ctx context.Context) error {
	return c.doSafe(ctx, http.MethodGet, bootstrapProbePath+"?view=status&format=json", nil, http.StatusOK)
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
	return c.doSafe(ctx, http.MethodPut, CredentialsPropertiesPath, body, http.StatusNoContent)
}

// doSafe performs an authenticated request and discards the response body. Failures never include
// request or response content.
func (c *managementClient) doSafe(ctx context.Context, method, pathAndQuery string, body []byte, expected int) error {
	path := pathAndQuery
	for i, r := range pathAndQuery {
		if r == '?' {
			path = pathAndQuery[:i]
			break
		}
	}
	operation := method + " " + path

	headers := map[string]string{"Accept": "application/json"}
	if body != nil {
		headers["Content-Type"] = "application/json"
	}
	resp, err := c.doRequestWithAuth(ctx, method, c.baseURL+pathAndQuery, headers, body)
	if err != nil {
		return &CredentialError{Operation: operation}
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDiscardedResponseBytes))
	_ = resp.Body.Close()
	if resp.StatusCode != expected {
		return &CredentialError{Operation: operation, StatusCode: resp.StatusCode}
	}
	return nil
}
