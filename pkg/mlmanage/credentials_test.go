// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package mlmanage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type recordedRequest struct {
	method      string
	requestURI  string
	contentType string
	body        map[string]string
}

func newCredentialTestServer(t *testing.T, status int, responseBody string) (CredentialClient, *[]recordedRequest, func()) {
	t.Helper()
	var requests []recordedRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		raw, _ := io.ReadAll(r.Body)
		rec := recordedRequest{method: r.Method, requestURI: r.RequestURI, contentType: r.Header.Get("Content-Type")}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &rec.body); err != nil {
				t.Errorf("request body is not a JSON object of strings")
			}
		}
		requests = append(requests, rec)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(responseBody))
	}))
	client := &managementClient{baseURL: server.URL, username: "user", password: "password", httpClient: server.Client()}
	return client, &requests, server.Close
}

func TestApplyAWSCredentialsPayload(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		creds AWSCredentials
		want  map[string]string
	}{
		{
			name:  "without session token",
			creds: AWSCredentials{AccessKey: "ak", SecretKey: "sk"},
			want:  map[string]string{"type": "aws", "access-key": "ak", "secret-key": "sk"},
		},
		{
			name:  "with session token",
			creds: AWSCredentials{AccessKey: "ak", SecretKey: "sk", SessionToken: "st"},
			want:  map[string]string{"type": "aws", "access-key": "ak", "secret-key": "sk", "session-token": "st"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			client, requests, closeServer := newCredentialTestServer(t, http.StatusNoContent, "")
			defer closeServer()

			if err := client.ApplyAWSCredentials(context.Background(), test.creds); err != nil {
				t.Fatalf("ApplyAWSCredentials returned error: %v", err)
			}
			if len(*requests) != 1 {
				t.Fatalf("expected 1 request, got %d", len(*requests))
			}
			got := (*requests)[0]
			if got.method != http.MethodPut || got.requestURI != CredentialsPropertiesPath || got.contentType != "application/json" {
				t.Fatalf("unexpected request %s %s (%s)", got.method, got.requestURI, got.contentType)
			}
			if len(got.body) != len(test.want) {
				t.Fatalf("payload has %d fields, want %d", len(got.body), len(test.want))
			}
			for key, value := range test.want {
				if got.body[key] != value {
					t.Fatalf("payload field %q differs from expectation", key)
				}
			}
		})
	}
}

func TestApplyAzureCredentialsPayload(t *testing.T) {
	t.Parallel()

	client, requests, closeServer := newCredentialTestServer(t, http.StatusNoContent, "")
	defer closeServer()

	if err := client.ApplyAzureCredentials(context.Background(), AzureCredentials{StorageAccount: "acct", StorageKey: "key"}); err != nil {
		t.Fatalf("ApplyAzureCredentials returned error: %v", err)
	}
	got := (*requests)[0]
	want := map[string]string{"type": "azure", "storage-account": "acct", "storage-key": "key"}
	if len(got.body) != len(want) {
		t.Fatalf("payload has %d fields, want %d", len(got.body), len(want))
	}
	for key, value := range want {
		if got.body[key] != value {
			t.Fatalf("payload field %q differs from expectation", key)
		}
	}
}

func TestApplyCredentialsFailureIsSecretSafe(t *testing.T) {
	t.Parallel()

	const hostileBody = "echo of ak-secret-value and sk-secret-value"
	for _, status := range []int{400, 401, 403, 404, 405, 500, 503, 200, 201} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			t.Parallel()
			client, _, closeServer := newCredentialTestServer(t, status, hostileBody)
			defer closeServer()

			err := client.ApplyAWSCredentials(context.Background(), AWSCredentials{AccessKey: "ak-secret-value", SecretKey: "sk-secret-value", SessionToken: "st-secret-value"})
			var credErr *CredentialError
			if !errors.As(err, &credErr) {
				t.Fatalf("expected CredentialError, got %v", err)
			}
			if credErr.StatusCode != status || credErr.IsTransport() {
				t.Fatalf("status code = %d, transport = %v, want %d", credErr.StatusCode, credErr.IsTransport(), status)
			}
			message := err.Error()
			for _, leaked := range []string{"secret-value", "echo of"} {
				if strings.Contains(message, leaked) {
					t.Fatalf("error message leaks request or response content")
				}
			}
			if !strings.Contains(message, fmt.Sprint(status)) || !strings.Contains(message, CredentialsPropertiesPath) {
				t.Fatalf("error message %q must contain the status code and endpoint", message)
			}
		})
	}
}

func TestApplyCredentialsTransportFailureHasNoStatusCode(t *testing.T) {
	t.Parallel()

	client := &managementClient{
		baseURL: "http://management.example.test",
		httpClient: &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("dial tcp 10.0.0.1: connection refused for key sk-secret-value")
		})},
	}
	err := client.ApplyAzureCredentials(context.Background(), AzureCredentials{StorageAccount: "a", StorageKey: "sk-secret-value"})
	var credErr *CredentialError
	if !errors.As(err, &credErr) || !credErr.IsTransport() || credErr.StatusCode != 0 {
		t.Fatalf("expected transport CredentialError, got %v", err)
	}
	if strings.Contains(err.Error(), "secret-value") || strings.Contains(err.Error(), "10.0.0.1") {
		t.Fatalf("transport error leaks raw error text")
	}
}

func TestApplyCredentialsRetriesWithDigestChallenge(t *testing.T) {
	t.Parallel()

	var calls int
	var bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		raw, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(raw))
		if calls == 1 {
			w.Header().Set("WWW-Authenticate", `Digest realm="manage", nonce="nonce123", qop="auth", algorithm=MD5`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Digest ") {
			t.Errorf("retry must carry a digest authorization header")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	client := &managementClient{baseURL: server.URL, username: "user", password: "password", httpClient: server.Client()}
	if err := client.ApplyAzureCredentials(context.Background(), AzureCredentials{StorageAccount: "a", StorageKey: "k"}); err != nil {
		t.Fatalf("expected success after digest retry, got %v", err)
	}
	if calls != 2 || len(bodies) != 2 || bodies[0] == "" || bodies[0] != bodies[1] {
		t.Fatalf("digest retry must replay the same body; calls=%d", calls)
	}
}

func TestCheckBootstrapReady(t *testing.T) {
	t.Parallel()

	t.Run("ready", func(t *testing.T) {
		t.Parallel()
		client, requests, closeServer := newCredentialTestServer(t, http.StatusOK, `{"hosts":"ignored"}`)
		defer closeServer()
		if err := client.CheckBootstrapReady(context.Background()); err != nil {
			t.Fatalf("expected ready, got %v", err)
		}
		if got := (*requests)[0]; got.method != http.MethodGet || !strings.HasPrefix(got.requestURI, "/manage/v2/hosts") {
			t.Fatalf("probe must be a read-only hosts request, got %s %s", got.method, got.requestURI)
		}
	})

	t.Run("unauthorized", func(t *testing.T) {
		t.Parallel()
		client, _, closeServer := newCredentialTestServer(t, http.StatusUnauthorized, "bad credentials for user")
		defer closeServer()
		err := client.CheckBootstrapReady(context.Background())
		var credErr *CredentialError
		if !errors.As(err, &credErr) || credErr.StatusCode != http.StatusUnauthorized {
			t.Fatalf("expected 401 CredentialError, got %v", err)
		}
		if strings.Contains(err.Error(), "bad credentials") {
			t.Fatalf("probe error leaks the response body")
		}
		if strings.Contains(err.Error(), "?") {
			t.Fatalf("probe error must not include the query string")
		}
	})
}

func TestCredentialStringersRedact(t *testing.T) {
	t.Parallel()

	aws := AWSCredentials{AccessKey: "ak-secret-value", SecretKey: "sk-secret-value", SessionToken: "st-secret-value"}
	azure := AzureCredentials{StorageAccount: "acct-secret-value", StorageKey: "key-secret-value"}
	for _, formatted := range []string{
		fmt.Sprintf("%v", aws), fmt.Sprintf("%+v", aws), fmt.Sprintf("%#v", aws), fmt.Sprint(aws),
		fmt.Sprintf("%v", azure), fmt.Sprintf("%+v", azure), fmt.Sprintf("%#v", azure), fmt.Sprint(azure),
	} {
		if strings.Contains(formatted, "secret-value") {
			t.Fatalf("formatted credentials expose material")
		}
	}
}
