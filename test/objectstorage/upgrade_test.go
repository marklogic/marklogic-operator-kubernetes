// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package objectstorage

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	ml "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	testutils "github.com/marklogic/marklogic-operator-kubernetes/test/utils"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestUpgradeOptionsRequireExplicitOptInAndScope(t *testing.T) {
	t.Setenv("E2E_OBJECT_STORAGE_UPGRADE_RUN", "")
	if _, enabled, err := UpgradeOptionsFromEnv("namespace"); enabled || err != nil {
		t.Fatal("unconfigured package must not enable cluster work")
	}
	t.Setenv("E2E_OBJECT_STORAGE_UPGRADE_RUN", "true")
	t.Setenv("E2E_OBJECT_STORAGE_UPGRADE_SCOPE", "")
	if _, _, err := UpgradeOptionsFromEnv("namespace"); err == nil {
		t.Fatal("missing scope must fail")
	}
	t.Setenv("E2E_OBJECT_STORAGE_UPGRADE_SCOPE", "cluster")
	if _, enabled, err := UpgradeOptionsFromEnv("namespace"); enabled || err != nil {
		t.Fatal("unselected scope must not contact a cluster")
	}
	opts := UpgradeOptions{Scope: "namespace", Context: "dedicated", SourceDir: "/source", TargetDir: "/candidate", SourceImage: "localhost:5000/operator:old", TargetImage: "localhost:5000/operator:new", MarkLogicImage: "marklogic:12.0.3", FixturePath: "/private/fixture.json"}
	if err := opts.validate(); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*UpgradeOptions){func(o *UpgradeOptions) { o.Context = "" }, func(o *UpgradeOptions) { o.SourceDir = o.TargetDir }, func(o *UpgradeOptions) { o.SourceImage = o.TargetImage }, func(o *UpgradeOptions) { o.TargetImage = "untagged" }, func(o *UpgradeOptions) { o.FixturePath = "relative" }} {
		invalid := opts
		change(&invalid)
		if invalid.validate() == nil {
			t.Fatal("unsafe or ambiguous upgrade options accepted")
		}
	}
}

func TestUpgradeTransitionRequiresQuiescentSchemaWindow(t *testing.T) {
	for _, fail := range []string{"", "stop", "schema", "roundtrip", "install-held", "verify-held"} {
		t.Run("failure-"+fail, func(t *testing.T) {
			var steps []string
			step := func(name string) func() error {
				return func() error {
					steps = append(steps, name)
					if name == fail {
						return errors.New("step failed")
					}
					return nil
				}
			}
			transition := upgradeTransition{stop: step("stop"), schema: step("schema"), validateSchema: step("roundtrip"), installStopped: step("install-held"), validateStopped: step("verify-held"), start: step("start")}
			err := transition.run()
			expected := []string{"stop", "schema", "roundtrip", "install-held", "verify-held", "start"}
			if fail != "" {
				expected = expected[:slices.Index(expected, fail)+1]
			}
			if !reflect.DeepEqual(steps, expected) || (err != nil) != (fail != "") {
				t.Fatalf("transition failed to keep controller stopped on failure: steps=%v", steps)
			}
		})
	}
}

func TestUpgradeHelmCommandsUseExplicitArtifactsContextAndStoppedCandidate(t *testing.T) {
	u := &upgradeInstaller{opts: UpgradeOptions{Scope: "namespace", Context: "dedicated-context"}, namespace: "os-upgrade-test", release: "os-upgrade-test", owned: map[string]*unstructured.Unstructured{}}
	var calls [][]string
	u.run = func(_ context.Context, name string, args []string, input []byte) ([]byte, error) {
		if name != "helm" || input != nil {
			t.Fatal("namespace installation must use Helm without credential stdin")
		}
		calls = append(calls, slices.Clone(args))
		return nil, nil
	}
	if err := u.install(context.Background(), "/source checkout", "registry:5000/operator:old", 1, nil); err != nil {
		t.Fatal(err)
	}
	if err := u.install(context.Background(), "/candidate checkout", "registry:5000/operator:new", 0, nil); err != nil {
		t.Fatal(err)
	}
	for i, args := range calls {
		root, tag, replicas := "/source checkout", "old", "1"
		if i == 1 {
			root, tag, replicas = "/candidate checkout", "new", "0"
		}
		for _, value := range []string{"upgrade", "--install", "--kube-context", "dedicated-context", filepath.Join(root, "charts", "marklogic-operator-kubernetes"), "controllerManager.manager.image.repository=registry:5000/operator", "controllerManager.manager.image.tag=" + tag, "controllerManager.replicas=" + replicas, "scope.type=namespace", "scope.watchNamespaces=os-upgrade-test", "metrics.secure=false"} {
			if !slices.Contains(args, value) {
				t.Fatalf("installation command missing required control %q", value)
			}
		}
	}
}

func TestUpgradeCandidateTemplateUsesUpgradeBranch(t *testing.T) {
	u := &upgradeInstaller{opts: UpgradeOptions{Context: "offline"}, namespace: "ns", release: "release"}
	if !slices.Contains(u.helmArgs("template", "/target", "operator:new", 0), "--is-upgrade") {
		t.Fatal("candidate template must render the same upgrade branch as the actual Helm operation")
	}
	if slices.Contains(u.helmArgs("template", "/source", "operator:old", 1), "--is-upgrade") {
		t.Fatal("source template must retain install semantics")
	}
}

func TestUpgradeRequiresDifferentRuntimeImagesDespiteDistinctTags(t *testing.T) {
	for _, tc := range []struct {
		name              string
		source, candidate []string
		fail              bool
	}{
		{"different-tags-same-runtime-id", []string{"sha256:same"}, []string{"sha256:same"}, true},
		{"different-repository-aliases-same-content", []string{"docker-pullable://source/operator@sha256:same"}, []string{"docker-pullable://candidate/operator@sha256:same"}, true},
		{"different-runtime-wrapper-same-content", []string{"containerd://sha256:same"}, []string{"candidate/operator@sha256:same"}, true},
		{"mixed-candidate-still-runs-source", []string{"sha256:source"}, []string{"sha256:candidate", "sha256:source"}, true},
		{"different-running-content", []string{"source/operator@sha256:source"}, []string{"candidate/operator@sha256:candidate"}, false},
		{"missing-source-evidence", nil, []string{"sha256:candidate"}, true},
		{"empty-candidate-identity", []string{"sha256:source"}, []string{""}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := distinctUpgradeImageIDs(tc.source, tc.candidate); (err != nil) != tc.fail {
				t.Fatal("runtime image identity check did not distinguish actual controller versions")
			}
		})
	}
}

func TestUpgradeLegacyRecoveryKeepsPublishedFingerprintAndCurrentMarkers(t *testing.T) {
	secret := testutils.SecretMeta{Name: "aws", UID: "secret-uid", ResourceVersion: "7"}
	eligible := true
	before := &ml.ObjectStorageProviderStatus{Phase: ml.ObjectStoragePhaseApplied, AuthType: "secret", AppliedFingerprint: "opaque-controller-published-marker", LastAppliedTime: &metav1.Time{Time: time.Unix(50, 0)}, AppliedSecret: &ml.ObjectStorageSecretRef{Name: secret.Name, UID: secret.UID, ResourceVersion: secret.ResourceVersion}, DetachEligible: &eligible, ObservedGeneration: 3, ObservedSecret: &ml.ObjectStorageSecretRef{Name: secret.Name, UID: secret.UID, ResourceVersion: secret.ResourceVersion}}
	cluster := &ml.MarklogicCluster{ObjectMeta: metav1.ObjectMeta{Generation: 3}, Spec: ml.MarklogicClusterSpec{ObjectStorage: &ml.ObjectStorageConfig{AWS: &ml.AWSObjectStorage{SecretName: secret.Name}}}, Status: ml.MarklogicClusterStatus{ObjectStorage: &ml.ObjectStorageStatus{AWS: before.DeepCopy()}}}
	if ready, _ := testutils.ObjectStorageReady(cluster, testutils.ObjectStorageAWS, secret); !ready {
		t.Fatal("test baseline must be fresh before checkpoint truncation")
	}
	cluster.Status.ObjectStorage.AWS = legacyUpgradeEntry(before, cluster.Generation, secret)
	if err := legacyUpgradeNotReady(cluster, testutils.ObjectStorageAWS, secret, before.AppliedFingerprint); err != nil {
		t.Fatal(err)
	}
	if cluster.Status.ObjectStorage.AWS.AppliedFingerprint != before.AppliedFingerprint || before.AppliedSecret == nil || before.DetachEligible == nil {
		t.Fatal("checkpoint truncation changed the opaque fingerprint or mutated the original success")
	}
	for _, change := range []func(*ml.MarklogicCluster){func(c *ml.MarklogicCluster) { c.Status.ObjectStorage.AWS.ObservedGeneration-- }, func(c *ml.MarklogicCluster) { c.Status.ObjectStorage.AWS.ObservedSecret.ResourceVersion = "stale" }, func(c *ml.MarklogicCluster) { c.Spec.ObjectStorage = nil }, func(c *ml.MarklogicCluster) { c.Spec.ObjectStorage.AWS.SecretName = "different" }, func(c *ml.MarklogicCluster) { c.Status.ObjectStorage.AWS.AppliedFingerprint = "different" }} {
		invalid := cluster.DeepCopy()
		change(invalid)
		if legacyUpgradeNotReady(invalid, testutils.ObjectStorageAWS, secret, before.AppliedFingerprint) == nil {
			t.Fatal("unrelated staleness or changed material must not satisfy the unchanged-material legacy recovery precondition")
		}
	}
}

func TestUpgradeAppliedContinuationAllowsOnlyCheckpointedEncodingChange(t *testing.T) {
	before := &ml.ObjectStorageProviderStatus{AuthType: "secret", AppliedFingerprint: "opaque-published-source", LastAppliedTime: &metav1.Time{Time: time.Unix(10, 0)}, AppliedSecret: &ml.ObjectStorageSecretRef{Name: "aws", UID: "uid", ResourceVersion: "7"}, ObservedSecret: &ml.ObjectStorageSecretRef{Name: "aws", UID: "uid", ResourceVersion: "7"}}
	if err := upgradeAppliedContinuation(before, before.DeepCopy()); err != nil {
		t.Fatal(err)
	}
	migrated := before.DeepCopy()
	migrated.AppliedFingerprint = "opaque-published-candidate"
	migrated.LastAppliedTime = &metav1.Time{Time: time.Unix(20, 0)}
	if err := upgradeAppliedContinuation(before, migrated); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*ml.ObjectStorageProviderStatus){func(e *ml.ObjectStorageProviderStatus) { e.AppliedFingerprint = before.AppliedFingerprint }, func(e *ml.ObjectStorageProviderStatus) { e.LastAppliedTime = before.LastAppliedTime.DeepCopy() }, func(e *ml.ObjectStorageProviderStatus) { e.AppliedSecret.UID = "replaced" }, func(e *ml.ObjectStorageProviderStatus) { e.ObservedSecret.ResourceVersion = "8" }} {
		invalid := migrated.DeepCopy()
		change(invalid)
		if upgradeAppliedContinuation(before, invalid) == nil {
			t.Fatal("upgrade accepted an uncheckpointed migration or unnecessary unchanged-material write")
		}
	}
}

func testUpgradeCRD(uid string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition", "metadata": map[string]any{"name": "marklogicclusters.marklogic.progress.com", "uid": uid}, "spec": map[string]any{"group": "marklogic.progress.com"}}}
	return obj
}

func TestUpgradeSchemaForceApplyRequiresOwnedUID(t *testing.T) {
	for _, currentUID := range []string{"owned", "replacement"} {
		t.Run(currentUID, func(t *testing.T) {
			owned := testUpgradeCRD("owned")
			current := testUpgradeCRD(currentUID)
			target := testUpgradeCRD("")
			u := &upgradeInstaller{opts: UpgradeOptions{Context: "dedicated-context"}, release: "os-upgrade-test", client: fake.NewClientBuilder().WithObjects(current).Build(), owned: map[string]*unstructured.Unstructured{upgradeObjectKey(owned): owned}}
			calls := 0
			u.run = func(_ context.Context, name string, args []string, input []byte) ([]byte, error) {
				calls++
				if name != "kubectl" || !slices.Contains(args, "--force-conflicts") || !slices.Contains(args, "dedicated-context") || !slices.Contains(args, "-") || !strings.Contains(string(input), "marklogicclusters.marklogic.progress.com") {
					t.Fatal("schema command lost ownership upgrade controls or stdin manifest")
				}
				return nil, nil
			}
			err := u.applyOwnedSchemas(context.Background(), []*unstructured.Unstructured{target})
			if currentUID == "owned" {
				if err != nil || calls != 1 {
					t.Fatal("owned schema was not upgraded")
				}
			} else if err == nil || calls != 0 {
				t.Fatal("replacement CRD was overwritten")
			}
		})
	}
}

func TestUpgradePreflightRefusesExistingSchema(t *testing.T) {
	obj := testUpgradeCRD("existing")
	u := &upgradeInstaller{client: fake.NewClientBuilder().WithObjects(obj).Build()}
	if err := u.preflight(context.Background(), []*unstructured.Unstructured{obj}); err == nil {
		t.Fatal("upgrade must not adopt an existing cluster schema")
	}
}

func TestUpgradeCleanupWaitsForBothOwnedNamespacesAndRetainsForeignResources(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := ml.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	for _, ns := range []string{"", "operator-ns", "workload-ns", "foreign-ns"} {
		t.Run("remaining-"+ns, func(t *testing.T) {
			var objects []client.Object
			if ns != "" {
				objects = append(objects, &ml.MarklogicGroup{ObjectMeta: metav1.ObjectMeta{Name: "remaining-group", Namespace: ns}})
			}
			u := &upgradeInstaller{namespace: "operator-ns", workloadNamespace: "workload-ns", client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()}
			removed, err := u.markLogicResourcesRemoved(context.Background())
			if removed != (ns == "") || (err != nil) != (ns == "foreign-ns") {
				t.Fatal("cleanup incorrectly treated cross-namespace owned work or foreign resources as removable")
			}
		})
	}
}

func TestUpgradeCommandFailuresSuppressBothOutputStreams(t *testing.T) {
	_, err := runUpgradeCommand(context.Background(), "sh", []string{"-c", "printf sensitive-fixture-test; printf sensitive-fixture-test >&2; exit 1"}, nil)
	if err == nil || strings.Contains(err.Error(), "sensitive-fixture-test") {
		t.Fatal("command failure exposed diagnostic output")
	}
}

func TestUpgradeSchemaDetectionRequiresCompleteProviderCheckpoints(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "config", "crd", "bases", "marklogic.progress.com_marklogicclusters.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	objects, err := decodeUpgradeManifests(data)
	if err != nil {
		t.Fatal(err)
	}
	if !sourceHasDurableCheckpoint(objects) {
		t.Fatal("current CRD must expose both complete provider checkpoints")
	}
	versions, _, _ := unstructured.NestedSlice(objects[0].Object, "spec", "versions")
	version := versions[0].(map[string]any)
	unstructured.RemoveNestedField(version, "schema", "openAPIV3Schema", "properties", "status", "properties", "objectStorage", "properties", "azure", "properties", "detachEligible")
	_ = unstructured.SetNestedSlice(objects[0].Object, versions, "spec", "versions")
	if sourceHasDurableCheckpoint(objects) {
		t.Fatal("legacy one-provider/incomplete schema must not claim historical durable success")
	}
}

func TestUpgradeClusterRenderUsesCandidatePackageWithNoController(t *testing.T) {
	u := &upgradeInstaller{opts: UpgradeOptions{Scope: "cluster", Context: "dedicated-context"}, namespace: "os-upgrade-test", release: "os-upgrade-test"}
	u.run = func(_ context.Context, name string, args []string, input []byte) ([]byte, error) {
		if name != "kubectl" || !reflect.DeepEqual(args, []string{"--context", "dedicated-context", "kustomize", "/candidate/config/default"}) || input != nil {
			t.Fatal("cluster render did not select explicit context and candidate artifacts")
		}
		return []byte(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: marklogic-operator-controller-manager
  namespace: old-system
spec:
  replicas: 1
  template:
    spec:
      containers:
      - name: manager
        image: old:tag
---
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: marklogicclusters.marklogic.progress.com
---
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: marklogicgroups.marklogic.progress.com
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: manager-binding
subjects:
- kind: ServiceAccount
  name: manager
  namespace: old-system
`), nil
	}
	objects, err := u.render(context.Background(), "/candidate", "operator:new", 0)
	if err != nil {
		t.Fatal(err)
	}
	deployment := objects[0]
	replicas, _, _ := unstructured.NestedInt64(deployment.Object, "spec", "replicas")
	containers, _, _ := unstructured.NestedSlice(deployment.Object, "spec", "template", "spec", "containers")
	if replicas != 0 || deployment.GetNamespace() != u.namespace || containers[0].(map[string]any)["image"] != "operator:new" {
		t.Fatal("candidate could start before schema validation or run the wrong image")
	}
	subjects, _, _ := unstructured.NestedSlice(objects[3].Object, "subjects")
	if subjects[0].(map[string]any)["namespace"] != u.namespace {
		t.Fatal("Kustomize role binding still points to original namespace")
	}
}

func TestUpgradeCurrentArtifactsRenderWithoutCluster(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	for _, scope := range []string{"namespace", "cluster"} {
		t.Run(scope, func(t *testing.T) {
			tool := "helm"
			if scope == "cluster" {
				tool = "kubectl"
			}
			if _, err := exec.LookPath(tool); err != nil {
				t.Skip("local render tool unavailable")
			}
			u := &upgradeInstaller{opts: UpgradeOptions{Scope: scope, Context: "offline-no-cluster"}, namespace: "os-upgrade-render-test", release: "os-upgrade-render-test", run: runUpgradeCommand}
			objects, err := u.render(context.Background(), root, "example.invalid/operator:candidate", 0)
			if err != nil {
				t.Fatal(err)
			}
			if !sourceHasDurableCheckpoint(objects) {
				t.Fatal("rendered candidate is missing the checkpoint schema")
			}
		})
	}
}
