// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package mlmanage

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"encoding/xml"
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
	RequestDynamicHostToken(ctx context.Context, clusterName, groupName, hostFQDN, duration string) (string, error)
	JoinDynamicHost(ctx context.Context, hostFQDN, token string) error
	ListGroupHosts(ctx context.Context, groupName string) ([]GroupHost, error)
	RemoveDynamicHost(ctx context.Context, clusterName, hostID string) error
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

type GroupHost struct {
	Name   string
	HostID string
	Online bool
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

func (c *managementClient) RequestDynamicHostToken(ctx context.Context, clusterName, groupName, hostFQDN, duration string) (string, error) {
	if strings.TrimSpace(duration) == "" {
		duration = "PT15M"
	}
	payload := map[string]any{
		"dynamic-host-token": map[string]any{
			"group":    groupName,
			"host":     hostFQDN,
			"port":     8001,
			"duration": duration,
			"comment":  "operator-managed",
		},
	}
	data, _, err := c.doJSON(ctx, http.MethodPost, "/manage/v2/clusters/"+url.PathEscape(clusterName)+"/dynamic-host-token", nil, payload, http.StatusCreated, http.StatusOK)
	if err != nil {
		return "", err
	}

	var body any
	if err := json.Unmarshal(data, &body); err != nil {
		return "", err
	}
	token := extractDynamicHostToken(body)
	if token == "" {
		return "", fmt.Errorf("dynamic host token was not present in response")
	}
	return token, nil
}

func (c *managementClient) JoinDynamicHost(ctx context.Context, hostFQDN, token string) error {
	scheme := "http"
	if strings.HasPrefix(c.baseURL, "https://") {
		scheme = "https"
	}
	host := hostFQDN
	if parsedHost, _, err := net.SplitHostPort(hostFQDN); err == nil {
		host = parsedHost
	}
	joinURL := fmt.Sprintf("%s://%s:8001/admin/v1/init", scheme, host)
	body := fmt.Sprintf("<init xmlns=\"http://marklogic.com/manage\"><dynamic-host-token>%s</dynamic-host-token></init>", token)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, joinURL, strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/xml")
	req.Header.Set("Accept", "application/xml")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusNoContent {
		return nil
	}

	return fmt.Errorf("dynamic host init POST /admin/v1/init returned status %d: %s", resp.StatusCode, string(respBody))
}

func (c *managementClient) ListGroupHosts(ctx context.Context, groupName string) ([]GroupHost, error) {
	query := url.Values{}
	query.Set("group-id", groupName)
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
	hosts := make([]GroupHost, 0, len(items))
	for _, item := range items {
		name := firstString(item, "nameref", "host-name", "name")
		if name == "" {
			continue
		}
		status := strings.ToLower(firstString(item, "status", "host-status"))
		hosts = append(hosts, GroupHost{
			Name:   name,
			HostID: firstString(item, "id", "idref", "host-id"),
			Online: status == "online",
		})
	}
	return hosts, nil
}

func (c *managementClient) RemoveDynamicHost(ctx context.Context, clusterName, hostID string) error {
	if strings.TrimSpace(clusterName) == "" {
		return fmt.Errorf("cluster name is required for dynamic host removal")
	}
	if strings.TrimSpace(hostID) == "" {
		return fmt.Errorf("host ID is required for dynamic host removal")
	}
	body := fmt.Sprintf("<dynamic-hosts><dynamic-host>%s</dynamic-host></dynamic-hosts>", escapeXMLText(hostID))
	_, _, err := c.doXML(ctx, http.MethodDelete, "/manage/v2/clusters/"+url.PathEscape(clusterName)+"/dynamic-hosts", nil, body, http.StatusAccepted, http.StatusNoContent, http.StatusOK)
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

func (c *managementClient) doXML(ctx context.Context, method, path string, query url.Values, body string, expectedStatus ...int) ([]byte, int, error) {
	endpoint := c.baseURL + path
	if len(query) > 0 {
		endpoint = endpoint + "?" + query.Encode()
	}

	var bodyReader io.Reader
	if strings.TrimSpace(body) != "" {
		bodyReader = strings.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, endpoint, bodyReader)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/xml")
	if bodyReader != nil {
		req.Header.Set("Content-Type", "application/xml")
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

func escapeXMLText(value string) string {
	var buf bytes.Buffer
	if err := xml.EscapeText(&buf, []byte(value)); err != nil {
		return value
	}
	return buf.String()
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

func extractDynamicHostToken(payload any) string {
	root, ok := payload.(map[string]any)
	if ok {
		if tokenValue, exists := root["dynamic-host-token"]; exists {
			switch t := tokenValue.(type) {
			case string:
				return t
			case map[string]any:
				if token := firstString(t, "token", "dynamic-host-token", "value"); token != "" {
					return token
				}
			}
		}
	}

	if token := findFirstStringByKeys(payload, "token", "dynamic-host-token"); token != "" {
		return token
	}
	return ""
}

func findFirstStringByKeys(payload any, keys ...string) string {
	switch current := payload.(type) {
	case map[string]any:
		for _, key := range keys {
			if value, ok := current[key]; ok {
				if str := toString(value); str != "" {
					return str
				}
			}
		}
		for _, value := range current {
			if found := findFirstStringByKeys(value, keys...); found != "" {
				return found
			}
		}
	case []any:
		for _, value := range current {
			if found := findFirstStringByKeys(value, keys...); found != "" {
				return found
			}
		}
	}
	return ""
}
