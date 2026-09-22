// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package oauth

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/marklogic/marklogic-operator-kubernetes/test/integration/marklogic-server/fixtures/oauthclient"
	"github.com/marklogic/marklogic-operator-kubernetes/test/integration/marklogic-server/testutil"
)

// Inspect each live node's Management API, not MARKLOGIC_VERSION or the image tag.
func verifyAuthCodeServerVersions(t *testing.T, namespace, cluster string) {
	t.Helper()
	topology := make([]map[string]string, 0, 2)
	for node := 0; node < 2; node++ {
		host := marklogicServerDNSNames(cluster, namespace, node)[0]
		output := testutil.ExecuteInPod(t, namespace, oauthclient.DefaultName, "curl", "curl", "--fail", "--silent", "--show-error", "--max-time", "20", "--digest", "--user", marklogicAdminUsername+":"+marklogicAdminPassword, "https://"+host+":8002/manage/v2?format=json")
		version, err := authCodeServerVersion([]byte(output))
		if err != nil {
			t.Fatalf("node-%d (%s): %v", node, host, err)
		}
		query := `xquery version "1.0-ml";
string-join((concat("HOST_ID=", xdmp:host()),
concat("HOSTS=", string-join(for $h in xdmp:hosts() order by $h return string($h), ",")),
concat("SECURITY_ID=", xdmp:database("Security"))), "&#10;")`
		observation := testutil.ExecuteInPod(t, namespace, oauthclient.DefaultName, "curl", "curl", "--fail", "--silent", "--show-error", "--max-time", "20", "--digest", "--user", marklogicAdminUsername+":"+marklogicAdminPassword, "--data-urlencode", "xquery="+query, "https://"+host+":8000/v1/eval")
		topology = append(topology, parseMarkers(observation))
		t.Logf("Actual MarkLogic backend=node-%d pod=%s-%d dns=%s version=%s", node, cluster, node, host, version)
	}
	if err := validateAuthCodeTopology(topology); err != nil {
		t.Fatal(err)
	}
	for node, observation := range topology {
		t.Logf("Verified cluster backend=node-%d hostID=%s sharedHosts=%s securityDatabaseID=%s", node, observation["HOST_ID"], observation["HOSTS"], observation["SECURITY_ID"])
	}
}

func validateAuthCodeTopology(nodes []map[string]string) error {
	if len(nodes) != 2 {
		return fmt.Errorf("require two independently observed MarkLogic nodes")
	}
	numeric := regexp.MustCompile(`^[1-9][0-9]*$`)
	for _, node := range nodes {
		if !numeric.MatchString(node["HOST_ID"]) || !numeric.MatchString(node["SECURITY_ID"]) {
			return fmt.Errorf("missing live host or Security database identity")
		}
		hosts := strings.Split(node["HOSTS"], ",")
		if len(hosts) != 2 || hosts[0] == hosts[1] || !numeric.MatchString(hosts[0]) || !numeric.MatchString(hosts[1]) || (node["HOST_ID"] != hosts[0] && node["HOST_ID"] != hosts[1]) {
			return fmt.Errorf("live node does not report the expected two-node cluster")
		}
	}
	if nodes[0]["HOST_ID"] == nodes[1]["HOST_ID"] || nodes[0]["HOSTS"] != nodes[1]["HOSTS"] || nodes[0]["SECURITY_ID"] != nodes[1]["SECURITY_ID"] {
		return fmt.Errorf("nodes must be distinct members of one cluster sharing the Security database")
	}
	return nil
}

func authCodeServerVersion(data []byte) (string, error) {
	var root any
	if err := json.Unmarshal(data, &root); err != nil {
		return "", fmt.Errorf("invalid Management API version response")
	}
	versions := map[string]bool{}
	var visit func(any)
	visit = func(value any) {
		switch v := value.(type) {
		case map[string]any:
			for key, child := range v {
				if key == "version" || key == "product-version" {
					if version, ok := child.(string); ok {
						versions[version] = true
					}
				} else {
					visit(child)
				}
			}
		case []any:
			for _, child := range v {
				visit(child)
			}
		}
	}
	visit(root)
	if len(versions) != 1 {
		return "", fmt.Errorf("Management API must identify one unambiguous actual Server version")
	}
	for version := range versions {
		parts := regexp.MustCompile(`^(\d+)\.(\d+)(?:[.\-][0-9A-Za-z.\-]+)?$`).FindStringSubmatch(version)
		if parts == nil {
			return "", fmt.Errorf("unrecognized Server version format")
		}
		major, _ := strconv.Atoi(parts[1])
		minor, _ := strconv.Atoi(parts[2])
		if major < 12 || major == 12 && minor < 1 {
			return "", fmt.Errorf("actual Server %s does not meet this suite's 12.1+ prerequisite", version)
		}
		return version, nil
	}
	return "", fmt.Errorf("missing Server version")
}

func TestAuthCodeActualVersion(t *testing.T) {
	for _, tc := range []struct {
		body  string
		valid bool
	}{
		{`{"local-cluster-default":{"version":"12.1-0"}}`, true},
		{`{"version":"12.1.0"}`, true}, {`{"version":"12.0-3"}`, false},
		{`{"image":"marklogic:12.1.0"}`, false}, {`{"version":"12.1.0","other":{"version":"12.0.3"}}`, false},
		{`{"version":"garbage"}`, false}, {`not json`, false},
	} {
		_, err := authCodeServerVersion([]byte(tc.body))
		if (err == nil) != tc.valid {
			t.Errorf("%s: %v", strings.ReplaceAll(tc.body, "\n", ""), err)
		}
	}
}

func TestAuthCodeTopology(t *testing.T) {
	for _, key := range []string{"", "HOST_ID", "HOSTS", "SECURITY_ID"} {
		nodes := []map[string]string{{"HOST_ID": "1", "HOSTS": "1,2", "SECURITY_ID": "3"}, {"HOST_ID": "2", "HOSTS": "1,2", "SECURITY_ID": "3"}}
		if key != "" {
			nodes[1][key] = "1"
		}
		if err := validateAuthCodeTopology(nodes); (err == nil) != (key == "") {
			t.Errorf("%s: %v", key, err)
		}
	}
}
