// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package objectstorage

import (
	"fmt"
	"strings"
	"testing"
)

const (
	testSalt      = "8f2a1c4e-0b6d-4a3f-9c1e-2d7b5a9f3e10"
	testAccessKey = "AKIAIOSFODNN7EXAMPLE"
	testSecretKey = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
)

func awsMaterial() AWSMaterial {
	return AWSMaterial{AccessKey: testAccessKey, SecretKey: testSecretKey}
}

func TestFingerprintIsDeterministic(t *testing.T) {
	t.Parallel()

	first := awsMaterial().Fingerprint(testSalt)
	for i := 0; i < 100; i++ {
		// Repeated over a map, whose iteration order Go randomises per range.
		if got := awsMaterial().Fingerprint(testSalt); got != first {
			t.Fatalf("fingerprint is not stable across calls: %q != %q", got, first)
		}
	}
}

func TestFingerprintChangesWhenAnyFieldChanges(t *testing.T) {
	t.Parallel()

	baseline := awsMaterial().Fingerprint(testSalt)

	awsCases := map[string]AWSMaterial{
		"accessKey changed":  {AccessKey: "AKIAIOSFODNN7CHANGED", SecretKey: testSecretKey},
		"secretKey changed":  {AccessKey: testAccessKey, SecretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYCHANGED"},
		"accessKey cleared":  {AccessKey: "", SecretKey: testSecretKey},
		"secretKey cleared":  {AccessKey: testAccessKey, SecretKey: ""},
		"sessionToken added": {AccessKey: testAccessKey, SecretKey: testSecretKey, SessionToken: "testSessionToken123"},
	}

	for name, material := range awsCases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := material.Fingerprint(testSalt); got == baseline {
				t.Fatalf("fingerprint did not change for %q", name)
			}
		})
	}

	azureBaseline := AzureMaterial{StorageAccount: "mystorageacct", StorageKey: "dGVzdGtleQ=="}.Fingerprint(testSalt)
	azureCases := map[string]AzureMaterial{
		"storageAccount changed": {StorageAccount: "otherstorageacct", StorageKey: "dGVzdGtleQ=="},
		"storageKey changed":     {StorageAccount: "mystorageacct", StorageKey: "b3RoZXJrZXk="},
	}

	for name, material := range azureCases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := material.Fingerprint(testSalt); got == azureBaseline {
				t.Fatalf("fingerprint did not change for %q", name)
			}
		})
	}
}

// Two clusters holding identical credentials must not publish identical
// fingerprints, otherwise status discloses that they share credentials.
func TestFingerprintIsSaltScoped(t *testing.T) {
	t.Parallel()

	clusterA := awsMaterial().Fingerprint("uid-of-cluster-a")
	clusterB := awsMaterial().Fingerprint("uid-of-cluster-b")

	if clusterA == clusterB {
		t.Fatal("identical material produced the same fingerprint under different salts")
	}
}

func TestFingerprintIsProviderScoped(t *testing.T) {
	t.Parallel()

	fields := map[string]string{"key": "same-value"}

	if Fingerprint(testSalt, ProviderAWS, fields) == Fingerprint(testSalt, ProviderAzure, fields) {
		t.Fatal("identical fields produced the same fingerprint for different providers")
	}
}

// Guards the length-prefixed encoding: naive concatenation would collide here.
func TestFingerprintIsInjectiveAcrossFieldBoundaries(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		a    map[string]string
		b    map[string]string
	}{
		{
			name: "value boundary shifted",
			a:    map[string]string{"accessKey": "AB", "secretKey": "C"},
			b:    map[string]string{"accessKey": "A", "secretKey": "BC"},
		},
		{
			name: "key and value boundary shifted",
			a:    map[string]string{"ab": "c"},
			b:    map[string]string{"a": "bc"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if Fingerprint(testSalt, ProviderAWS, test.a) == Fingerprint(testSalt, ProviderAWS, test.b) {
				t.Fatalf("distinct field sets collided: %v vs %v", test.a, test.b)
			}
		})
	}
}

func TestFingerprintNeverContainsMaterial(t *testing.T) {
	t.Parallel()

	aws := awsMaterial().Fingerprint(testSalt)
	azure := AzureMaterial{StorageAccount: "mystorageacct", StorageKey: "dGVzdGtleQ=="}.Fingerprint(testSalt)

	for _, secret := range []string{testAccessKey, testSecretKey, "mystorageacct", "dGVzdGtleQ==", testSalt} {
		for _, digest := range []string{aws, azure} {
			if strings.Contains(digest, secret) {
				t.Fatalf("fingerprint %q leaked %q", digest, secret)
			}
		}
	}
}

// The material types are passed to loggers and error wrappers, so every fmt
// verb that could render them must yield the redaction placeholder instead.
func TestMaterialRedactsWhenFormatted(t *testing.T) {
	t.Parallel()

	aws := awsMaterial()
	azure := AzureMaterial{StorageAccount: "mystorageacct", StorageKey: "dGVzdGtleQ=="}

	rendered := []string{
		fmt.Sprintf("%v", aws),
		fmt.Sprintf("%+v", aws),
		fmt.Sprintf("%s", aws),
		fmt.Sprintf("%v", &aws),
		aws.String(),
		fmt.Sprintf("%v", azure),
		fmt.Sprintf("%+v", azure),
		fmt.Sprintf("%s", azure),
		fmt.Sprintf("%v", &azure),
		azure.String(),
	}

	for _, out := range rendered {
		if out != redacted {
			t.Fatalf("expected %q, got %q", redacted, out)
		}
		for _, secret := range []string{testAccessKey, testSecretKey, "mystorageacct", "dGVzdGtleQ=="} {
			if strings.Contains(out, secret) {
				t.Fatalf("formatted material leaked %q: %q", secret, out)
			}
		}
	}
}
