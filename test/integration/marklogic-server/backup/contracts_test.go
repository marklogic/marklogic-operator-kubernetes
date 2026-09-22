// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package backup

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	"github.com/marklogic/marklogic-operator-kubernetes/pkg/objectstorage"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func environment(key string) string {
	switch key {
	case "INTEGRATION_BACKUP_S3_URI":
		return "s3://example-bucket/integration/backup"
	case "AWS_SESSION_TOKEN":
		return "test-session-token"
	default:
		return "test-only"
	}
}

func TestBackupConfigurationRequiresDedicatedPrefixAndCredentials(t *testing.T) {
	c, err := configFromEnvironment(environment)
	if err != nil {
		t.Fatal(err)
	}
	if c.AWS.SessionToken != "test-session-token" {
		t.Fatal("STS token lost")
	}
	for _, uri := range []string{"", "s3://example-bucket", "s3://example-bucket/", "https://example-bucket/test", "s3://user:password@example-bucket/test", "s3://example-bucket/test?token=secret", "s3://example-bucket/test?", "s3://example-bucket/test#", "s3://example-bucket/test#fragment", "s3://example-bucket/test/../other", "s3://example-bucket/test//other", "s3://example-bucket/%2e%2e/test", "s3://example-bucket/path with spaces", "s3://example-bucket:9000/test"} {
		if _, err := configFromEnvironment(func(key string) string {
			if key == "INTEGRATION_BACKUP_S3_URI" {
				return uri
			}
			return environment(key)
		}); err == nil {
			t.Errorf("accepted unsafe destination %q", uri)
		}
	}
	for _, key := range []string{"INTEGRATION_CONTEXT", "INTEGRATION_OPERATOR_NAMESPACE", "INTEGRATION_OPERATOR_DEPLOYMENT", "MARKLOGIC_IMAGE", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"} {
		if _, err := configFromEnvironment(func(k string) string {
			if k == key {
				return " "
			}
			return environment(k)
		}); err == nil {
			t.Errorf("accepted missing %s", key)
		}
	}
	if _, err := configFromEnvironment(func(key string) string {
		if key == "AWS_SESSION_TOKEN" {
			return ""
		}
		return environment(key)
	}); err != nil {
		t.Fatal("static credentials should not require STS token")
	}
}

func TestBackupConfigurationNormalizesCredentialFingerprint(t *testing.T) {
	for _, token := range []string{"test-session-token", ""} {
		c, err := configFromEnvironment(func(key string) string {
			switch key {
			case "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY":
				return " \t" + environment(key) + "\n"
			case "AWS_SESSION_TOKEN":
				return " \t" + token + "\n"
			default:
				return environment(key)
			}
		})
		if err != nil {
			t.Fatal(err)
		}
		expected := objectstorage.AWSMaterial{AccessKey: "test-only", SecretKey: "test-only", SessionToken: token}
		if c.AWS.Fingerprint("owned-uid") != expected.Fingerprint("owned-uid") {
			t.Fatal("credential fingerprint does not match the operator's normalized material")
		}
	}
}

func TestBackupFixtureUsesOperatorCredentialsWithoutOAuth(t *testing.T) {
	c, err := configFromEnvironment(environment)
	if err != nil {
		t.Fatal(err)
	}
	objects, err := buildObjects(c, "owned-namespace", "test-storage", "test-password")
	if err != nil {
		t.Fatal(err)
	}
	var cluster *marklogicv1.MarklogicCluster
	var pod *corev1.Pod
	var credentials *corev1.Secret
	for _, object := range objects {
		metadata, err := meta.Accessor(object)
		if err != nil {
			t.Fatal(err)
		}
		if metadata.GetNamespace() != "owned-namespace" {
			t.Fatal("resource escaped run namespace")
		}
		switch typed := object.(type) {
		case *marklogicv1.MarklogicCluster:
			cluster = typed
		case *corev1.Pod:
			if pod != nil {
				t.Fatal("unexpected second client")
			}
			pod = typed
		case *corev1.Secret:
			if typed.Name == credentialSecret {
				credentials = typed
			}
		default:
			t.Fatalf("unexpected OAuth/infrastructure dependency %T", object)
		}
	}
	if cluster == nil || pod == nil || credentials == nil {
		t.Fatal("missing required fixture resource")
	}
	if cluster.Spec.ObjectStorage.AWS.SecretName != credentials.Name || cluster.Spec.ObjectStorage.AWS.AuthType != marklogicv1.ObjectStorageAuthSecret || cluster.Spec.ObjectStorage.Azure != nil {
		t.Fatal("wrong object-storage configuration")
	}
	if credentials.StringData["sessionToken"] != c.AWS.SessionToken || credentials.StringData["secretKey"] != c.AWS.SecretKey {
		t.Fatal("credential material lost")
	}
	if cluster.Spec.HAProxy.Enabled {
		t.Fatal("backup must target the coordinator directly")
	}
	if cluster.Spec.Persistence.StorageClassName != "test-storage" {
		t.Fatal("preflight storage class ignored")
	}
	serialized, err := json.Marshal(pod)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"test-password", c.AWS.SessionToken} {
		if strings.Contains(string(serialized), secret) {
			t.Fatal("client pod spec embeds credential values")
		}
	}
	if len(pod.Spec.Containers) != 1 || pod.Spec.Containers[0].Image != "curlimages/curl:8.12.1" {
		t.Fatal("expected only the pinned curl client")
	}
}

func TestAppliedCredentialsMustMatchMaterialAndClusterUID(t *testing.T) {
	c := config{AWS: objectstorage.AWSMaterial{AccessKey: "access", SecretKey: "secret", SessionToken: "token"}}
	cluster := marklogicv1.MarklogicCluster{ObjectMeta: metav1.ObjectMeta{UID: "owned-uid"}, Spec: marklogicv1.MarklogicClusterSpec{ObjectStorage: &marklogicv1.ObjectStorageConfig{AWS: &marklogicv1.AWSObjectStorage{SecretName: credentialSecret}}}}
	if applied, err := credentialsApplied(cluster, c); applied || err != nil {
		t.Fatal("missing status must wait")
	}
	cluster.Status.ObjectStorage = &marklogicv1.ObjectStorageStatus{AWS: &marklogicv1.ObjectStorageProviderStatus{Phase: marklogicv1.ObjectStoragePhaseApplied, AppliedFingerprint: c.AWS.Fingerprint("owned-uid")}}
	if applied, err := credentialsApplied(cluster, c); !applied || err != nil {
		t.Fatal("matching credentials rejected")
	}
	for _, mutate := range []func(*marklogicv1.MarklogicCluster){
		func(cluster *marklogicv1.MarklogicCluster) { cluster.UID = "other-uid" },
		func(cluster *marklogicv1.MarklogicCluster) {
			cluster.Status.ObjectStorage.AWS.AppliedFingerprint = "stale"
		},
		func(cluster *marklogicv1.MarklogicCluster) {
			cluster.Status.ObjectStorage.AWS.Phase = marklogicv1.ObjectStoragePhasePending
		},
		func(cluster *marklogicv1.MarklogicCluster) { cluster.Spec.ObjectStorage = nil },
		func(cluster *marklogicv1.MarklogicCluster) { cluster.Spec.ObjectStorage.AWS.SecretName = "unrelated" },
	} {
		changed := cluster.DeepCopy()
		mutate(changed)
		if applied, _ := credentialsApplied(*changed, c); applied {
			t.Fatal("stale, pruned, or foreign configuration passed")
		}
	}
	c.AWS.SessionToken = "rotated-token"
	if applied, _ := credentialsApplied(cluster, c); applied {
		t.Fatal("stale session token fingerprint passed")
	}
}

func evalResponse(body string) (string, string) {
	return "HTTP/1.1 401 Unauthorized\r\nWWW-Authenticate: Digest realm=example\r\n\r\nHTTP/1.1 200 OK\r\nContent-Type: multipart/mixed; boundary=example\r\n\r\n", "--example\r\nContent-Type: application/json\r\n\r\n" + body + "\r\n--example--\r\n"
}

func TestEvalResponseRejectsFalseSuccess(t *testing.T) {
	headers, body := evalResponse(`{"job":"18446744073709551614"}`)
	var result struct {
		Job string `json:"job"`
	}
	if err := decodeEval(headers, body, &result); err != nil || result.Job != "18446744073709551614" {
		t.Fatalf("lost large job ID: %v", err)
	}
	for _, tc := range []struct{ headers, body string }{
		{strings.ReplaceAll(headers, "200 OK", "500 Error"), body},
		{"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n\r\n", `{"job":"1"}`},
		{headers, "--example--\r\n"},
		{headers, strings.Replace(body, "--example--", "--example\r\nContent-Type: application/json\r\n\r\n{}\r\n--example--", 1)},
		{headers, strings.Replace(body, `{"job":"18446744073709551614"}`, `not-json`, 1)},
		{headers, strings.Replace(body, `{"job":"18446744073709551614"}`, `{} {}`, 1)},
	} {
		if err := decodeEval(tc.headers, tc.body, &result); err == nil {
			t.Fatal("invalid response accepted")
		}
	}
}

func TestBackupAndRestoreCompletionRequiresAllOwnedForests(t *testing.T) {
	runURI := "s3://example-bucket/test/run-123"
	expected := []forest{{ID: "1", Name: "Documents"}}
	valid := jobStatus{Status: "completed", Forests: []forest{{ID: "1", Name: "Documents", Status: "completed", BackupPath: runURI + "/timestamp"}}}
	if done, path, err := jobComplete(valid, expected, true, runURI); !done || err != nil || path != runURI+"/timestamp" {
		t.Fatalf("valid backup rejected: %v", err)
	}
	for _, state := range []string{"queued", "in-progress", "finishing", "failed", "canceled", "unknown", ""} {
		s := valid
		s.Status = state
		if done, _, _ := jobComplete(s, expected, true, runURI); done {
			t.Errorf("accepted %s", state)
		}
	}
	for _, forests := range [][]forest{nil, {{ID: "2", Status: "completed", BackupPath: runURI}}, {{ID: "1", Status: "failed", BackupPath: runURI}}, {{ID: "1", Status: "completed", BackupPath: "s3://example-bucket/test/other"}}} {
		s := valid
		s.Forests = forests
		if done, _, err := jobComplete(s, expected, true, runURI); done || err == nil {
			t.Fatal("bad forest or path accepted")
		}
	}
	restore := valid
	restore.Forests = []forest{{ID: "1", Status: "completed"}}
	if done, _, err := jobComplete(restore, expected, false, runURI); !done || err != nil {
		t.Fatalf("restore rejected: %v", err)
	}
	for _, path := range []string{runURI + "-other/backup", runURI + "/../other", runURI + "/%2e%2e/other", runURI + "/backup?token=secret", runURI + "/backup?", runURI + "/backup#", runURI + "/backup#fragment"} {
		if backupPathWithinRun(path, runURI) {
			t.Errorf("escaped path accepted: %s", path)
		}
	}
	// A successful overall job cannot mask duplicate or missing forests.
	duplicate := jobStatus{Status: "completed", Forests: []forest{valid.Forests[0], valid.Forests[0]}}
	if done, _, err := jobComplete(duplicate, append(expected, forest{ID: "2"}), true, runURI); done || err == nil {
		t.Fatal("duplicate forest accepted")
	}
}

func Example_configFromEnvironment() {
	c, _ := configFromEnvironment(environment)
	fmt.Println(c.BaseURI)
	// Output: s3://example-bucket/integration/backup
}
