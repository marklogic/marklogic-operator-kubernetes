// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package oauthclient

import "testing"

func TestBuildCreatesTrustedCookieJarClient(t *testing.T) {
	pod, err := Build(Config{Namespace: "oauth-test", CASecretName: "test-ca"})
	if err != nil {
		t.Fatalf("Build returned an error: %v", err)
	}
	container := pod.Spec.Containers[0]
	if container.Image != DefaultImage {
		t.Fatalf("image = %q, want %q", container.Image, DefaultImage)
	}
	if len(container.Command) != 2 || container.Command[0] != "sleep" || container.Command[1] != "infinity" {
		t.Fatalf("command = %#v, want an idle sleep command", container.Command)
	}
	if container.Env[0].Name != "CURL_CA_BUNDLE" || container.Env[0].Value != CAPath {
		t.Fatalf("CURL_CA_BUNDLE = %#v, want %q", container.Env, CAPath)
	}
	if len(container.VolumeMounts) != 2 || container.VolumeMounts[1].MountPath != "/work" {
		t.Fatalf("volume mounts = %#v, want CA and cookie work volumes", container.VolumeMounts)
	}
}

func TestBuildRequiresNamespaceAndCASecret(t *testing.T) {
	if _, err := Build(Config{CASecretName: "test-ca"}); err == nil {
		t.Fatal("Build accepted an empty namespace")
	}
	if _, err := Build(Config{Namespace: "oauth-test"}); err == nil {
		t.Fatal("Build accepted an empty CA Secret name")
	}
}
