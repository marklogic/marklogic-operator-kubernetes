// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package objectstorage

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	ml "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	testutils "github.com/marklogic/marklogic-operator-kubernetes/test/utils"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	yamlutil "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// UpgradeOptions selects two real installation artifacts and a disposable cluster.
// SourceDir and TargetDir are absolute checkout/artifact roots containing charts/ and config/.
// Both images must be explicit tagged images; these tests do not build or push them.
type UpgradeOptions struct {
	Scope, Context, SourceDir, TargetDir, SourceImage, TargetImage, MarkLogicImage, FixturePath string
}

// UpgradeOptionsFromEnv returns enabled=false unless this particular scope was explicitly selected.
// Merely compiling or running the package without opt-in never contacts Kubernetes.
func UpgradeOptionsFromEnv(scope string) (opts UpgradeOptions, enabled bool, err error) {
	if os.Getenv("E2E_OBJECT_STORAGE_UPGRADE_RUN") != "true" {
		return opts, false, nil
	}
	selected := os.Getenv("E2E_OBJECT_STORAGE_UPGRADE_SCOPE")
	if selected != "namespace" && selected != "cluster" {
		return opts, false, errors.New("E2E_OBJECT_STORAGE_UPGRADE_SCOPE must explicitly select namespace or cluster")
	}
	if selected != scope {
		return opts, false, nil
	}
	opts = UpgradeOptions{Scope: scope, Context: os.Getenv("E2E_OBJECT_STORAGE_UPGRADE_CONTEXT"),
		SourceDir: os.Getenv("E2E_OBJECT_STORAGE_UPGRADE_SOURCE_DIR"), TargetDir: os.Getenv("E2E_OBJECT_STORAGE_UPGRADE_TARGET_DIR"),
		SourceImage: os.Getenv("E2E_OBJECT_STORAGE_UPGRADE_SOURCE_IMAGE"), TargetImage: os.Getenv("E2E_OBJECT_STORAGE_UPGRADE_TARGET_IMAGE"),
		MarkLogicImage: os.Getenv("E2E_MARKLOGIC_IMAGE_VERSION"), FixturePath: os.Getenv("E2E_OBJECT_STORAGE_FIXTURE")}
	return opts, true, opts.validate()
}

func (o UpgradeOptions) validate() error {
	if o.Context == "" || o.MarkLogicImage == "" || o.FixturePath == "" ||
		(o.Scope != "namespace" && o.Scope != "cluster") {
		return errors.New("upgrade E2E requires an explicit scope, context, server image and credential fixture")
	}
	if !filepath.IsAbs(o.SourceDir) || !filepath.IsAbs(o.TargetDir) || !filepath.IsAbs(o.FixturePath) || filepath.Clean(o.SourceDir) == filepath.Clean(o.TargetDir) {
		return errors.New("upgrade E2E requires distinct absolute source/target artifact directories and an absolute fixture path")
	}
	for _, image := range []string{o.SourceImage, o.TargetImage} {
		if _, _, err := upgradeImageParts(image); err != nil {
			return err
		}
	}
	if o.SourceImage == o.TargetImage {
		return errors.New("upgrade E2E requires different explicit source and candidate images")
	}
	return nil
}

func upgradeImageParts(image string) (string, string, error) {
	colon := strings.LastIndex(image, ":")
	if colon <= strings.LastIndex(image, "/") || colon == len(image)-1 || strings.ContainsAny(image, "@ \t\r\n,") {
		return "", "", errors.New("upgrade images must be explicit repository:tag references")
	}
	return image[:colon], image[colon+1:], nil
}

type upgradeCommand func(context.Context, string, []string, []byte) ([]byte, error)

// Never return exec.ExitError, stdout or stderr in a failure: a command can print entire resources.
func runUpgradeCommand(ctx context.Context, name string, args []string, input []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = bytes.NewReader(input)
	var stdout bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, io.Discard
	if cmd.Run() != nil {
		return nil, errors.New("upgrade command failed (all command output suppressed)")
	}
	return stdout.Bytes(), nil
}

type upgradeInstaller struct {
	opts               UpgradeOptions
	namespace, release string
	workloadNamespace  string
	run                upgradeCommand
	client             client.Client
	owned              map[string]*unstructured.Unstructured
}

// Keep the quiescent schema window explicit and independently testable. Any
// validation or installation failure must prevent candidate startup.
type upgradeTransition struct {
	stop, schema, validateSchema, installStopped, validateStopped, start func() error
}

func (u upgradeTransition) run() error {
	for _, step := range []func() error{u.stop, u.schema, u.validateSchema, u.installStopped, u.validateStopped, u.start} {
		if err := step(); err != nil {
			return err
		}
	}
	return nil
}

func (u *upgradeInstaller) helmArgs(action, root, image string, replicas int64) []string {
	repository, tag, _ := upgradeImageParts(image)
	args := []string{action, u.release, filepath.Join(root, "charts", "marklogic-operator-kubernetes"),
		"--namespace", u.namespace, "--kube-context", u.opts.Context,
		"--set-string", "controllerManager.manager.image.repository=" + repository,
		"--set-string", "controllerManager.manager.image.tag=" + tag,
		"--set", fmt.Sprintf("controllerManager.replicas=%d", replicas),
		"--set-string", "scope.type=namespace", "--set-string", "scope.watchNamespaces=" + u.namespace,
		"--set", "metrics.secure=false"}
	if action == "upgrade" {
		args = append(args, "--install", "--timeout", "10m")
	} else if action == "template" && replicas == 0 {
		// The held candidate is an upgrade of an existing release. Render that same
		// branch of chart conditionals before validating its schema and controller.
		args = append(args, "--is-upgrade")
	}
	return args
}

func (u *upgradeInstaller) render(ctx context.Context, root, image string, replicas int64) ([]*unstructured.Unstructured, error) {
	var data []byte
	var err error
	if u.opts.Scope == "namespace" {
		data, err = u.run(ctx, "helm", u.helmArgs("template", root, image, replicas), nil)
	} else {
		data, err = u.run(ctx, "kubectl", []string{"--context", u.opts.Context, "kustomize", filepath.Join(root, "config", "default")}, nil)
	}
	if err != nil {
		return nil, err
	}
	objects, err := decodeUpgradeManifests(data)
	if err != nil {
		return nil, err
	}
	deployments, crds := 0, 0
	for _, obj := range objects {
		switch obj.GetKind() {
		case "CustomResourceDefinition":
			if obj.GetName() != "marklogicclusters.marklogic.progress.com" && obj.GetName() != "marklogicgroups.marklogic.progress.com" {
				return nil, errors.New("upgrade artifacts contain an unexpected CRD")
			}
			crds++
		case "Namespace":
			if u.opts.Scope == "namespace" && obj.GetName() != u.namespace {
				return nil, errors.New("chart creates an unexpected namespace")
			}
			obj.SetName(u.namespace)
		case "ClusterRole", "ClusterRoleBinding":
		case "Deployment", "Service", "ServiceAccount", "Role", "RoleBinding":
			if u.opts.Scope == "namespace" && obj.GetNamespace() != "" && obj.GetNamespace() != u.namespace {
				return nil, errors.New("namespace artifact targets an unexpected namespace")
			}
			obj.SetNamespace(u.namespace)
		default:
			return nil, errors.New("upgrade artifacts contain an unsupported resource kind (details suppressed)")
		}
		if obj.GetKind() == "RoleBinding" || obj.GetKind() == "ClusterRoleBinding" {
			subjects, _, _ := unstructured.NestedSlice(obj.Object, "subjects")
			for _, subject := range subjects {
				entry, ok := subject.(map[string]any)
				if ok && entry["kind"] == "ServiceAccount" {
					if u.opts.Scope == "namespace" && entry["namespace"] != u.namespace {
						return nil, errors.New("chart role binding targets an unexpected ServiceAccount namespace")
					}
					entry["namespace"] = u.namespace
				}
			}
			_ = unstructured.SetNestedSlice(obj.Object, subjects, "subjects")
		}
		if obj.GetKind() == "Deployment" {
			deployments++
			if obj.GetName() != "marklogic-operator-controller-manager" {
				return nil, errors.New("unexpected operator Deployment identity")
			}
			containers, _, _ := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "containers")
			found, scoped := false, false
			for _, value := range containers {
				container, ok := value.(map[string]any)
				if !ok || container["name"] != "manager" {
					continue
				}
				found = true
				if u.opts.Scope == "namespace" && container["image"] != image {
					return nil, errors.New("chart ignored the requested operator image")
				}
				container["image"] = image
				if env, ok := container["env"].([]any); ok {
					for _, item := range env {
						entry, ok := item.(map[string]any)
						if ok && entry["name"] == "WATCH_NAMESPACE" && entry["value"] == u.namespace {
							scoped = true
						}
						if ok && u.opts.Scope == "cluster" && entry["name"] == "WATCH_NAMESPACE" && (entry["valueFrom"] != nil || entry["value"] != "") {
							return nil, errors.New("cluster artifact restricts its watch namespace")
						}
					}
				}
			}
			if !found || (u.opts.Scope == "namespace" && !scoped) {
				return nil, errors.New("operator artifacts do not implement the requested scope")
			}
			_ = unstructured.SetNestedSlice(obj.Object, containers, "spec", "template", "spec", "containers")
			if u.opts.Scope == "namespace" {
				if actual, found, _ := unstructured.NestedInt64(obj.Object, "spec", "replicas"); !found || actual != replicas {
					return nil, errors.New("chart ignored the requested controller replica count")
				}
			}
			_ = unstructured.SetNestedField(obj.Object, replicas, "spec", "replicas")
		}
		labels := obj.GetLabels()
		if labels == nil {
			labels = map[string]string{}
		}
		if u.opts.Scope == "cluster" {
			labels["e2e-object-storage-upgrade"] = u.release
		}
		obj.SetLabels(labels)
	}
	if deployments != 1 || crds != 2 {
		return nil, errors.New("upgrade artifacts require one operator Deployment and both MarkLogic CRDs")
	}
	if u.opts.Scope == "namespace" {
		if err := validateNamespaceUpgradeRBAC(objects, u.namespace); err != nil {
			return nil, err
		}
	}
	return objects, nil
}

func decodeUpgradeManifests(data []byte) ([]*unstructured.Unstructured, error) {
	decoder := yamlutil.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	var result []*unstructured.Unstructured
	for {
		obj := &unstructured.Unstructured{}
		err := decoder.Decode(obj)
		if err == io.EOF {
			return result, nil
		}
		if err != nil {
			return nil, errors.New("cannot parse upgrade manifests (details suppressed)")
		}
		if len(obj.Object) == 0 {
			continue
		}
		if obj.GetKind() == "" || obj.GetName() == "" {
			return nil, errors.New("upgrade manifest lacks resource identity")
		}
		result = append(result, obj)
	}
}

func (u *upgradeInstaller) apply(ctx context.Context, objects []*unstructured.Unstructured) error {
	return u.applyManifests(ctx, objects, false)
}

func (u *upgradeInstaller) applyOwnedSchemas(ctx context.Context, objects []*unstructured.Unstructured) error {
	for _, obj := range objects {
		owned := u.owned[upgradeObjectKey(obj)]
		current := &unstructured.Unstructured{}
		current.SetGroupVersionKind(obj.GroupVersionKind())
		if obj.GetKind() != "CustomResourceDefinition" || owned == nil || u.client.Get(ctx, client.ObjectKeyFromObject(obj), current) != nil || current.GetUID() != owned.GetUID() {
			return errors.New("refusing schema replacement without matching installation ownership and UID")
		}
	}
	// Helm owns the source schema fields; force conflicts only after every CRD has
	// been verified as the exact object created by this run.
	return u.applyManifests(ctx, objects, true)
}

func (u *upgradeInstaller) applyManifests(ctx context.Context, objects []*unstructured.Unstructured, forceOwnedSchemas bool) error {
	var input bytes.Buffer
	for _, obj := range objects {
		data, err := json.Marshal(obj.Object)
		if err != nil {
			return errors.New("cannot encode upgrade manifests")
		}
		input.Write(data)
		input.WriteString("\n---\n")
	}
	args := []string{"--context", u.opts.Context, "apply", "--server-side", "--field-manager", u.release, "-f", "-"}
	if forceOwnedSchemas {
		args = append(args, "--force-conflicts")
	}
	_, err := u.run(ctx, "kubectl", args, input.Bytes())
	return err
}

func upgradeObjectKey(obj *unstructured.Unstructured) string {
	return obj.GroupVersionKind().String() + "/" + obj.GetNamespace() + "/" + obj.GetName()
}

func (u *upgradeInstaller) preflight(ctx context.Context, groups ...[]*unstructured.Unstructured) error {
	for _, group := range groups {
		for _, obj := range group {
			current := &unstructured.Unstructured{}
			current.SetGroupVersionKind(obj.GroupVersionKind())
			err := u.client.Get(ctx, client.ObjectKeyFromObject(obj), current)
			if !apierrors.IsNotFound(err) {
				return errors.New("upgrade requires a clean disposable cluster: an installation identity exists or cannot be checked")
			}
		}
	}
	return nil
}

func (u *upgradeInstaller) recordOwned(ctx context.Context, objects []*unstructured.Unstructured) {
	for _, obj := range objects {
		if obj.GetKind() == "Namespace" {
			continue
		} // Created and tracked separately.
		current := &unstructured.Unstructured{}
		current.SetGroupVersionKind(obj.GroupVersionKind())
		if u.client.Get(ctx, client.ObjectKeyFromObject(obj), current) != nil {
			continue
		}
		owned := current.GetLabels()["e2e-object-storage-upgrade"] == u.release
		if u.opts.Scope == "namespace" {
			owned = current.GetAnnotations()["meta.helm.sh/release-name"] == u.release && current.GetAnnotations()["meta.helm.sh/release-namespace"] == u.namespace
		}
		if owned {
			u.owned[upgradeObjectKey(current)] = current.DeepCopy()
		}
	}
}

func (u *upgradeInstaller) install(ctx context.Context, root, image string, replicas int64, objects []*unstructured.Unstructured) error {
	var err error
	if u.opts.Scope == "namespace" {
		_, err = u.run(ctx, "helm", u.helmArgs("upgrade", root, image, replicas), nil)
	} else {
		err = u.apply(ctx, objects)
	}
	// Also recover the owned part of a failed installation for UID-guarded cleanup.
	u.recordOwned(ctx, objects)
	return err
}

func sourceHasDurableCheckpoint(objects []*unstructured.Unstructured) bool {
	for _, obj := range objects {
		if obj.GetKind() != "CustomResourceDefinition" || obj.GetName() != "marklogicclusters.marklogic.progress.com" {
			continue
		}
		versions, _, _ := unstructured.NestedSlice(obj.Object, "spec", "versions")
		for _, value := range versions {
			version, ok := value.(map[string]any)
			if !ok || version["name"] != "v1" || version["served"] != true {
				continue
			}
			for _, provider := range []string{"aws", "azure"} {
				for _, field := range []string{"appliedSecret", "detachEligible", "observedGeneration", "observedSecret", "lastAppliedTime", "appliedFingerprint"} {
					_, found, _ := unstructured.NestedMap(version, "schema", "openAPIV3Schema", "properties", "status", "properties", "objectStorage", "properties", provider, "properties", field)
					if !found {
						return false
					}
				}
			}
			return true
		}
	}
	return false
}

// RunUpgrade performs a package and CRD upgrade, never an image-only simulation.
// A schema-only source is supported by explicitly synthetic legacy checkpoint injection;
// this is not evidence that an older controller applied credentials successfully.
func RunUpgrade(ctx context.Context, t *testing.T, opts UpgradeOptions) {
	t.Helper()
	if err := opts.validate(); err != nil {
		t.Fatal(err)
	}
	f, err := loadFixture(opts.FixturePath)
	if err != nil || f == nil {
		t.Fatal("upgrade E2E requires a valid credential fixture")
	}
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{CurrentContext: opts.Context}).ClientConfig()
	if err != nil {
		t.Fatal("cannot load the explicitly selected Kubernetes context (details suppressed)")
	}
	config.Timeout = 30 * time.Second
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{ml.AddToScheme, corev1.AddToScheme, appsv1.AddToScheme} {
		if add(scheme) != nil {
			t.Fatal("cannot register upgrade resource types")
		}
	}
	apiClient, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal("cannot create upgrade resource client")
	}
	kube, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatal("cannot create upgrade Kubernetes client")
	}
	var suffix [6]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal("cannot generate upgrade identity")
	}
	id := hex.EncodeToString(suffix[:])
	namespace := "os-upgrade-" + id
	workloadNamespace := namespace
	if opts.Scope == "cluster" {
		workloadNamespace += "-workload"
	}
	u := &upgradeInstaller{opts: opts, namespace: namespace, workloadNamespace: workloadNamespace, release: "os-upgrade-" + id, run: runUpgradeCommand, client: apiClient, owned: map[string]*unstructured.Unstructured{}}
	source, err := u.render(ctx, opts.SourceDir, opts.SourceImage, 1)
	if err != nil {
		t.Fatal(err)
	}
	target, err := u.render(ctx, opts.TargetDir, opts.TargetImage, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !sourceHasDurableCheckpoint(target) {
		t.Fatal("candidate artifacts lack durable object storage checkpoint schema")
	}
	if err := u.preflight(ctx, source, target); err != nil {
		t.Fatal(err)
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
	if apiClient.Create(ctx, ns) != nil {
		t.Fatal("cannot create dedicated upgrade namespace")
	}
	s := &scenario{ctx: ctx, client: apiClient, kube: kube, opts: Options{Namespace: workloadNamespace, OperatorNamespace: namespace, OperatorImage: opts.SourceImage, MarkLogicImage: opts.MarkLogicImage}, name: "os-" + id, group: "os-node-" + id, admin: "os-admin-" + id, started: metav1.Now()}
	for _, values := range []material{f.AWS.Initial, f.AWS.Rotated, f.Azure.Initial, f.Azure.Rotated} {
		for _, value := range values {
			s.values = append(s.values, value, strings.TrimSpace(value))
		}
	}
	t.Cleanup(func() { s.cleanup(t); u.cleanup(t, ns) })
	if workloadNamespace != namespace {
		workloadNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: workloadNamespace}}
		if apiClient.Create(ctx, workloadNS) != nil {
			t.Fatal("cannot create dedicated cross-namespace upgrade workload")
		}
		s.namespace = workloadNS.DeepCopy()
	}
	defer func() {
		if t.Failed() && s.clusterUID != "" && len(s.operatorEpochs) != 0 && !s.operatorEpochs[len(s.operatorEpochs)-1].Closed {
			s.checkPublishedSafety(t)
		}
	}()
	t.Logf("upgrade scope=%s source=%s candidate=%s run=%s", opts.Scope, opts.SourceImage, opts.TargetImage, id)
	if err := u.install(ctx, opts.SourceDir, opts.SourceImage, 1, source); err != nil {
		t.Fatal(err)
	}
	s.waitUpgradeDeployment(t)
	s.verifyOperator(t)
	sourceImageIDs := s.upgradeImageIDs(t)
	durable := sourceHasDurableCheckpoint(source)
	s.setupUpgradeWorkload(t, f, durable)
	s.waitPod(t, 0)
	s.waitPod(t, 1)
	beforeChildren := s.upgradeWorkloadIdentity(s.ctx, t)
	var beforeAWS, beforeAzure *ml.ObjectStorageProviderStatus
	if durable {
		beforeAWS = s.waitReady(t, testutils.ObjectStorageAWS)
		s.waitReady(t, testutils.ObjectStorageAzure)
		s.deleteSecret(t, s.name+"-azure")
		beforeAzure = s.waitPhase(t, testutils.ObjectStorageAzure, ml.ObjectStoragePhaseDetached, ml.ObjectStorageReasonSecretDeletedAfterApply)
		t.Log("source controller established real AWS Applied and Azure Detached checkpoints")
	} else {
		t.Log("source lacks complete checkpoint schema; legacy Applied will be synthetic test setup, not historical credential-application evidence")
	}
	var crds []*unstructured.Unstructured
	for _, obj := range target {
		if obj.GetKind() == "CustomResourceDefinition" {
			crds = append(crds, obj)
		}
	}
	var stopped *operatorDeployment
	var heldCluster *ml.MarklogicCluster
	transition := upgradeTransition{
		stop: func() error { stopped = s.stopOperator(t); return nil },
		schema: func() error {
			if err := u.applyOwnedSchemas(ctx, crds); err != nil {
				return err
			}
			u.recordOwned(ctx, crds)
			return u.waitSchemas(ctx, crds)
		},
		validateSchema: func() error {
			s.roundTripUpgradeSchema(t)
			if !durable {
				s.seedLegacyUpgradeCheckpoint(t, nil)
			}
			heldCluster = &ml.MarklogicCluster{}
			if apiClient.Get(ctx, s.key(s.name), heldCluster) != nil {
				return errors.New("cannot record cluster checkpoint before held candidate installation")
			}
			return nil
		},
		installStopped: func() error { return u.install(ctx, opts.TargetDir, opts.TargetImage, 0, target) },
		validateStopped: func() error {
			deployment := &appsv1.Deployment{}
			if apiClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: "marklogic-operator-controller-manager"}, deployment) != nil || deployment.UID != stopped.UID || deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 0 {
				return errors.New("candidate installation did not retain the stopped controller identity")
			}
			pods, err := s.operatorPods(ctx)
			if err != nil || len(pods) != 0 {
				return errors.New("candidate installation started controller pods during the schema window")
			}
			found := false
			for _, container := range deployment.Spec.Template.Spec.Containers {
				if container.Name == "manager" && container.Image == opts.TargetImage {
					found = true
				}
			}
			if !found {
				return errors.New("candidate installation did not set the requested operator image")
			}
			actual := &ml.MarklogicCluster{}
			if apiClient.Get(ctx, s.key(s.name), actual) != nil || !reflect.DeepEqual(heldCluster.Spec.ObjectStorage, actual.Spec.ObjectStorage) || !reflect.DeepEqual(heldCluster.Status.ObjectStorage, actual.Status.ObjectStorage) {
				return errors.New("held package installation pruned or changed the existing objectStorage checkpoint")
			}
			// Validate the schema installed by the actual package operation too; Helm
			// can evaluate upgrade/API-capability conditionals differently from rendering.
			s.roundTripUpgradeSchema(t)
			return nil
		},
		start: func() error { s.startOperator(t, stopped, opts.TargetImage); return nil },
	}
	if err := transition.run(); err != nil {
		t.Fatal(err)
	}
	if err := distinctUpgradeImageIDs(sourceImageIDs, s.upgradeImageIDs(t)); err != nil {
		t.Fatal(err)
	}
	if durable {
		// Increment generation with an informational-only field so both statuses must be
		// reconciled by the new controller before preservation can pass.
		s.updateCluster(t, func(c *ml.MarklogicCluster) {
			if c.Spec.ObjectStorage.AWS.Region == "us-west-2" {
				c.Spec.ObjectStorage.AWS.Region = "us-east-1"
			} else {
				c.Spec.ObjectStorage.AWS.Region = "us-west-2"
			}
		})
		if err := upgradeAppliedContinuation(beforeAWS, s.waitReady(t, testutils.ObjectStorageAWS)); err != nil {
			t.Fatal(err)
		}
		afterAzure := s.waitPhase(t, testutils.ObjectStorageAzure, ml.ObjectStoragePhaseDetached, ml.ObjectStorageReasonSecretDeletedAfterApply)
		assertSameSuccess(t, beforeAzure, afterAzure)
		if !reflect.DeepEqual(beforeAzure.ObservedSecret, afterAzure.ObservedSecret) {
			t.Fatal("upgrade changed the detached provider's retained observed Secret identity")
		}
		s.createSecret(t, s.name+"-azure", secretData(f.Azure.Initial))
		s.waitReady(t, testutils.ObjectStorageAzure)
	} else {
		for _, provider := range []testutils.ObjectStorageProvider{testutils.ObjectStorageAWS, testutils.ObjectStorageAzure} {
			after := s.waitReady(t, provider)
			if after.LastAppliedTime.Time.Equal(time.Unix(1, 0)) || after.AppliedFingerprint == "synthetic-legacy-checkpoint" {
				t.Fatal("candidate retained synthetic legacy success instead of establishing a complete checkpoint")
			}
		}
	}
	s.recoverUnchangedUpgradeCheckpoint(t)
	s.assertUpgradeWorkloadPreserved(t, beforeChildren)
	s.checkPublishedSafety(t)
}

func (s *scenario) upgradeImageIDs(t *testing.T) []string {
	t.Helper()
	pods, err := s.operatorPods(s.ctx)
	if err != nil {
		t.Fatal("cannot inspect the running upgrade operator image identities")
	}
	if _, err := operatorSources(pods, s.opts.OperatorImage); err != nil {
		t.Fatal("upgrade operator image identities are not ready or complete")
	}
	var ids []string
	for _, pod := range pods {
		for _, status := range pod.Status.ContainerStatuses {
			if status.Name == "manager" {
				ids = append(ids, status.ImageID)
			}
		}
	}
	return ids
}

// Tags identify requested versions, but only runtime-reported IDs show what was
// actually executed. Strip repository/runtime wrappers so aliases cannot hide an
// identical digest. Apply this only at the source-to-candidate transition.
func distinctUpgradeImageIDs(source, candidate []string) error {
	if len(source) == 0 || len(candidate) == 0 {
		return errors.New("upgrade requires recorded source and candidate image identities")
	}
	canonical := func(id string) string {
		id = strings.TrimSpace(id)
		if index := strings.LastIndex(id, "@"); index >= 0 {
			id = id[index+1:]
		}
		if index := strings.Index(id, "://"); index >= 0 {
			id = id[index+3:]
		}
		return id
	}
	sourceIDs := map[string]bool{}
	for _, id := range source {
		key := canonical(id)
		if key == "" {
			return errors.New("upgrade source image identity is empty")
		}
		sourceIDs[key] = true
	}
	for _, id := range candidate {
		key := canonical(id)
		if key == "" {
			return errors.New("upgrade candidate image identity is empty")
		}
		if sourceIDs[key] {
			return errors.New("upgrade source and candidate resolved to the same running operator image")
		}
	}
	return nil
}

func (u *upgradeInstaller) waitSchemas(ctx context.Context, crds []*unstructured.Unstructured) error {
	err := wait.PollUntilContextTimeout(ctx, pollInterval, time.Minute, true, func(ctx context.Context) (bool, error) {
		for _, obj := range crds {
			current := &unstructured.Unstructured{}
			current.SetGroupVersionKind(obj.GroupVersionKind())
			if u.client.Get(ctx, client.ObjectKeyFromObject(obj), current) != nil {
				return false, nil
			}
			conditions, _, _ := unstructured.NestedSlice(current.Object, "status", "conditions")
			established := false
			for _, value := range conditions {
				if condition, ok := value.(map[string]any); ok && condition["type"] == "Established" && condition["status"] == "True" {
					established = true
				}
			}
			if !established {
				return false, nil
			}
		}
		return true, nil
	})
	if err != nil {
		return errors.New("candidate CRDs were not Established within the bounded schema wait")
	}
	return nil
}

func (s *scenario) waitUpgradeDeployment(t *testing.T) {
	t.Helper()
	if wait.PollUntilContextTimeout(s.ctx, pollInterval, 5*time.Minute, true, func(ctx context.Context) (bool, error) {
		d := &appsv1.Deployment{}
		if s.client.Get(ctx, types.NamespacedName{Namespace: s.opts.OperatorNamespace, Name: "marklogic-operator-controller-manager"}, d) != nil {
			return false, nil
		}
		return d.Status.ObservedGeneration == d.Generation && d.Status.ReadyReplicas == 1 && d.Status.UpdatedReplicas == 1 && d.Status.Replicas == 1, nil
	}) != nil {
		t.Fatal("upgrade operator readiness timed out (diagnostic output suppressed)")
	}
}

func (s *scenario) setupUpgradeWorkload(t *testing.T, f *fixture, durable bool) {
	t.Helper()
	var password [24]byte
	if _, err := rand.Read(password[:]); err != nil {
		t.Fatal("cannot generate upgrade bootstrap password")
	}
	value := hex.EncodeToString(password[:])
	s.values = append(s.values, value)
	s.createSecret(t, s.admin, map[string][]byte{"username": []byte("os-admin"), "password": []byte(value), "wallet-password": []byte(value)})
	s.createSecret(t, s.name+"-aws", secretData(f.AWS.Initial))
	s.createSecret(t, s.name+"-azure", secretData(f.Azure.Initial))
	replicas := int32(2)
	c := &ml.MarklogicCluster{ObjectMeta: metav1.ObjectMeta{Name: s.name, Namespace: s.opts.Namespace}, Spec: ml.MarklogicClusterSpec{Image: s.opts.MarkLogicImage, Auth: &ml.AdminAuth{SecretName: &s.admin}, ServiceAccountName: s.name + "-workload", Persistence: &ml.Persistence{Enabled: false, Size: "1Gi"}, MarkLogicGroups: []*ml.MarklogicGroups{{Name: s.group, Replicas: &replicas, IsBootstrap: true}}}}
	if durable {
		c.Spec.ObjectStorage = s.upgradeStorageConfig()
	}
	if s.client.Create(s.ctx, c) != nil {
		t.Fatal("cannot create baseline upgrade workload (details suppressed)")
	}
	s.clusterUID = c.UID
}

func (s *scenario) upgradeStorageConfig() *ml.ObjectStorageConfig {
	return &ml.ObjectStorageConfig{AWS: &ml.AWSObjectStorage{AuthType: ml.ObjectStorageAuthSecret, SecretName: s.name + "-aws", Region: "us-east-1"}, Azure: &ml.AzureObjectStorage{AuthType: ml.ObjectStorageAuthSecret, SecretName: s.name + "-azure"}}
}

type upgradeWorkloadState struct {
	ResourceUIDs []types.UID
	Pods         []workloadPodState
}

func (s *scenario) upgradeWorkloadIdentity(ctx context.Context, t *testing.T) upgradeWorkloadState {
	t.Helper()
	objects := []client.Object{&ml.MarklogicCluster{}, &ml.MarklogicGroup{}, &appsv1.StatefulSet{}, &corev1.Pod{}, &corev1.Pod{}}
	names := []string{s.name, s.group, s.group, s.group + "-0", s.group + "-1"}
	state := upgradeWorkloadState{}
	for i, obj := range objects {
		if s.client.Get(ctx, s.key(names[i]), obj) != nil || obj.GetUID() == "" || obj.GetDeletionTimestamp() != nil {
			t.Fatal("cannot verify preserved upgrade workload identity")
		}
		state.ResourceUIDs = append(state.ResourceUIDs, obj.GetUID())
		if pod, ok := obj.(*corev1.Pod); ok {
			identity, err := workloadPodIdentity(pod)
			if err != nil {
				t.Fatal("upgrade workload container identity is not ready or complete")
			}
			state.Pods = append(state.Pods, identity)
		}
	}
	return state
}

func (s *scenario) assertUpgradeWorkloadPreserved(t *testing.T, before upgradeWorkloadState) {
	t.Helper()
	settled := time.Time{}
	err := wait.PollUntilContextTimeout(s.ctx, pollInterval, statusTimeout, true, func(ctx context.Context) (bool, error) {
		if !reflect.DeepEqual(before, s.upgradeWorkloadIdentity(ctx, t)) {
			return false, errors.New("workload identity changed")
		}
		sts := &appsv1.StatefulSet{}
		if s.client.Get(ctx, s.key(s.group), sts) != nil || sts.Status.ObservedGeneration != sts.Generation || sts.Status.ReadyReplicas != 2 {
			settled = time.Time{}
			return false, nil
		}
		if settled.IsZero() {
			settled = time.Now()
		}
		return time.Since(settled) >= 10*time.Second, nil
	})
	if err != nil {
		t.Fatal("upgrade recreated the cluster/group/StatefulSet/server pods or workload did not settle")
	}
}

func (s *scenario) roundTripUpgradeSchema(t *testing.T) {
	t.Helper()
	// No controller is running. Keep a valid bootstrap group in the probe spec;
	// zero replicas also prevent pods if external interference restarts a controller.
	replicas := int32(0)
	probe := &ml.MarklogicCluster{ObjectMeta: metav1.ObjectMeta{Name: s.name + "-schema", Namespace: s.opts.Namespace}, Spec: ml.MarklogicClusterSpec{Image: s.opts.MarkLogicImage, ObjectStorage: s.upgradeStorageConfig(), MarkLogicGroups: []*ml.MarklogicGroups{{Name: s.group + "-schema", Replicas: &replicas, IsBootstrap: true}}}}
	defer func() {
		if probe.UID != "" && s.client.Delete(s.ctx, probe, client.Preconditions{UID: &probe.UID}) != nil {
			t.Error("cannot remove schema probe")
		}
	}()
	eligible := true
	entry := &ml.ObjectStorageProviderStatus{Phase: ml.ObjectStoragePhaseApplied, Message: "synthetic schema roundtrip", AuthType: "secret", AppliedFingerprint: "synthetic-schema-roundtrip", LastAppliedTime: &metav1.Time{Time: time.Unix(1, 0).UTC()}, AppliedSecret: &ml.ObjectStorageSecretRef{Name: s.name + "-aws", UID: "schema-test-uid", ResourceVersion: "schema-test-rv"}, DetachEligible: &eligible, ObservedSecret: &ml.ObjectStorageSecretRef{Name: s.name + "-aws", UID: "schema-test-uid", ResourceVersion: "schema-test-rv"}}
	// CRD Established may precede schema propagation to every API server. Retry
	// read/write roundtrips, without allowing a candidate controller to start.
	err := wait.PollUntilContextTimeout(s.ctx, pollInterval, time.Minute, true, func(ctx context.Context) (bool, error) {
		if probe.UID == "" {
			if err := s.client.Create(ctx, probe); err != nil {
				return false, nil
			}
		} else {
			if s.client.Get(ctx, client.ObjectKeyFromObject(probe), probe) != nil {
				return false, nil
			}
			probe.Spec.ObjectStorage = s.upgradeStorageConfig()
			if s.client.Update(ctx, probe) != nil {
				return false, nil
			}
		}
		expectedSpec := s.upgradeStorageConfig()
		entry.ObservedGeneration = probe.Generation
		expectedStatus := &ml.ObjectStorageStatus{AWS: entry.DeepCopy(), Azure: entry.DeepCopy()}
		expectedStatus.Azure.AppliedSecret.Name = s.name + "-azure"
		expectedStatus.Azure.ObservedSecret.Name = s.name + "-azure"
		for _, state := range []struct {
			phase    ml.ObjectStoragePhase
			reason   ml.ObjectStorageReason
			eligible bool
		}{
			{ml.ObjectStoragePhaseApplied, "", true},
			{ml.ObjectStoragePhaseDetached, ml.ObjectStorageReasonSecretDeletedAfterApply, true},
			{ml.ObjectStoragePhasePending, ml.ObjectStorageReasonBootstrapNotReady, false},
			{ml.ObjectStoragePhaseFailed, ml.ObjectStorageReasonSecretKeyMissing, false},
		} {
			status := expectedStatus.DeepCopy()
			for _, entry := range []*ml.ObjectStorageProviderStatus{status.AWS, status.Azure} {
				entry.Phase, entry.Reason, entry.DetachEligible = state.phase, state.reason, &state.eligible
			}
			probe.Status.ObjectStorage = status.DeepCopy()
			if s.client.Status().Update(ctx, probe) != nil {
				return false, nil
			}
			actual := &ml.MarklogicCluster{}
			if s.client.Get(ctx, client.ObjectKeyFromObject(probe), actual) != nil || !reflect.DeepEqual(expectedSpec, actual.Spec.ObjectStorage) || !reflect.DeepEqual(status, actual.Status.ObjectStorage) {
				return false, nil
			}
		}
		return true, nil
	})
	if err != nil {
		t.Fatal("candidate API schema rejected or pruned objectStorage spec/checkpoint status fields")
	}
}

// Only equality comparisons against controller-published fingerprints are permitted.
// A synthetic marker can demonstrate schema-upgrade recovery but cannot prove that
// incomplete status forces a PUT when the material fingerprint already matches.
func legacyUpgradeEntry(before *ml.ObjectStorageProviderStatus, generation int64, secret testutils.SecretMeta) *ml.ObjectStorageProviderStatus {
	entry := &ml.ObjectStorageProviderStatus{Phase: ml.ObjectStoragePhaseApplied, AuthType: "secret", AppliedFingerprint: "synthetic-legacy-checkpoint"}
	if before != nil {
		entry = before.DeepCopy()
	}
	entry.Phase = ml.ObjectStoragePhaseApplied
	entry.AppliedSecret, entry.DetachEligible = nil, nil
	entry.LastAppliedTime = &metav1.Time{Time: time.Unix(1, 0).UTC()}
	entry.ObservedGeneration = generation
	entry.ObservedSecret = &ml.ObjectStorageSecretRef{Name: secret.Name, UID: secret.UID, ResourceVersion: secret.ResourceVersion}
	return entry
}

// An encoding change between declared releases may legitimately change the opaque
// marker and require one PUT. Unchanged markers must retain their success record.
func upgradeAppliedContinuation(before, after *ml.ObjectStorageProviderStatus) error {
	if before == nil || after == nil || before.LastAppliedTime == nil || after.LastAppliedTime == nil || before.AppliedSecret == nil || after.AppliedSecret == nil || after.ObservedSecret == nil || after.AppliedFingerprint == "" || before.AuthType != after.AuthType || before.AppliedSecret.Name != after.AppliedSecret.Name || before.AppliedSecret.UID != after.AppliedSecret.UID {
		return errors.New("upgrade did not preserve the applied provider binding")
	}
	if before.AppliedFingerprint == after.AppliedFingerprint {
		if !reflect.DeepEqual(before.AppliedSecret, after.AppliedSecret) || !reflect.DeepEqual(before.LastAppliedTime, after.LastAppliedTime) {
			return errors.New("upgrade unnecessarily replaced success for an unchanged fingerprint")
		}
	} else if reflect.DeepEqual(before.LastAppliedTime, after.LastAppliedTime) || !reflect.DeepEqual(after.AppliedSecret, after.ObservedSecret) {
		return errors.New("changed upgrade fingerprint lacks a fresh success checkpoint for the current Secret")
	}
	return nil
}

func legacyUpgradeNotReady(c *ml.MarklogicCluster, provider testutils.ObjectStorageProvider, secret testutils.SecretMeta, fingerprint string) error {
	if c == nil || c.Spec.ObjectStorage == nil {
		return errors.New("legacy upgrade readiness assertion requires a declared provider")
	}
	name, auth := "", ml.ObjectStorageAuthType("")
	if provider == testutils.ObjectStorageAWS && c.Spec.ObjectStorage.AWS != nil {
		name, auth = c.Spec.ObjectStorage.AWS.SecretName, c.Spec.ObjectStorage.AWS.AuthType
	}
	if provider == testutils.ObjectStorageAzure && c.Spec.ObjectStorage.Azure != nil {
		name, auth = c.Spec.ObjectStorage.Azure.SecretName, c.Spec.ObjectStorage.Azure.AuthType
	}
	if name == "" || name != secret.Name || (auth != "" && auth != ml.ObjectStorageAuthSecret) {
		return errors.New("legacy upgrade readiness assertion requires the current declared secret binding")
	}
	if err := currentIncompleteCheckpoint(c, provider, secret); err != nil {
		return err
	}
	entry := providerStatus(c, provider)
	if entry.AppliedFingerprint != fingerprint || !entry.LastAppliedTime.Time.Equal(time.Unix(1, 0)) {
		return errors.New("incomplete upgrade checkpoint did not roundtrip intact")
	}
	return nil
}

func (s *scenario) seedLegacyUpgradeCheckpoint(t *testing.T, before map[testutils.ObjectStorageProvider]*ml.ObjectStorageProviderStatus) {
	t.Helper()
	if before == nil {
		s.updateCluster(t, func(c *ml.MarklogicCluster) { c.Spec.ObjectStorage = s.upgradeStorageConfig() })
	}
	expected := map[testutils.ObjectStorageProvider]string{}
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		c := &ml.MarklogicCluster{}
		if err := s.client.Get(s.ctx, s.key(s.name), c); err != nil {
			return err
		}
		if c.UID != s.clusterUID {
			return errors.New("upgrade checkpoint cluster was replaced")
		}
		c.Status.ObjectStorage = &ml.ObjectStorageStatus{}
		for _, provider := range []testutils.ObjectStorageProvider{testutils.ObjectStorageAWS, testutils.ObjectStorageAzure} {
			secret := &metav1.PartialObjectMetadata{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"}}
			if err := s.client.Get(s.ctx, s.key(s.name+"-"+string(provider)), secret); err != nil {
				return err
			}
			if before != nil {
				prior := before[provider]
				if prior == nil || prior.ObservedSecret == nil || prior.ObservedSecret.Name != secret.Name || prior.ObservedSecret.UID != string(secret.UID) || prior.ObservedSecret.ResourceVersion != secret.ResourceVersion {
					return errors.New("upgrade Secret changed before unchanged-material legacy checkpoint setup")
				}
			}
			entry := legacyUpgradeEntry(before[provider], c.Generation, testutils.SecretMeta{Name: secret.Name, UID: string(secret.UID), ResourceVersion: secret.ResourceVersion})
			expected[provider] = entry.AppliedFingerprint
			if provider == testutils.ObjectStorageAWS {
				c.Status.ObjectStorage.AWS = entry
			} else {
				c.Status.ObjectStorage.Azure = entry
			}
		}
		return s.client.Status().Update(s.ctx, c)
	})
	if err != nil {
		t.Fatal("cannot seed incomplete upgrade checkpoint (details suppressed)")
	}
	c := &ml.MarklogicCluster{}
	if s.client.Get(s.ctx, s.key(s.name), c) != nil {
		t.Fatal("cannot read back incomplete upgrade checkpoint")
	}
	for _, provider := range []testutils.ObjectStorageProvider{testutils.ObjectStorageAWS, testutils.ObjectStorageAzure} {
		secret := &metav1.PartialObjectMetadata{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"}}
		if s.client.Get(s.ctx, s.key(s.name+"-"+string(provider)), secret) != nil {
			t.Fatal("cannot read current Secret metadata for upgrade readiness assertion")
		}
		if err := legacyUpgradeNotReady(c, provider, testutils.SecretMeta{Name: secret.Name, UID: string(secret.UID), ResourceVersion: secret.ResourceVersion}, expected[provider]); err != nil {
			t.Fatal(err)
		}
	}
}

func (s *scenario) recoverUnchangedUpgradeCheckpoint(t *testing.T) {
	t.Helper()
	before := map[testutils.ObjectStorageProvider]*ml.ObjectStorageProviderStatus{}
	for _, provider := range []testutils.ObjectStorageProvider{testutils.ObjectStorageAWS, testutils.ObjectStorageAzure} {
		before[provider] = s.waitReady(t, provider)
	}
	operator := s.stopOperator(t)
	// Keep real opaque fingerprints and the exact current Secret revisions. Readiness
	// is checked while stopped, before any generation/revision-changing operation.
	s.seedLegacyUpgradeCheckpoint(t, before)
	s.startOperator(t, operator, operator.Image)
	after := map[testutils.ObjectStorageProvider]*ml.ObjectStorageProviderStatus{}
	for _, provider := range []testutils.ObjectStorageProvider{testutils.ObjectStorageAWS, testutils.ObjectStorageAzure} {
		after[provider] = s.waitReady(t, provider)
		if after[provider].AppliedFingerprint != before[provider].AppliedFingerprint || after[provider].LastAppliedTime.Time.Equal(time.Unix(1, 0)) {
			t.Fatal("unchanged-material legacy recovery did not establish a new complete success checkpoint")
		}
	}
	// Separate timestamp resolution before verifying the post-recovery unchanged path.
	start := time.Now()
	if wait.PollUntilContextTimeout(s.ctx, pollInterval, 10*time.Second, true, func(context.Context) (bool, error) { return time.Since(start) >= 2*time.Second, nil }) != nil {
		t.Fatal("upgrade unchanged-checkpoint observation interrupted")
	}
	s.updateCluster(t, func(c *ml.MarklogicCluster) {
		if c.Spec.ObjectStorage.AWS.Region == "us-west-2" {
			c.Spec.ObjectStorage.AWS.Region = "us-east-1"
		} else {
			c.Spec.ObjectStorage.AWS.Region = "us-west-2"
		}
	})
	for _, provider := range []testutils.ObjectStorageProvider{testutils.ObjectStorageAWS, testutils.ObjectStorageAzure} {
		assertSameSuccess(t, after[provider], s.waitReady(t, provider))
	}
	t.Log("candidate recovered incomplete checkpoints with unchanged published fingerprints, then preserved complete checkpoints across unchanged reconciliation")
}

func (u *upgradeInstaller) cleanup(t *testing.T, ns *corev1.Namespace) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	// Group garbage collection may lag the cluster/pod cleanup. Give owned-namespace
	// objects time to disappear; foreign objects always prevent shared-schema deletion.
	err := wait.PollUntilContextTimeout(ctx, pollInterval, 2*time.Minute, true, u.markLogicResourcesRemoved)
	if err != nil {
		t.Error("upgrade cleanup retained installation: MarkLogic resources remain or cannot be checked")
		return
	}
	// Delete only resource identities created by this run, with UID preconditions.
	for _, obj := range u.owned {
		uid := obj.GetUID()
		if err := u.client.Delete(ctx, obj, client.Preconditions{UID: &uid}); err != nil && !apierrors.IsNotFound(err) {
			t.Error("upgrade cleanup could not remove an owned installation resource (details suppressed)")
		}
	}
	if err := u.client.Delete(ctx, ns, client.Preconditions{UID: &ns.UID}); err != nil && !apierrors.IsNotFound(err) {
		t.Error("upgrade cleanup could not remove its owned namespace")
	}
}

func (u *upgradeInstaller) markLogicResourcesRemoved(ctx context.Context) (bool, error) {
	empty := true
	for _, kind := range []string{"MarklogicClusterList", "MarklogicGroupList"} {
		list := &unstructured.UnstructuredList{}
		list.SetAPIVersion("marklogic.progress.com/v1")
		list.SetKind(kind)
		if err := u.client.List(ctx, list); err != nil {
			if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
				continue
			}
			return false, errors.New("cannot inspect remaining resources")
		}
		for _, obj := range list.Items {
			if obj.GetNamespace() != u.namespace && obj.GetNamespace() != u.workloadNamespace {
				return false, errors.New("foreign resource exists")
			}
			empty = false
		}
	}
	return empty, nil
}
