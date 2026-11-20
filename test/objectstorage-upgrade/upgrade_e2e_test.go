// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package objectstorageupgrade

import (
	"context"
	"testing"

	"github.com/marklogic/marklogic-operator-kubernetes/test/objectstorage"
)

// There is intentionally no TestMain: compilation and unconfigured go test runs
// must not install an operator or contact a cluster.
func TestUpgradeNamespaceScope(t *testing.T) { run(t, "namespace") }
func TestUpgradeClusterScope(t *testing.T)   { run(t, "cluster") }

func run(t *testing.T, scope string) {
	t.Helper()
	opts, enabled, err := objectstorage.UpgradeOptionsFromEnv(scope)
	if err != nil {
		t.Fatal(err)
	}
	if !enabled {
		t.Skip("set E2E_OBJECT_STORAGE_UPGRADE_RUN=true and an explicit scope to enable this dedicated-cluster test")
	}
	objectstorage.RunUpgrade(context.Background(), t, opts)
}
