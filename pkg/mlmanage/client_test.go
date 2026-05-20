// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package mlmanage

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRemoveDynamicHostUsesXMLBodyContract(t *testing.T) {
	t.Parallel()

	var gotMethod string
	var gotRequestURI string
	var gotContentType string
	var gotAccept string
	var gotBody string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		gotMethod = r.Method
		gotRequestURI = r.RequestURI
		gotContentType = r.Header.Get("Content-Type")
		gotAccept = r.Header.Get("Accept")
		gotBody = string(data)

		username, password, ok := r.BasicAuth()
		if !ok || username != "user" || password != "password" {
			t.Fatalf("expected basic auth user/password, got %q/%q", username, password)
		}

		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	client := &managementClient{
		baseURL:    server.URL,
		username:   "user",
		password:   "password",
		httpClient: server.Client(),
	}

	err := client.RemoveDynamicHost(context.Background(), "cluster one", "host-1")
	if err != nil {
		t.Fatalf("RemoveDynamicHost returned error: %v", err)
	}

	if gotMethod != http.MethodDelete {
		t.Fatalf("expected method DELETE, got %s", gotMethod)
	}
	if gotRequestURI != "/manage/v2/clusters/cluster%20one/dynamic-hosts" {
		t.Fatalf("expected request URI /manage/v2/clusters/cluster%%20one/dynamic-hosts, got %s", gotRequestURI)
	}
	if strings.Contains(gotRequestURI, "?") {
		t.Fatalf("expected no query string in remove request, got %s", gotRequestURI)
	}
	if gotContentType != "application/xml" {
		t.Fatalf("expected Content-Type application/xml, got %s", gotContentType)
	}
	if gotAccept != "application/xml" {
		t.Fatalf("expected Accept application/xml, got %s", gotAccept)
	}

	expectedBody := "<dynamic-hosts><dynamic-host>host-1</dynamic-host></dynamic-hosts>"
	if gotBody != expectedBody {
		t.Fatalf("expected body %q, got %q", expectedBody, gotBody)
	}
}

func TestRemoveDynamicHostEscapesXMLBodyText(t *testing.T) {
	t.Parallel()

	hostID := `a&b<c>d"e'f`
	var gotBody string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		gotBody = string(data)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	client := &managementClient{
		baseURL:    server.URL,
		username:   "user",
		password:   "password",
		httpClient: server.Client(),
	}

	err := client.RemoveDynamicHost(context.Background(), "cluster", hostID)
	if err != nil {
		t.Fatalf("RemoveDynamicHost returned error: %v", err)
	}

	expectedBody := "<dynamic-hosts><dynamic-host>a&amp;b&lt;c&gt;d&#34;e&#39;f</dynamic-host></dynamic-hosts>"
	if gotBody != expectedBody {
		t.Fatalf("expected escaped body %q, got %q", expectedBody, gotBody)
	}
}

func TestRemoveDynamicHostReturnsAPIError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("bad request"))
	}))
	defer server.Close()

	client := &managementClient{
		baseURL:    server.URL,
		username:   "user",
		password:   "password",
		httpClient: server.Client(),
	}

	err := client.RemoveDynamicHost(context.Background(), "cluster", "host-1")
	if err == nil {
		t.Fatal("expected error for non-success status")
	}
	if !strings.Contains(err.Error(), "returned status 400") {
		t.Fatalf("expected status detail in error, got %v", err)
	}
}
