// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package e2e

import (
	"context"
	"testing"

	"github.com/marklogic/marklogic-operator-kubernetes/test/objectstorage"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

const objectStorageNamespace = "ml-object-storage"

func TestObjectStorageClusterScoped(t *testing.T) {
	trackTest(t)
	// Restart coverage deliberately stops the shared controller. Keep this top-level test
	// sequential so Go does not run other parallel E2E cases during the outage.

	feature := features.New("Object Storage Credentials — Cluster-Scoped").
		WithLabel("type", "object-storage").
		Assess("Credential resolution and lifecycle", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			objectstorage.Run(ctx, t, c, objectstorage.Options{
				Namespace:         objectStorageNamespace,
				MarkLogicImage:    marklogicImage,
				OperatorNamespace: namespace,
				OperatorImage:     dockerImage,
			})
			return ctx
		})

	testEnv.Test(t, feature.Feature())
}
