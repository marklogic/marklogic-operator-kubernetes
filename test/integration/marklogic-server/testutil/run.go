// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package testutil

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

const runLabel = "integration.marklogic.progress.com/run-id"

// Run owns one fresh namespace. It never adopts an existing namespace.
type Run struct {
	Namespace    string
	ID           string
	StorageClass string
	uid          types.UID
	client       kubernetes.Interface
	report       *runReport
}

// NewRun checks prerequisites before creating resources and registers bounded cleanup.
// INTEGRATION_CONTEXT is required even when invoking go test directly.
func NewRun(t *testing.T, scenario string, needsMarkLogic bool) *Run {
	t.Helper()
	run := &Run{ID: string(uuid.NewUUID())}
	report, err := newRunReport(run.ID, scenario, t.Name())
	if err != nil {
		t.Fatalf("Create integration results directory: %v", err)
	}
	run.report = report
	t.Logf("Integration results: %s", report.dir)
	t.Cleanup(func() { run.finishReport(t) })
	run.Stage(t, "preflight")
	prerequisiteFailure := func(err error) {
		run.updateReport(t, func(result *runResult) { result.Outcome = "prerequisites_unmet"; result.FailureStage = "preflight" })
		t.Fatalf("Integration prerequisites not met: %v", err)
	}
	target := strings.TrimSpace(os.Getenv("INTEGRATION_CONTEXT"))
	if target == "" {
		prerequisiteFailure(fmt.Errorf("set INTEGRATION_CONTEXT to the explicit kubectl context for this run"))
	}
	if _, err := exec.LookPath("kubectl"); err != nil {
		prerequisiteFailure(fmt.Errorf("kubectl is required: %w", err))
	}
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{CurrentContext: target}).ClientConfig()
	if err != nil {
		prerequisiteFailure(err)
	}
	config.Timeout = 30 * time.Second
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		prerequisiteFailure(err)
	}
	run.client = client
	run.updateReport(t, func(result *runResult) { result.Server = run.report.redactor.text(config.Host) })
	t.Logf("Integration scenario=%s context=%s server=%s run=%s", scenario, target, config.Host, run.ID)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := run.preflight(ctx, t, needsMarkLogic); err != nil {
		prerequisiteFailure(err)
	}
	run.Stage(t, "create_namespace")
	ns, err := client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		GenerateName: scenario + "-", Labels: map[string]string{runLabel: run.ID},
	}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Create isolated namespace: %v", err)
	}
	run.Namespace, run.uid = ns.Name, ns.UID
	run.updateReport(t, func(result *runResult) { result.Namespace = ns.Name; result.Cleanup = "pending" })
	t.Logf("Created namespace %s (run=%s)", run.Namespace, run.ID)
	t.Cleanup(func() {
		if t.Failed() {
			run.updateReport(t, func(result *runResult) {
				if len(result.Stages) > 0 {
					result.FailureStage = result.Stages[len(result.Stages)-1].Name
				}
			})
			run.Stage(t, "diagnostics")
			run.collectDiagnostics(t)
		}
		if strings.EqualFold(os.Getenv("INTEGRATION_RETAIN_NAMESPACE"), "true") || strings.EqualFold(os.Getenv("MARKLOGIC_OAUTH_RETAIN_NAMESPACE"), "true") {
			run.updateReport(t, func(result *runResult) { result.Cleanup = "retained" })
			t.Logf("Retained namespace %s; inspect with kubectl --context=%q get pods -n %s; clean up with kubectl --context=%q delete namespace %s", run.Namespace, target, run.Namespace, target, run.Namespace)
			return
		}
		run.Stage(t, "cleanup")
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cleanupCancel()
		if err := run.cleanup(cleanupCtx); err != nil {
			run.updateReport(t, func(result *runResult) { result.Cleanup = "failed" })
			t.Errorf("Cleanup namespace %s: %v", run.Namespace, err)
		} else {
			run.updateReport(t, func(result *runResult) { result.Cleanup = "completed" })
			t.Logf("Namespace %s cleanup completed", run.Namespace)
		}
	})
	return run
}

// ApplyObjects labels copies of scenario resources with the run's ownership ID.
func (r *Run) ApplyObjects(t *testing.T, objects ...runtime.Object) {
	t.Helper()
	copies := make([]runtime.Object, 0, len(objects))
	for _, object := range objects {
		if object == nil {
			t.Fatal("Cannot apply a nil object")
		}
		copy := object.DeepCopyObject()
		metadata, err := meta.Accessor(copy)
		if err != nil {
			t.Fatal(err)
		}
		if metadata.GetNamespace() != r.Namespace {
			t.Fatalf("Resource %s namespace %q does not belong to run namespace %q", metadata.GetName(), metadata.GetNamespace(), r.Namespace)
		}
		labels := metadata.GetLabels()
		if labels == nil {
			labels = map[string]string{}
		}
		labels[runLabel] = r.ID
		metadata.SetLabels(labels)
		if r.report != nil {
			if secret, ok := copy.(*corev1.Secret); ok {
				for _, value := range secret.Data {
					r.report.redactor.add(string(value))
				}
				for _, value := range secret.StringData {
					r.report.redactor.add(value)
				}
			}
			contents, err := marshalKubernetesObject(copy)
			if err != nil {
				t.Fatal(err)
			}
			var decoded map[string]any
			if err := json.Unmarshal(contents, &decoded); err != nil {
				t.Fatal(err)
			}
			r.report.redactor.discover(decoded)
			r.updateReport(t, func(result *runResult) {
				result.Resources = append(result.Resources, resourceSummary{Kind: fmt.Sprint(decoded["kind"]), Name: metadata.GetName(), Namespace: metadata.GetNamespace()})
			})
		}
		copies = append(copies, copy)
	}
	r.Stage(t, "deploy_resources")
	ApplyObjects(t, copies...)
}

func (r *Run) cleanup(ctx context.Context) error {
	ns, err := r.client.CoreV1().Namespaces().Get(ctx, r.Namespace, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if ns.UID != r.uid || ns.Labels[runLabel] != r.ID {
		return fmt.Errorf("refusing to delete namespace whose UID or ownership label changed")
	}
	// Record bound volumes before namespace deletion removes the PVCs.
	claims, err := r.client.CoreV1().PersistentVolumeClaims(r.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list PVCs before cleanup: %w", err)
	}
	volumes := map[string]types.UID{}
	for _, claim := range claims.Items {
		if claim.Spec.VolumeName == "" {
			continue
		}
		pv, err := r.client.CoreV1().PersistentVolumes().Get(ctx, claim.Spec.VolumeName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return err
		}
		if pv.Spec.ClaimRef == nil || pv.Spec.ClaimRef.UID != claim.UID {
			return fmt.Errorf("volume %s does not match PVC UID; inspect manually", pv.Name)
		}
		if pv.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimDelete {
			return fmt.Errorf("volume %s has reclaim policy %s; retained namespace for manual cleanup", pv.Name, pv.Spec.PersistentVolumeReclaimPolicy)
		}
		volumes[pv.Name] = pv.UID
	}
	// UID and resourceVersion preconditions protect against replacement or relabeling
	// between the ownership check and the delete request.
	err = r.client.CoreV1().Namespaces().Delete(ctx, r.Namespace, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &r.uid, ResourceVersion: &ns.ResourceVersion}})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return wait.PollUntilContextCancel(ctx, time.Second, true, func(ctx context.Context) (bool, error) {
		ns, err := r.client.CoreV1().Namespaces().Get(ctx, r.Namespace, metav1.GetOptions{})
		if err == nil && ns.UID == r.uid {
			return false, nil
		}
		if err != nil && !apierrors.IsNotFound(err) {
			return false, err
		}
		for name, uid := range volumes {
			pv, err := r.client.CoreV1().PersistentVolumes().Get(ctx, name, metav1.GetOptions{})
			if err == nil && pv.UID == uid {
				return false, nil
			}
			if err != nil && !apierrors.IsNotFound(err) {
				return false, err
			}
		}
		return true, nil
	})
}

// LogImages records configured image references and runtime image IDs after readiness.
func (r *Run) LogImages(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pods, err := r.client.CoreV1().Pods(r.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Errorf("Record component images: %v", err)
		return
	}
	for _, pod := range pods.Items {
		for _, container := range pod.Status.ContainerStatuses {
			r.recordVersion(t, pod.Name+"/"+container.Name, container.Image+" ("+container.ImageID+")")
			t.Logf("Component pod=%s container=%s image=%s imageID=%s", pod.Name, container.Name, container.Image, container.ImageID)
		}
	}
}
