// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package mlmanage

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Client interface {
	ListHostsStatus(ctx context.Context) ([]HostStatus, error)
	GetGroup(ctx context.Context, groupName string) (GroupInfo, error)
	CreateGroup(ctx context.Context, groupName string) error
	EnableDynamicHosts(ctx context.Context, groupName string) error
	EnableAdminAPITokenAuthentication(ctx context.Context, groupName string) error
	EnsureManageAdminUser(ctx context.Context, username, password string) error
}

type ClientOptions struct {
	Host               string
	Username           string
	Password           string
	UseTLS             bool
	InsecureSkipVerify bool
	HTTPClient         *http.Client
}

type HostStatus struct {
	Name    string
	Online  bool
	Version string
}

type GroupInfo struct {
	Exists      bool
	ForestCount int
}

type managementClient struct {
	baseURL    string
	username   string
	password   string
	httpClient *http.Client
}

func NewClient(opts ClientOptions) Client {
	return &managementClient{
		baseURL:    buildBaseURL(opts.Host, opts.UseTLS),
		username:   opts.Username,
		password:   opts.Password,
		httpClient: buildHTTPClient(opts),
	}
}

func buildHTTPClient(opts ClientOptions) *http.Client {
	if opts.HTTPClient != nil {
		return opts.HTTPClient
	}
	transport := &http.Transport{}
	if opts.UseTLS {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: opts.InsecureSkipVerify}
	}
	return &http.Client{Timeout: 15 * time.Second, Transport: transport}
}

func buildBaseURL(host string, useTLS bool) string {
	scheme := "http"
	if useTLS {
		scheme = "https"
	}
	if _, _, err := net.SplitHostPort(host); err != nil {
		host = net.JoinHostPort(host, "8002")
	}
	return fmt.Sprintf("%s://%s", scheme, host)
}

func (c *managementClient) ListHostsStatus(ctx context.Context) ([]HostStatus, error) {
	query := url.Values{}
	query.Set("view", "status")
	query.Set("format", "json")
	data, _, err := c.doJSON(ctx, http.MethodGet, "/manage/v2/hosts", query, nil, http.StatusOK)
	if err != nil {
		return nil, err
	}

	var payload any
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, err
	}

	items := extractHostItems(payload)
	hosts := make([]HostStatus, 0, len(items))
	for i, item := range items {
		name := firstString(item, "nameref", "host-name", "name")
		if name == "" {
			name = fmt.Sprintf("host-%d", i)
		}
		status := strings.ToLower(firstString(item, "status", "host-status"))
		version := firstString(item, "version", "product-version")
		hosts = append(hosts, HostStatus{
			Name:    name,
			Online:  status == "online",
			Version: version,
		})
	}
	if len(hosts) == 0 {
		return nil, fmt.Errorf("no hosts returned from bootstrap")
	}

	clusterVersion := ""
	for _, host := range hosts {
		if host.Version != "" {
			clusterVersion = host.Version
			break
		}
	}
	if clusterVersion == "" {
		clusterVersion, _ = c.fetchClusterVersion(ctx)
	}
	if clusterVersion != "" {
		for i := range hosts {
			if hosts[i].Version == "" {
				hosts[i].Version = clusterVersion
			}
		}
	}

	return hosts, nil
}

func (c *managementClient) GetGroup(ctx context.Context, groupName string) (GroupInfo, error) {
	query := url.Values{}
	query.Set("format", "json")
	data, statusCode, err := c.doJSON(ctx, http.MethodGet, "/manage/v2/groups/"+url.PathEscape(groupName), query, nil, http.StatusOK, http.StatusNotFound)
	if err != nil {
		return GroupInfo{}, err
	}
	if statusCode == http.StatusNotFound {
		return GroupInfo{Exists: false}, nil
	}

	var payload any
	if err := json.Unmarshal(data, &payload); err != nil {
		return GroupInfo{}, err
	}
	return GroupInfo{Exists: true, ForestCount: countForests(payload)}, nil
}

func (c *managementClient) CreateGroup(ctx context.Context, groupName string) error {
	payload := map[string]any{"group-name": groupName}
	_, _, err := c.doJSON(ctx, http.MethodPost, "/manage/v2/groups", nil, payload, http.StatusCreated, http.StatusAccepted, http.StatusNoContent)
	return err
}

func (c *managementClient) EnableDynamicHosts(ctx context.Context, groupName string) error {
	payload := map[string]any{"allow-dynamic-hosts": true}
	_, _, err := c.doJSON(ctx, http.MethodPut, "/manage/v2/groups/"+url.PathEscape(groupName)+"/properties", nil, payload, http.StatusAccepted, http.StatusNoContent)
	return err
}

func (c *managementClient) EnableAdminAPITokenAuthentication(ctx context.Context, groupName string) error {
	payload := map[string]any{"API-token-authentication": true}
	query := url.Values{}
	query.Set("group-id", groupName)
	_, _, err := c.doJSON(ctx, http.MethodPut, "/manage/v2/servers/Admin/properties", query, payload, http.StatusAccepted, http.StatusNoContent)
	return err
}

func (c *managementClient) EnsureManageAdminUser(ctx context.Context, username, password string) error {
	query := url.Values{}
	query.Set("format", "json")
	_, statusCode, err := c.doJSON(ctx, http.MethodGet, "/manage/v2/users/"+url.PathEscape(username), query, nil, http.StatusOK, http.StatusNotFound)
	if err != nil {
		return err
	}

	payload := map[string]any{
		"user-name": username,
		"password":  password,
		"role":      []string{"manage-admin"},
	}
	if statusCode == http.StatusNotFound {
		_, _, err = c.doJSON(ctx, http.MethodPost, "/manage/v2/users", nil, payload, http.StatusCreated, http.StatusAccepted, http.StatusNoContent)
		return err
	}

	_, _, err = c.doJSON(ctx, http.MethodPut, "/manage/v2/users/"+url.PathEscape(username)+"/properties", nil, payload, http.StatusAccepted, http.StatusNoContent)
	return err
}

func (c *managementClient) fetchClusterVersion(ctx context.Context) (string, error) {
	query := url.Values{}
	query.Set("format", "json")
	data, _, err := c.doJSON(ctx, http.MethodGet, "/manage/v2", query, nil, http.StatusOK)
	if err != nil {
		return "", err
	}
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		return "", err
	}
	return firstString(payload, "version", "product-version"), nil
}

func (c *managementClient) doJSON(ctx context.Context, method, path string, query url.Values, body any, expectedStatus ...int) ([]byte, int, error) {
	endpoint := c.baseURL + path
	if len(query) > 0 {
		endpoint = endpoint + "?" + query.Encode()
	}

	var bodyReader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		bodyReader = bytes.NewBuffer(payload)
	}

	req, err := http.NewRequestWithContext(ctx, method, endpoint, bodyReader)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.SetBasicAuth(c.username, c.password)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}

	for _, code := range expectedStatus {
		if resp.StatusCode == code {
			return data, resp.StatusCode, nil
		}
	}
	return data, resp.StatusCode, fmt.Errorf("management api %s %s returned status %d: %s", method, path, resp.StatusCode, string(data))
}

func extractHostItems(payload any) []map[string]any {
	root, ok := payload.(map[string]any)
	if !ok {
		return nil
	}
	listItems, ok := root["host-default-list"].(map[string]any)
	if !ok {
		return nil
	}
	listData, ok := listItems["list-items"].(map[string]any)
	if !ok {
		return nil
	}
	rawItems, ok := listData["list-item"]
	if !ok {
		return nil
	}

	result := []map[string]any{}
	switch items := rawItems.(type) {
	case []any:
		for _, raw := range items {
			if entry, ok := raw.(map[string]any); ok {
				result = append(result, entry)
			}
		}
	case map[string]any:
		result = append(result, items)
	}
	return result
}

func countForests(payload any) int {
	forestNodes := 0
	walkAny(payload, func(m map[string]any) {
		for key, value := range m {
			if key != "forest" {
				continue
			}
			switch v := value.(type) {
			case []any:
				forestNodes += len(v)
			case map[string]any:
				forestNodes++
			case string:
				if v != "" {
					forestNodes++
				}
			}
		}
	})
	return forestNodes
}

func walkAny(payload any, fn func(map[string]any)) {
	switch current := payload.(type) {
	case map[string]any:
		fn(current)
		for _, value := range current {
			walkAny(value, fn)
		}
	case []any:
		for _, value := range current {
			walkAny(value, fn)
		}
	}
}

func firstString(data map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := data[key]; ok {
			if converted := toString(value); converted != "" {
				return converted
			}
		}
	}
	return ""
}

func toString(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case float64:
		return fmt.Sprintf("%g", v)
	case bool:
		if v {
			return "true"
		}
		return "false"
	default:
		return ""
	}
}
