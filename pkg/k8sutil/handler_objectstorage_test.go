// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package k8sutil

import (
	"context"
	"errors"
	"testing"
	"time"

	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	"github.com/marklogic/marklogic-operator-kubernetes/pkg/result"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func TestReconcileOutcomeCombination(t *testing.T) {
	t.Parallel()
	errCore := errors.New("core failure")
	errOther := errors.New("other failure")

	tests := []struct {
		name      string
		apply     func(*reconcileOutcome)
		want      reconcile.Result
		wantErrs  []error
		wantNoErr bool
	}{
		{
			name:      "nothing requested",
			apply:     func(o *reconcileOutcome) { o.addStep(result.Continue()); o.add(reconcile.Result{}, nil) },
			want:      reconcile.Result{},
			wantNoErr: true,
		},
		{
			name: "core error plus object storage retry keeps the error",
			apply: func(o *reconcileOutcome) {
				o.add(reconcile.Result{}, errCore)
				o.add(reconcile.Result{RequeueAfter: 10 * time.Second}, nil)
			},
			wantErrs: []error{errCore},
		},
		{
			name: "core retry plus object storage continue keeps the retry",
			apply: func(o *reconcileOutcome) {
				o.addStep(result.RequeueSoon(5))
				o.add(reconcile.Result{}, nil)
			},
			want:      reconcile.Result{Requeue: true, RequeueAfter: 5 * time.Second},
			wantNoErr: true,
		},
		{
			name: "simultaneous errors are all retained",
			apply: func(o *reconcileOutcome) {
				o.addStep(result.Error(errCore))
				o.add(reconcile.Result{}, errOther)
			},
			wantErrs: []error{errCore, errOther},
		},
		{
			name: "earliest positive delay wins",
			apply: func(o *reconcileOutcome) {
				o.add(reconcile.Result{RequeueAfter: 30 * time.Second}, nil)
				o.add(reconcile.Result{RequeueAfter: 10 * time.Second}, nil)
				o.addStep(result.RequeueSoon(20))
			},
			want:      reconcile.Result{Requeue: true, RequeueAfter: 10 * time.Second},
			wantNoErr: true,
		},
		{
			name: "immediate requeue wins over delays when error free",
			apply: func(o *reconcileOutcome) {
				o.add(reconcile.Result{RequeueAfter: 10 * time.Second}, nil)
				o.add(reconcile.Result{Requeue: true}, nil)
			},
			want:      reconcile.Result{Requeue: true},
			wantNoErr: true,
		},
		{
			name: "error wins over an immediate request",
			apply: func(o *reconcileOutcome) {
				o.add(reconcile.Result{Requeue: true}, nil)
				o.add(reconcile.Result{}, errCore)
			},
			wantErrs: []error{errCore},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			outcome := &reconcileOutcome{}
			test.apply(outcome)
			got, err := outcome.output()
			if test.wantNoErr {
				if err != nil {
					t.Fatalf("unexpected error %v", err)
				}
				if got != test.want {
					t.Fatalf("result = %+v, want %+v", got, test.want)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected an error")
			}
			if !got.IsZero() {
				t.Fatalf("a result alongside an error would be ignored; expected zero, got %+v", got)
			}
			for _, want := range test.wantErrs {
				if !errors.Is(err, want) {
					t.Fatalf("combined error %v lost %v", err, want)
				}
			}
		})
	}
}

func handlerHarness(t *testing.T, mutate func(*marklogicv1.MarklogicCluster)) *osHarness {
	t.Helper()
	h := newOSHarness(t, func(c *marklogicv1.MarklogicCluster) {
		c.Spec.ObjectStorage.Azure = nil
		if mutate != nil {
			mutate(c)
		}
	})
	h.setSecret("aws-creds", "aws-uid-1", awsMaterial("1"))
	return h
}

func (h *osHarness) groupExists(name string) bool {
	h.t.Helper()
	err := h.c.Get(context.Background(), types.NamespacedName{Namespace: osNamespace, Name: name}, &marklogicv1.MarklogicGroup{})
	if err != nil && !apierrors.IsNotFound(err) {
		h.t.Fatalf("get group: %v", err)
	}
	return err == nil
}

func failCreateOf[T client.Object](err error) func(client.Object) error {
	return func(obj client.Object) error {
		if _, ok := obj.(T); ok {
			return err
		}
		return nil
	}
}

func TestClusterHandlerHappyPathAppliesObjectStorageAfterGroups(t *testing.T) {
	t.Parallel()
	h := handlerHarness(t, nil)
	res, err := h.context().ReconsileMarklogicClusterHandler()
	if err != nil || res.RequeueAfter != 0 {
		t.Fatalf("unexpected result %+v, %v", res, err)
	}
	if !h.groupExists("dnode") {
		t.Fatalf("core group reconciliation should have run")
	}
	assertPhase(t, h.entry(providerAWS), marklogicv1.ObjectStoragePhaseApplied, "")
}

func TestClusterHandlerServiceAccountFailureStillEvaluatesObjectStorage(t *testing.T) {
	t.Parallel()
	failure := errors.New("serviceaccount create failed")
	h := handlerHarness(t, func(c *marklogicv1.MarklogicCluster) { c.Spec.ServiceAccountName = "workload" })
	h.createErr = failCreateOf[*corev1.ServiceAccount](failure)

	res, err := h.context().ReconsileMarklogicClusterHandler()
	if !errors.Is(err, failure) || !res.IsZero() {
		t.Fatalf("expected the core error to be returned unchanged, got %+v, %v", res, err)
	}
	if h.groupExists("dnode") {
		t.Fatalf("existing prerequisite ordering must be preserved: groups are not created after a service account failure")
	}
	assertPhase(t, h.entry(providerAWS), marklogicv1.ObjectStoragePhaseApplied, "")
}

func TestClusterHandlerAdminSecretFailureStillPersistsResolutionFailures(t *testing.T) {
	t.Parallel()
	failure := errors.New("admin secret create failed")
	h := newOSHarness(t, nil)
	// No bootstrap admin Secret exists and its creation fails; the provider Secrets are missing.
	if err := h.c.Delete(context.Background(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: osCluster + "-admin", Namespace: osNamespace}}); err != nil {
		t.Fatalf("delete admin secret: %v", err)
	}
	h.createErr = failCreateOf[*corev1.Secret](failure)

	_, err := h.context().ReconsileMarklogicClusterHandler()
	if !errors.Is(err, failure) {
		t.Fatalf("expected the admin Secret error, got %v", err)
	}
	assertPhase(t, h.entry(providerAWS), marklogicv1.ObjectStoragePhaseFailed, marklogicv1.ObjectStorageReasonSecretNotFound)
	assertPhase(t, h.entry(providerAzure), marklogicv1.ObjectStoragePhaseFailed, marklogicv1.ObjectStorageReasonSecretNotFound)
}

func TestClusterHandlerGroupFailureDoesNotSkipObjectStorage(t *testing.T) {
	t.Parallel()
	failure := errors.New("group create failed")
	h := handlerHarness(t, nil)
	h.createErr = failCreateOf[*marklogicv1.MarklogicGroup](failure)

	_, err := h.context().ReconsileMarklogicClusterHandler()
	if !errors.Is(err, failure) {
		t.Fatalf("expected the group error, got %v", err)
	}
	assertPhase(t, h.entry(providerAWS), marklogicv1.ObjectStoragePhaseApplied, "")
}

func TestClusterHandlerNetworkPolicyFailureDoesNotSkipObjectStorage(t *testing.T) {
	t.Parallel()
	failure := errors.New("networkpolicy create failed")
	h := handlerHarness(t, func(c *marklogicv1.MarklogicCluster) { c.Spec.NetworkPolicy.Enabled = true })
	h.createErr = failCreateOf[*networkingv1.NetworkPolicy](failure)

	_, err := h.context().ReconsileMarklogicClusterHandler()
	if !errors.Is(err, failure) {
		t.Fatalf("expected the NetworkPolicy error, got %v", err)
	}
	if !h.groupExists("dnode") {
		t.Fatalf("group reconciliation precedes the NetworkPolicy step")
	}
	assertPhase(t, h.entry(providerAWS), marklogicv1.ObjectStoragePhaseApplied, "")
}

func TestClusterHandlerObjectStorageFailureDoesNotGateGroups(t *testing.T) {
	t.Parallel()
	h := handlerHarness(t, nil)
	h.statusWriteErr = func() error { return errors.New("status unavailable") }

	_, err := h.context().ReconsileMarklogicClusterHandler()
	if err == nil {
		t.Fatalf("status persistence failures are retriable errors")
	}
	if !h.groupExists("dnode") {
		t.Fatalf("independent group reconciliation must still run when object storage fails")
	}
}

func TestClusterHandlerCoreErrorTakesPrecedenceOverObjectStorageRetry(t *testing.T) {
	t.Parallel()
	failure := errors.New("group create failed")
	h := handlerHarness(t, nil)
	h.creds.readyErr = errors.New("not ready")
	h.createErr = failCreateOf[*marklogicv1.MarklogicGroup](failure)

	res, err := h.context().ReconsileMarklogicClusterHandler()
	if !errors.Is(err, failure) || res.RequeueAfter != 0 {
		t.Fatalf("a core error must not be turned into a timed retry, got %+v, %v", res, err)
	}
	assertPhase(t, h.entry(providerAWS), marklogicv1.ObjectStoragePhasePending, marklogicv1.ObjectStorageReasonBootstrapNotReady)
}

func TestClusterHandlerObjectStorageRetryIsReturnedWithoutErrors(t *testing.T) {
	t.Parallel()
	h := handlerHarness(t, nil)
	h.creds.readyErr = errors.New("not ready")

	res, err := h.context().ReconsileMarklogicClusterHandler()
	if err != nil || res.RequeueAfter != 10*time.Second {
		t.Fatalf("expected the 10s bootstrap retry, got %+v, %v", res, err)
	}
}

func TestClusterAnnotationsExcludeTrackingAndReconcileRequestKeys(t *testing.T) {
	t.Parallel()
	source := map[string]string{
		"kubectl.kubernetes.io/last-applied-configuration": "x",
		"e2e.marklogic.progress.com/reconcile-kick":        "1",
		ReconcileRequestAnnotation:                         "2",
		"example.com/keep":                                 "kept",
	}
	cc := &ClusterContext{}
	cc.SetClusterAnnotations(source)
	oc := &OperatorContext{}
	oc.SetOperatorAnnotations(source)

	for name, got := range map[string]map[string]string{"cluster": cc.Annotations, "operator": oc.Annotations} {
		if len(got) != 1 || got["example.com/keep"] != "kept" {
			t.Fatalf("%s annotations = %v, want only the user annotation", name, got)
		}
	}
	if len(source) != 4 {
		t.Fatalf("the source map (an informer-owned object in other callers) must not be mutated")
	}
}
