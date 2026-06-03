// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package mlmanage

import (
	"context"
	"encoding/json"
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

func TestDoJSONRetriesWithDigestAuthChallenge(t *testing.T) {
	t.Parallel()

	var calls int
	var authHeaders []string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		authHeaders = append(authHeaders, r.Header.Get("Authorization"))
		if calls == 1 {
			w.Header().Set("WWW-Authenticate", `Digest realm="manage", nonce="nonce123", qop="auth", algorithm=MD5`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Digest ") {
			t.Fatalf("expected digest authorization header on retry, got %q", r.Header.Get("Authorization"))
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer server.Close()

	client := &managementClient{
		baseURL:    server.URL,
		username:   "user",
		password:   "password",
		httpClient: server.Client(),
	}

	_, _, err := client.doJSON(context.Background(), http.MethodGet, "/manage/v2", nil, nil, http.StatusOK)
	if err != nil {
		t.Fatalf("doJSON returned error: %v", err)
	}

	if calls != 2 {
		t.Fatalf("expected 2 calls (no-auth then digest), got %d", calls)
	}
	if len(authHeaders) != 2 {
		t.Fatalf("expected 2 auth headers, got %d", len(authHeaders))
	}
	if authHeaders[0] != "" {
		t.Fatalf("expected first request to have no Authorization header, got %q", authHeaders[0])
	}
	if !strings.HasPrefix(authHeaders[1], "Digest ") {
		t.Fatalf("expected second request to use digest auth, got %q", authHeaders[1])
	}
}

func TestFetchClusterVersionParsesNestedVersion(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/manage/v2" {
			t.Fatalf("expected /manage/v2 path, got %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"local-cluster-default":{"version":"12.0.0","effective-version":12000000}}`))
	}))
	defer server.Close()

	client := &managementClient{
		baseURL:    server.URL,
		username:   "user",
		password:   "password",
		httpClient: server.Client(),
	}

	version, err := client.fetchClusterVersion(context.Background())
	if err != nil {
		t.Fatalf("fetchClusterVersion returned error: %v", err)
	}
	if version != "12.0.0" {
		t.Fatalf("expected version 12.0.0, got %s", version)
	}
}

func TestResolveClusterNameParsesVersionEnvelopeKey(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/manage/v2/clusters":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"errorResponse":{"message":"not found"}}`))
		case "/manage/v2":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"local-cluster-default":{"version":"12.0.0","effective-version":12000000}}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client := &managementClient{
		baseURL:    server.URL,
		username:   "user",
		password:   "password",
		httpClient: server.Client(),
	}

	clusterName, err := client.ResolveClusterName(context.Background())
	if err != nil {
		t.Fatalf("ResolveClusterName returned error: %v", err)
	}
	if clusterName != "local-cluster-default" {
		t.Fatalf("expected cluster name local-cluster-default, got %s", clusterName)
	}
}

func TestResolveClusterNameParsesExplicitClusterName(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/manage/v2/clusters":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"cluster-name":"ml-dynamic-cluster"}`))
		case "/manage/v2":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"cluster-name":"should-not-be-used"}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client := &managementClient{
		baseURL:    server.URL,
		username:   "user",
		password:   "password",
		httpClient: server.Client(),
	}

	clusterName, err := client.ResolveClusterName(context.Background())
	if err != nil {
		t.Fatalf("ResolveClusterName returned error: %v", err)
	}
	if clusterName != "ml-dynamic-cluster" {
		t.Fatalf("expected cluster name ml-dynamic-cluster, got %s", clusterName)
	}
}

func TestResolveClusterNamePrefersClusterListEnvelope(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/manage/v2/clusters":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"cluster-default-list":{"list-items":{"list-item":[{"nameref":"actual-cluster-name"}]}}}`))
		case "/manage/v2":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"cluster-name":"fallback-cluster"}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client := &managementClient{
		baseURL:    server.URL,
		username:   "user",
		password:   "password",
		httpClient: server.Client(),
	}

	clusterName, err := client.ResolveClusterName(context.Background())
	if err != nil {
		t.Fatalf("ResolveClusterName returned error: %v", err)
	}
	if clusterName != "actual-cluster-name" {
		t.Fatalf("expected cluster name actual-cluster-name, got %s", clusterName)
	}
}

func TestResolveClusterNameCandidatesFromClusterListIncludesIdrefAndNameref(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/manage/v2/clusters":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"cluster-default-list":{"list-items":{"list-item":[{"nameref":"local-cluster-default","idref":"actual-cluster-id"}]}}}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client := &managementClient{
		baseURL:    server.URL,
		username:   "user",
		password:   "password",
		httpClient: server.Client(),
	}

	candidates, err := client.ResolveClusterNameCandidates(context.Background())
	if err != nil {
		t.Fatalf("ResolveClusterNameCandidates returned error: %v", err)
	}

	expected := []string{"actual-cluster-id", "local-cluster-default"}
	if len(candidates) < len(expected) {
		t.Fatalf("expected at least %d cluster name candidates, got %v", len(expected), candidates)
	}
	for i, candidate := range expected {
		if candidates[i] != candidate {
			t.Fatalf("expected candidate order prefix %v, got %v", expected, candidates)
		}
	}
}

func TestListHostsStatusParsesHostStatusListEnvelope(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/manage/v2/hosts" {
			t.Fatalf("expected /manage/v2/hosts path, got %s", r.URL.Path)
		}
		if r.URL.Query().Get("view") != "status" {
			t.Fatalf("expected view=status, got %s", r.URL.Query().Get("view"))
		}
		if r.URL.Query().Get("format") != "json" {
			t.Fatalf("expected format=json, got %s", r.URL.Query().Get("format"))
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"host-status-list":{"summary":{"total-hosts-offline":{"units":"quantity","value":0}},"status-list-items":{"status-list-item":[{"nameref":"node-0.node.default.svc.cluster.local","version":"12.0-1"}]}}}`))
	}))
	defer server.Close()

	client := &managementClient{
		baseURL:    server.URL,
		username:   "user",
		password:   "password",
		httpClient: server.Client(),
	}

	hosts, err := client.ListHostsStatus(context.Background())
	if err != nil {
		t.Fatalf("ListHostsStatus returned error: %v", err)
	}
	if len(hosts) != 1 {
		t.Fatalf("expected 1 host, got %d", len(hosts))
	}
	if hosts[0].Name != "node-0.node.default.svc.cluster.local" {
		t.Fatalf("expected host name node-0.node.default.svc.cluster.local, got %s", hosts[0].Name)
	}
	if !hosts[0].Online {
		t.Fatalf("expected host to be online")
	}
	if hosts[0].Version != "12.0-1" {
		t.Fatalf("expected version 12.0-1, got %s", hosts[0].Version)
	}
}

func TestListHostsStatusUsesSummaryWhenItemStatusMissing(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"host-status-list":{"status-list-summary":{"total-hosts-offline":{"units":"quantity","value":1}},"status-list-items":{"status-list-item":[{"nameref":"node-0.node.default.svc.cluster.local"}]}}}`))
	}))
	defer server.Close()

	client := &managementClient{
		baseURL:    server.URL,
		username:   "user",
		password:   "password",
		httpClient: server.Client(),
	}

	hosts, err := client.ListHostsStatus(context.Background())
	if err != nil {
		t.Fatalf("ListHostsStatus returned error: %v", err)
	}
	if len(hosts) != 1 {
		t.Fatalf("expected 1 host, got %d", len(hosts))
	}
	if hosts[0].Online {
		t.Fatalf("expected host to be inferred offline when total-hosts-offline > 0")
	}
}

func TestListHostsStatusUsesStatusListSummaryWhenItemStatusMissing(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"host-status-list":{"status-list-summary":{"total-hosts-offline":{"units":"quantity","value":0}},"status-list-items":{"status-list-item":[{"nameref":"node-0.node.default.svc.cluster.local"}]}}}`))
	}))
	defer server.Close()

	client := &managementClient{
		baseURL:    server.URL,
		username:   "user",
		password:   "password",
		httpClient: server.Client(),
	}

	hosts, err := client.ListHostsStatus(context.Background())
	if err != nil {
		t.Fatalf("ListHostsStatus returned error: %v", err)
	}
	if len(hosts) != 1 {
		t.Fatalf("expected 1 host, got %d", len(hosts))
	}
	if !hosts[0].Online {
		t.Fatalf("expected host to be inferred online when total-hosts-offline == 0")
	}
}

func TestListHostsStatusParsesHostDefaultListEnvelope(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"host-default-list":{"list-items":{"list-item":[{"nameref":"node-0","status":"online","version":"12.0-1"}]}}}`))
	}))
	defer server.Close()

	client := &managementClient{
		baseURL:    server.URL,
		username:   "user",
		password:   "password",
		httpClient: server.Client(),
	}

	hosts, err := client.ListHostsStatus(context.Background())
	if err != nil {
		t.Fatalf("ListHostsStatus returned error: %v", err)
	}
	if len(hosts) != 1 {
		t.Fatalf("expected 1 host, got %d", len(hosts))
	}
	if hosts[0].Name != "node-0" {
		t.Fatalf("expected host name node-0, got %s", hosts[0].Name)
	}
}
