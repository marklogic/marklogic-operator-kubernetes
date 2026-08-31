// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package mlmanage

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const (
	testAccessKey      = "AKIAIOSFODNN7EXAMPLE"
	testSecretKey      = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	testStorageAccount = "mystorageacct"
	testStorageKey     = "dGVzdHN0b3JhZ2VrZXk="
)

// credentialsTestServer captures the single request made by an Ensure* call.
func credentialsTestServer(t *testing.T, status int, responseBody string) (*httptest.Server, *http.Request, *[]byte) {
	t.Helper()

	captured := &http.Request{}
	body := &[]byte{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		read, _ := io.ReadAll(r.Body)
		*body = read
		*captured = *r
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if responseBody != "" {
			_, _ = w.Write([]byte(responseBody))
		}
	}))
	t.Cleanup(server.Close)

	return server, captured, body
}

func credentialsTestClient(server *httptest.Server) *managementClient {
	return &managementClient{baseURL: server.URL, httpClient: server.Client()}
}

func TestEnsureAWSCredentialsSendsTypeInBody(t *testing.T) {
	server, request, body := credentialsTestServer(t, http.StatusNoContent, "")
	client := credentialsTestClient(server)

	err := client.EnsureAWSCredentials(context.Background(), AWSCredentials{
		AccessKey: testAccessKey,
		SecretKey: testSecretKey,
	})
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}

	if request.Method != http.MethodPut {
		t.Fatalf("expected PUT, got %s", request.Method)
	}
	if request.URL.Path != credentialsPropertiesPath {
		t.Fatalf("unexpected path %q", request.URL.Path)
	}
	// The query string must not be relied on: MarkLogic ignores it.
	if got := request.URL.Query().Get("type"); got != "" {
		t.Fatalf("expected no type query parameter, got %q", got)
	}

	var payload map[string]any
	if err := json.Unmarshal(*body, &payload); err != nil {
		t.Fatalf("request body is not JSON: %v", err)
	}
	if payload["type"] != "aws" {
		t.Fatalf("expected body type 'aws', got %v", payload["type"])
	}
	if payload["access-key"] != testAccessKey || payload["secret-key"] != testSecretKey {
		t.Fatalf("unexpected credential fields in payload")
	}
	if _, present := payload["session-token"]; present {
		t.Fatal("session-token must never be sent")
	}
}

func TestEnsureAzureCredentialsSendsTypeInBody(t *testing.T) {
	server, request, body := credentialsTestServer(t, http.StatusNoContent, "")
	client := credentialsTestClient(server)

	err := client.EnsureAzureCredentials(context.Background(), AzureCredentials{
		StorageAccount: testStorageAccount,
		StorageKey:     testStorageKey,
	})
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if request.Method != http.MethodPut || request.URL.Path != credentialsPropertiesPath {
		t.Fatalf("unexpected request %s %s", request.Method, request.URL.Path)
	}

	var payload map[string]any
	if err := json.Unmarshal(*body, &payload); err != nil {
		t.Fatalf("request body is not JSON: %v", err)
	}
	// Azure returns 400 MANAGE-INVALIDPAYLOAD without this field.
	if payload["type"] != "azure" {
		t.Fatalf("expected body type 'azure', got %v", payload["type"])
	}
	if payload["storage-account"] != testStorageAccount || payload["storage-key"] != testStorageKey {
		t.Fatalf("unexpected credential fields in payload")
	}
}

func TestEnsureCredentialsRejectsMissingFields(t *testing.T) {
	t.Parallel()

	tests := map[string]func(*managementClient) error{
		"aws missing access key": func(c *managementClient) error {
			return c.EnsureAWSCredentials(context.Background(), AWSCredentials{SecretKey: testSecretKey})
		},
		"aws missing secret key": func(c *managementClient) error {
			return c.EnsureAWSCredentials(context.Background(), AWSCredentials{AccessKey: testAccessKey})
		},
		"azure missing storage account": func(c *managementClient) error {
			return c.EnsureAzureCredentials(context.Background(), AzureCredentials{StorageKey: testStorageKey})
		},
		"azure missing storage key": func(c *managementClient) error {
			return c.EnsureAzureCredentials(context.Background(), AzureCredentials{StorageAccount: testStorageAccount})
		},
	}

	for name, call := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			// No server: validation must fail before any request is attempted.
			err := call(&managementClient{baseURL: "http://unused.invalid"})
			if err == nil {
				t.Fatal("expected a validation error")
			}
			if !strings.Contains(err.Error(), "is required") {
				t.Fatalf("expected a required-field error, got %v", err)
			}
		})
	}
}

func TestEnsureCredentialsSurfacesStatusAndMessageCode(t *testing.T) {
	tests := map[string]struct {
		status          int
		responseBody    string
		wantMessageCode string
	}{
		"400 invalid payload": {
			status:          http.StatusBadRequest,
			responseBody:    `{"errorResponse":{"statusCode":"400","status":"Bad Request","messageCode":"MANAGE-INVALIDPAYLOAD","message":"Payload has errors in structure, content-type or values."}}`,
			wantMessageCode: "MANAGE-INVALIDPAYLOAD",
		},
		"403 insufficient privilege": {
			status:       http.StatusForbidden,
			responseBody: `{"errorResponse":{"statusCode":"403","status":"Forbidden","messageCode":"","message":"You do not have credentials to access /manage/v2/credentials/properties ."}}`,
		},
		"401 unauthenticated": {
			status:       http.StatusUnauthorized,
			responseBody: `{"errorResponse":{"statusCode":"401","status":"Unauthorized","messageCode":"","message":"Unauthorized"}}`,
		},
		"500 with no structured body": {
			status:       http.StatusInternalServerError,
			responseBody: `upstream failure`,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			server, _, _ := credentialsTestServer(t, test.status, test.responseBody)
			client := credentialsTestClient(server)

			err := client.EnsureAWSCredentials(context.Background(), AWSCredentials{
				AccessKey: testAccessKey,
				SecretKey: testSecretKey,
			})
			if err == nil {
				t.Fatal("expected an error")
			}

			var credentialsErr *CredentialsError
			if !errors.As(err, &credentialsErr) {
				t.Fatalf("expected *CredentialsError, got %T: %v", err, err)
			}
			if credentialsErr.StatusCode != test.status {
				t.Fatalf("expected status %d, got %d", test.status, credentialsErr.StatusCode)
			}
			if credentialsErr.MessageCode != test.wantMessageCode {
				t.Fatalf("expected messageCode %q, got %q", test.wantMessageCode, credentialsErr.MessageCode)
			}
		})
	}
}

// The request body carries credential material, so it must never appear in an
// error even when the server echoes it back.
func TestEnsureCredentialsErrorNeverLeaksMaterial(t *testing.T) {
	echoServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received, _ := io.ReadAll(r.Body)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write(received)
	}))
	t.Cleanup(echoServer.Close)

	client := credentialsTestClient(echoServer)

	awsErr := client.EnsureAWSCredentials(context.Background(), AWSCredentials{
		AccessKey: testAccessKey,
		SecretKey: testSecretKey,
	})
	azureErr := client.EnsureAzureCredentials(context.Background(), AzureCredentials{
		StorageAccount: testStorageAccount,
		StorageKey:     testStorageKey,
	})

	for _, err := range []error{awsErr, azureErr} {
		if err == nil {
			t.Fatal("expected an error")
		}
		for _, material := range []string{testAccessKey, testSecretKey, testStorageKey} {
			if strings.Contains(err.Error(), material) {
				t.Fatalf("error leaked credential material: %q", err.Error())
			}
		}
	}
}

func TestEnsureCredentialsWrapsTransportFailure(t *testing.T) {
	t.Parallel()

	transportErr := errors.New("dial tcp: connection refused")
	client := &managementClient{
		baseURL: "http://management.example.test",
		httpClient: &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
			return nil, transportErr
		})},
	}

	err := client.EnsureAWSCredentials(context.Background(), AWSCredentials{
		AccessKey: testAccessKey,
		SecretKey: testSecretKey,
	})
	if err == nil {
		t.Fatal("expected an error")
	}

	var credentialsErr *CredentialsError
	if !errors.As(err, &credentialsErr) {
		t.Fatalf("expected *CredentialsError, got %T", err)
	}
	// StatusCode 0 is what lets the controller distinguish "unreachable" from
	// an HTTP-level rejection.
	if credentialsErr.StatusCode != 0 {
		t.Fatalf("expected status code 0 for a transport failure, got %d", credentialsErr.StatusCode)
	}
	if !errors.Is(err, transportErr) {
		t.Fatalf("expected the transport error to be wrapped, got %v", err)
	}
}
