// Copyright (c) 2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package k8sutil

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	"github.com/marklogic/marklogic-operator-kubernetes/pkg/haproxyreload"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// updateCluster modifies the stored cluster; the controller also writes its status, so
// the copy the test holds goes stale.
func (h *reloadHarness) updateCluster(t *testing.T, mutate func(*marklogicv1.MarklogicCluster)) {
	t.Helper()
	cr := &marklogicv1.MarklogicCluster{}
	if err := h.cc.Client.Get(h.cc.Ctx, client.ObjectKeyFromObject(h.cc.MarklogicCluster), cr); err != nil {
		t.Fatal(err)
	}
	mutate(cr)
	if err := h.cc.Client.Update(h.cc.Ctx, cr); err != nil {
		t.Fatal(err)
	}
	h.cc.MarklogicCluster = cr
}

// useTLS switches the cluster to the given certificate and makes that the committed
// configuration, with both replicas reporting that they run it.
func (h *reloadHarness) useTLS(t *testing.T, secret, file string) {
	t.Helper()
	h.updateCluster(t, func(cr *marklogicv1.MarklogicCluster) {
		cr.Spec.HAProxy.Timeout.Client = 60
		cr.Spec.HAProxy.Tls = &marklogicv1.TlsForHAProxy{Enabled: true, SecretName: secret, CertFileName: file}
	})
	h.desired = generateHAProxyConfigMapData(h.cc.Ctx, h.cc.MarklogicCluster)
	active := h.getConfigMap(t, "marklogic-haproxy")
	active.Data = h.desired
	if err := h.cc.Client.Update(h.cc.Ctx, active); err != nil {
		t.Fatal(err)
	}
	if err := h.cc.idleHAProxyCandidate(h.getConfigMap(t, haproxyCandidateConfigMap), h.desired["haproxy.cfg"]); err != nil {
		t.Fatal(err)
	}
	hash := haproxyreload.Hash([]byte(h.desired["haproxy.cfg"]))
	h.reportSteady(t, map[string]haproxyreload.PodStatus{
		"haproxy-1": {Phase: "active", Hash: hash}, "haproxy-2": {Phase: "active", Hash: hash},
	})
}

// rotateTLS changes the certificate in the spec; the committed configuration is unchanged.
func (h *reloadHarness) rotateTLS(t *testing.T, secret, file string) {
	t.Helper()
	h.updateCluster(t, func(cr *marklogicv1.MarklogicCluster) {
		cr.Spec.HAProxy.Tls = &marklogicv1.TlsForHAProxy{Enabled: true, SecretName: secret, CertFileName: file}
	})
	h.desired = generateHAProxyConfigMapData(h.cc.Ctx, h.cc.MarklogicCluster)
}

func (h *reloadHarness) deployment(t *testing.T) *appsv1.Deployment {
	t.Helper()
	deployment := &appsv1.Deployment{}
	if err := h.cc.Client.Get(h.cc.Ctx, types.NamespacedName{Name: "marklogic-haproxy", Namespace: "test"}, deployment); err != nil {
		t.Fatal(err)
	}
	return deployment
}

func certificateVolume(t *testing.T, deployment *appsv1.Deployment) corev1.Volume {
	t.Helper()
	for _, volume := range deployment.Spec.Template.Spec.Volumes {
		if volume.Name == "ssl-certificate" {
			return volume
		}
	}
	t.Fatal("deployment has no certificate volume")
	return corev1.Volume{}
}

// reconcile runs a full HAProxy reconcile and returns its result.
func (h *reloadHarness) reconcile(t *testing.T) (waiting bool, err error) {
	t.Helper()
	step := h.cc.ReconcileHAProxy()
	if !step.Completed() {
		return false, nil
	}
	outcome, err := step.Output()
	return outcome.RequeueAfter > 0, err
}

func TestHAProxyTLSRotationStagesBothSecretsUntilCommit(t *testing.T) {
	for _, newFile := range []string{"old.pem", "new.pem"} {
		for _, failValidation := range []bool{false, true} {
			t.Run(fmt.Sprintf("newFile=%s/failValidation=%v", newFile, failValidation), func(t *testing.T) {
				h := newReloadHarness(t)
				h.useTLS(t, "old-tls", "old.pem")
				oldConfig := h.desired["haproxy.cfg"]
				if waiting, err := h.reconcile(t); err != nil || !waiting {
					t.Fatalf("initial deployment: waiting=%v err=%v", waiting, err)
				}

				h.rotateTLS(t, "new-tls", newFile)
				if waiting, err := h.reconcile(t); err != nil || !waiting {
					t.Fatalf("rotation must first roll the pods onto a volume holding both Secrets: waiting=%v err=%v", waiting, err)
				}
				staged := certificateVolume(t, h.deployment(t))
				if staged.Projected == nil || len(staged.Projected.Sources) != 2 ||
					staged.Projected.Sources[0].Secret.Name != "old-tls" || staged.Projected.Sources[1].Secret.Name != "new-tls" {
					t.Fatalf("rotation did not retain both certificate Secrets: %+v", staged)
				}
				oldPath := staged.Projected.Sources[0].Secret.Items[0].Path
				newPath := staged.Projected.Sources[1].Secret.Items[0].Path
				if oldPath == newPath || !strings.Contains(oldConfig, "/"+oldPath) || !strings.Contains(h.desired["haproxy.cfg"], "/"+newPath) {
					t.Fatalf("staged certificate paths collide or do not match their configurations: %s %s", oldPath, newPath)
				}

				// Pods that have not been replaced yet cannot load the new certificate.
				if waiting, err := h.reconcile(t); err != nil || !waiting {
					t.Fatalf("the reload must wait for pods that mount both Secrets: waiting=%v err=%v", waiting, err)
				}
				if phase := h.getConfigMap(t, haproxyCandidateConfigMap).Data["phase"]; phase != "idle" {
					t.Fatalf("a candidate was published before every pod could read the new certificate: %s", phase)
				}
				if status := h.status(t); status == nil || !strings.Contains(status.Message, "certificate staging") {
					t.Fatalf("waiting for the staging rollout must be visible: %+v", status)
				}

				for _, name := range []string{"haproxy-1", "haproxy-2"} {
					pod := &corev1.Pod{}
					if err := h.cc.Client.Get(h.cc.Ctx, types.NamespacedName{Name: name, Namespace: "test"}, pod); err != nil {
						t.Fatal(err)
					}
					pod.Spec.Volumes = []corev1.Volume{staged}
					if err := h.cc.Client.Update(h.cc.Ctx, pod); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := h.reconcile(t); err != nil {
					t.Fatal(err)
				}
				candidate := h.getConfigMap(t, haproxyCandidateConfigMap)
				if candidate.Data["phase"] != "prepare" || candidate.Data["haproxy.cfg"] != h.desired["haproxy.cfg"] {
					t.Fatalf("the transaction did not start once every pod mounted both Secrets: %#v", candidate.Data)
				}
				keptOldCertificate := func() bool {
					return reflect.DeepEqual(certificateVolume(t, h.deployment(t)), staged)
				}
				if !keptOldCertificate() {
					t.Fatal("the old certificate was removed while the transaction was running")
				}

				hash := haproxyreload.Hash([]byte(h.desired["haproxy.cfg"]))
				if failValidation {
					h.reportPods(t, hash, map[string]string{"haproxy-1": "prepared", "haproxy-2": "failed"})
					if _, err := h.reconcile(t); err == nil {
						t.Fatal("a validation failure must be reported")
					}
					if h.getConfigMap(t, "marklogic-haproxy").Data["haproxy.cfg"] != oldConfig {
						t.Fatal("a failed rotation changed the committed configuration")
					}
					if !keptOldCertificate() {
						t.Fatal("a failed rotation discarded the old certificate, which the running configuration still uses")
					}
					return
				}
				h.reportPods(t, hash, map[string]string{"haproxy-1": "prepared", "haproxy-2": "prepared"})
				if _, err := h.reconcile(t); err != nil {
					t.Fatal(err)
				}
				if !keptOldCertificate() {
					t.Fatal("the old certificate was removed before every replica activated the new configuration")
				}
				h.reportPods(t, hash, map[string]string{"haproxy-1": "applied", "haproxy-2": "applied"})
				if _, err := h.reconcile(t); err != nil {
					t.Fatal(err)
				}
				if h.getConfigMap(t, "marklogic-haproxy").Data["haproxy.cfg"] != h.desired["haproxy.cfg"] {
					t.Fatal("the rotated configuration was not committed")
				}
				final := certificateVolume(t, h.deployment(t))
				if final.Secret == nil || final.Secret.SecretName != "new-tls" || final.Projected != nil {
					t.Fatalf("once committed, only the new Secret must remain mounted: %+v", final)
				}
			})
		}
	}
}

func TestHAProxyTLSConfigChangeIgnoresDefaultedVolumeMode(t *testing.T) {
	h := newReloadHarness(t)
	h.useTLS(t, "tls", "certificate.pem")
	if _, err := h.reconcile(t); err != nil {
		t.Fatal(err)
	}
	deployment := h.deployment(t)
	// The API server defaults this field; it must not look like a change.
	for i := range deployment.Spec.Template.Spec.Volumes {
		if secret := deployment.Spec.Template.Spec.Volumes[i].Secret; secret != nil {
			mode := int32(corev1.SecretVolumeSourceDefaultMode)
			secret.DefaultMode = &mode
		}
	}
	if err := h.cc.Client.Update(h.cc.Ctx, deployment); err != nil {
		t.Fatal(err)
	}
	before := h.deployment(t)
	if step := h.cc.ReconcileHAProxy(); step.Completed() {
		_, err := step.Output()
		t.Fatalf("a defaulted volume mode triggered a rollout: %v", err)
	}
	if !reflect.DeepEqual(h.deployment(t).Spec.Template, before.Spec.Template) {
		t.Fatal("a defaulted volume mode altered the pod template")
	}
}

func TestHAProxyDisablingTLSReloadsBeforeTheCertificateVolumeIsRemoved(t *testing.T) {
	h := newReloadHarness(t)
	h.useTLS(t, "tls", "certificate.pem")
	if _, err := h.reconcile(t); err != nil {
		t.Fatal(err)
	}
	// Turn TLS off: the running configuration still references the certificate files.
	h.updateCluster(t, func(cr *marklogicv1.MarklogicCluster) { cr.Spec.HAProxy.Tls = nil })
	h.desired = generateHAProxyConfigMapData(h.cc.Ctx, h.cc.MarklogicCluster)
	if _, err := h.reconcile(t); err != nil {
		t.Fatal(err)
	}
	if phase := h.getConfigMap(t, haproxyCandidateConfigMap).Data["phase"]; phase != "prepare" {
		t.Fatalf("the configuration must stop using the certificate before it is removed: %s", phase)
	}
	if certificateVolume(t, h.deployment(t)).Secret == nil {
		t.Fatal("the certificate volume was removed while the running configuration still needs it")
	}

	hash := haproxyreload.Hash([]byte(h.desired["haproxy.cfg"]))
	h.reportPods(t, hash, map[string]string{"haproxy-1": "prepared", "haproxy-2": "prepared"})
	if _, err := h.reconcile(t); err != nil {
		t.Fatal(err)
	}
	h.reportPods(t, hash, map[string]string{"haproxy-1": "applied", "haproxy-2": "applied"})
	if _, err := h.reconcile(t); err != nil {
		t.Fatal(err)
	}
	for _, volume := range h.deployment(t).Spec.Template.Spec.Volumes {
		if volume.Name == "ssl-certificate" {
			t.Fatal("the certificate volume must be removed once no configuration uses it")
		}
	}
}

func TestHAProxyDisablingTLSMigratesLegacyDeploymentBeforeReload(t *testing.T) {
	h := newReloadHarness(t)
	h.useTLS(t, "tls", "certificate.pem")
	meta := generateObjectMeta("marklogic-haproxy", "test", h.cc.GetHAProxyLabels("cluster"), nil)
	legacy := h.cc.createHAProxyDeploymentDef(meta)
	if err := h.cc.Client.Create(h.cc.Ctx, legacy); err != nil {
		t.Fatal(err)
	}

	h.updateCluster(t, func(cr *marklogicv1.MarklogicCluster) { cr.Spec.HAProxy.Tls = nil })
	h.desired = generateHAProxyConfigMapData(h.cc.Ctx, h.cc.MarklogicCluster)
	if waiting, err := h.reconcile(t); err != nil || !waiting {
		t.Fatalf("legacy deployment migration must roll out before reload: waiting=%v err=%v", waiting, err)
	}
	deployment := h.deployment(t)
	if len(deployment.Spec.Template.Spec.Containers) != 2 ||
		deployment.Spec.Template.Spec.Containers[1].Name != "haproxy-reloader" {
		t.Fatal("the legacy deployment did not gain the reloader before reload")
	}
	if certificateVolume(t, deployment).Secret == nil {
		t.Fatal("the certificate volume was removed during sidecar migration")
	}
	for _, container := range deployment.Spec.Template.Spec.Containers {
		found := false
		for _, mount := range container.VolumeMounts {
			found = found || mount.Name == certificateVolumeName
		}
		if !found {
			t.Fatalf("%s lost its certificate mount during migration", container.Name)
		}
	}
	if phase := h.getConfigMap(t, haproxyCandidateConfigMap).Data["phase"]; phase != "idle" {
		t.Fatalf("a reload started before the reloader rollout: %s", phase)
	}

	if waiting, err := h.reconcile(t); err != nil || !waiting {
		t.Fatalf("non-TLS reload did not start after migration: waiting=%v err=%v", waiting, err)
	}
	hash := haproxyreload.Hash([]byte(h.desired["haproxy.cfg"]))
	h.reportPods(t, hash, map[string]string{"haproxy-1": "prepared", "haproxy-2": "prepared"})
	if _, err := h.reconcile(t); err != nil {
		t.Fatal(err)
	}
	h.reportPods(t, hash, map[string]string{"haproxy-1": "applied", "haproxy-2": "applied"})
	if _, err := h.reconcile(t); err != nil {
		t.Fatal(err)
	}
	for _, volume := range h.deployment(t).Spec.Template.Spec.Volumes {
		if volume.Name == certificateVolumeName {
			t.Fatal("the certificate volume must be removed after the non-TLS configuration commits")
		}
	}
}
