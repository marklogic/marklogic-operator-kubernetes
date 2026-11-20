// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package e2ehelm

import (
	"context"
	"strings"
	"testing"

	"github.com/marklogic/marklogic-operator-kubernetes/test/objectstorage"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

const objectStorageNamespace = "ml-ns-object-storage"

func TestObjectStorageNamespaceScoped(t *testing.T) {
	trackTest(t)

	feature := features.New("Object Storage Credentials — Namespace-Scoped").
		WithLabel("type", "object-storage").
		Assess("Credential resolution and lifecycle", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			watched := false
			for _, ns := range strings.Split(watchedNamespaces, ",") {
				if strings.TrimSpace(ns) == objectStorageNamespace {
					watched = true
					break
				}
			}
			if !watched {
				t.Fatalf("E2E_WATCHED_NAMESPACES must include %s", objectStorageNamespace)
			}
			objectstorage.Run(ctx, t, c, objectstorage.Options{
				Namespace:         objectStorageNamespace,
				MarkLogicImage:    marklogicImage,
				OperatorNamespace: helmNS,
				OperatorImage:     dockerImage,
			})
			return ctx
		})

	testEnv.Test(t, feature.Feature())
}
