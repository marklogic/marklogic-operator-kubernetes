// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package oauth

import (
	"fmt"
	"testing"

	"github.com/marklogic/marklogic-operator-kubernetes/test/integration/marklogic-server/testutil"
)

// installOAuthIdentityProbe gives both flows the same observable success result.
// A plain HTTP App Server does not automatically provide REST API endpoints.
func installOAuthIdentityProbe(t *testing.T, namespace, clusterName string) string {
	t.Helper()
	const root = "/tmp/oauth-identity/"
	const probe = `xquery version "1.0-ml";
xdmp:set-response-content-type("text/plain"),
fn:concat("oauth-user:", xdmp:get-current-user())`
	for node := 0; node < 2; node++ {
		testutil.ExecuteInPod(t, namespace, fmt.Sprintf("%s-%d", clusterName, node), "marklogic-server",
			"sh", "-c", `mkdir -p "$1" && printf '%s' "$2" > "$1/identity.xqy"`, "install-oauth-probe", root, probe)
	}
	return root
}
