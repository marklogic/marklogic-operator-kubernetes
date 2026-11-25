// Copyright (c) 2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package k8sutil

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cisco-open/k8s-objectmatcher/patch"
	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	"github.com/marklogic/marklogic-operator-kubernetes/pkg/haproxyreload"
	"github.com/marklogic/marklogic-operator-kubernetes/pkg/result"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	haproxyBootstrapAnnotation = "marklogic.progress.com/haproxy-bootstrap"
	haproxyCandidateConfigMap  = haproxyreload.CandidateMap
	haproxyReloadStatusMap     = haproxyreload.StatusMap
	haproxyReloaderAccount     = "marklogic-haproxy-reloader"

	// haproxyReloaderDefaultUser is the `haproxy` user of the haproxytech images. The
	// sidecar always runs as a numeric non-root user because it alone holds a
	// Kubernetes token, even though those images default to root.
	haproxyReloaderDefaultUser = int64(1000)
	// haproxyReloaderInstallerUser matches the non-root UID in the operator image.
	haproxyReloaderInstallerUser = int64(65532)
	// haproxyPhaseTimeout bounds how long a transaction phase may wait for replicas.
	// It must cover projected ConfigMap propagation plus validation and activation.
	haproxyPhaseTimeout   = 10 * time.Minute
	haproxyPhaseStartedAt = "phaseStartedAt"
	haproxyFailureReason  = "failure"
	// haproxyConvergenceAnnotation records, on the candidate ConfigMap, when replicas
	// first failed to report the committed configuration. The clock is independent
	// of the status message, which changes as replica diagnostics change.
	haproxyConvergenceAnnotation = "marklogic.progress.com/haproxy-convergence-started"
	// haproxyRejectedAnnotation remembers "<hash>|<generation>|<reason>" for a configuration
	// whose activation failed and was rolled back, so it is not applied to live traffic
	// again until the spec changes. It survives operator restarts.
	haproxyRejectedAnnotation = "marklogic.progress.com/haproxy-rejected"
)

// discoveredReloaderImage remembers the operator image found through the API. The
// running operator's image cannot change, so the lookup is needed at most once.
var discoveredReloaderImage struct {
	sync.Mutex
	image string
}

const reloaderImageHint = "set HAPROXY_RELOADER_IMAGE (Helm value controllerManager.manager.haproxyReloaderImage)"

// haproxyReloaderImage returns the operator image that provides the reloader binary.
// The Helm chart sets HAPROXY_RELOADER_IMAGE, so no API call is normally needed.
func (cc *ClusterContext) haproxyReloaderImage() (string, error) {
	if cc.HAProxyReloaderImage != "" {
		return cc.HAProxyReloaderImage, nil
	}
	if image := strings.TrimSpace(os.Getenv("HAPROXY_RELOADER_IMAGE")); image != "" {
		return image, nil
	}
	discoveredReloaderImage.Lock()
	defer discoveredReloaderImage.Unlock()
	if discoveredReloaderImage.image != "" {
		return discoveredReloaderImage.image, nil
	}
	name, namespace := os.Getenv("POD_NAME"), os.Getenv("POD_NAMESPACE")
	if name == "" || namespace == "" {
		return "", errors.New("cannot determine the operator image for the HAProxy reloader: " + reloaderImageHint +
			", or run the operator with POD_NAME and POD_NAMESPACE")
	}
	pod := &corev1.Pod{}
	if err := cc.haproxyReader().Get(cc.Ctx, types.NamespacedName{Name: name, Namespace: namespace}, pod); err != nil {
		return "", fmt.Errorf("cannot read the operator pod %s/%s to find its image (%s to avoid this lookup): %w",
			namespace, name, reloaderImageHint, err)
	}
	for _, container := range pod.Spec.Containers {
		if container.Name == "manager" && container.Image != "" {
			discoveredReloaderImage.image = container.Image
			return container.Image, nil
		}
	}
	return "", errors.New("the operator pod has no manager container image; " + reloaderImageHint)
}

func (cc *ClusterContext) configureHAProxyHotReload(deployment *appsv1.Deployment) error {
	image, err := cc.haproxyReloaderImage()
	if err != nil {
		return err
	}
	template := &deployment.Spec.Template
	container := &template.Spec.Containers[0]
	container.TerminationMessagePolicy = corev1.TerminationMessageFallbackToLogsOnError
	container.Command = []string{haproxyreload.Executable, "start"}
	container.VolumeMounts[0] = corev1.VolumeMount{
		Name: "haproxy-config", MountPath: "/usr/local/etc/haproxy", ReadOnly: true,
	}
	runtimeMount := corev1.VolumeMount{Name: "haproxy-runtime", MountPath: haproxyreload.RuntimeDir}
	// Only the init container may write the executable; HAProxy and the sidecar
	// get a read-only view so neither can replace code run by the other.
	binMount := corev1.VolumeMount{Name: "haproxy-reloader-bin", MountPath: haproxyreload.BinDir}
	readOnlyBinMount := binMount
	readOnlyBinMount.ReadOnly = true
	healthMount := corev1.VolumeMount{Name: "haproxy-reloader-health", MountPath: haproxyreload.HealthDir}
	container.VolumeMounts = append(container.VolumeMounts, runtimeMount, readOnlyBinMount)
	container.ReadinessProbe = &corev1.Probe{
		ProbeHandler:  corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{haproxyreload.Executable, "ready"}}},
		PeriodSeconds: 5, TimeoutSeconds: 5, FailureThreshold: 3,
	}
	template.Spec.ServiceAccountName = haproxyReloaderAccount
	template.Spec.AutomountServiceAccountToken = boolPtr(false)
	candidateMount := corev1.VolumeMount{Name: "haproxy-candidate", MountPath: haproxyreload.CandidateDir, ReadOnly: true}
	apiMount := corev1.VolumeMount{Name: "haproxy-reloader-api", MountPath: haproxyreload.APITokenDir, ReadOnly: true}
	groupID := int64(101)
	if container.SecurityContext != nil && container.SecurityContext.RunAsGroup != nil {
		groupID = *container.SecurityContext.RunAsGroup
	} else if template.Spec.SecurityContext.RunAsGroup != nil {
		groupID = *template.Spec.SecurityContext.RunAsGroup
	} else if template.Spec.SecurityContext.FSGroup != nil {
		groupID = *template.Spec.SecurityContext.FSGroup
	}
	container.Env = append(container.Env, corev1.EnvVar{Name: "HAPROXY_MASTER_GROUP", Value: fmt.Sprint(groupID)})
	// The sidecar runs the installed reloader from the HAProxy image so that
	// candidates are validated by the same HAProxy binary and libraries. It runs
	// as a non-root user even when the HAProxy container itself runs as root.
	reloaderUser := haproxyReloaderUser(template.Spec.SecurityContext, container.SecurityContext)
	template.Spec.Containers = append(template.Spec.Containers, corev1.Container{
		Name:    "haproxy-reloader",
		Image:   container.Image,
		Command: []string{haproxyreload.Executable, "watch"},
		Env: []corev1.EnvVar{
			{Name: "POD_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}},
			{Name: "POD_NAMESPACE", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"}}},
		},
		VolumeMounts: []corev1.VolumeMount{runtimeMount, readOnlyBinMount, candidateMount, apiMount, healthMount},
		LivenessProbe: &corev1.Probe{
			ProbeHandler:        corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{haproxyreload.Executable, "live"}}},
			InitialDelaySeconds: 30, PeriodSeconds: 30, TimeoutSeconds: 10, FailureThreshold: 3,
		},
		Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
			"cpu": resource.MustParse("10m"), "memory": resource.MustParse("32Mi"),
		}},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: boolPtr(false),
			ReadOnlyRootFilesystem:   boolPtr(true),
			RunAsNonRoot:             boolPtr(true),
			RunAsUser:                &reloaderUser,
			RunAsGroup:               int64Ptr(groupID),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
	})
	for _, mount := range container.VolumeMounts {
		if mount.Name == "ssl-certificate" {
			template.Spec.Containers[1].VolumeMounts = append(template.Spec.Containers[1].VolumeMounts, mount)
		}
	}
	template.Spec.InitContainers = []corev1.Container{{
		Name: "install-haproxy-reloader", Image: image,
		Command:      []string{"/haproxy-reloader", "install"},
		VolumeMounts: []corev1.VolumeMount{binMount},
		Resources:    corev1.ResourceRequirements{Requests: container.Resources.Requests.DeepCopy()},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: boolPtr(false), ReadOnlyRootFilesystem: boolPtr(true),
			RunAsNonRoot: boolPtr(true), RunAsUser: int64Ptr(haproxyReloaderInstallerUser),
			Capabilities:   &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
	}}
	template.Spec.Volumes = append(template.Spec.Volumes, corev1.Volume{
		Name: "haproxy-runtime", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	}, corev1.Volume{
		Name: "haproxy-reloader-bin", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	}, corev1.Volume{
		Name: "haproxy-reloader-health", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	}, corev1.Volume{
		Name: "haproxy-candidate",
		VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: haproxyCandidateConfigMap},
		}},
	}, corev1.Volume{
		Name: "haproxy-reloader-api",
		VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{
			{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Path: "token", ExpirationSeconds: int64Ptr(3600)}},
			{ConfigMap: &corev1.ConfigMapProjection{
				LocalObjectReference: corev1.LocalObjectReference{Name: "kube-root-ca.crt"},
				Items:                []corev1.KeyToPath{{Key: "ca.crt", Path: "ca.crt"}},
			}},
		}}},
	})
	return nil
}

// haproxyReloaderUser picks a non-root identity: an explicit non-root user of the
// HAProxy container or pod wins, otherwise the image's `haproxy` user.
func haproxyReloaderUser(pod *corev1.PodSecurityContext, container *corev1.SecurityContext) int64 {
	if container != nil && container.RunAsUser != nil && *container.RunAsUser != 0 {
		return *container.RunAsUser
	}
	if pod != nil && pod.RunAsUser != nil && *pod.RunAsUser != 0 {
		return *pod.RunAsUser
	}
	return haproxyReloaderDefaultUser
}

func (cc *ClusterContext) haproxyReader() client.Reader {
	if cc.APIReader != nil {
		return cc.APIReader
	}
	return cc.Client
}

func (cc *ClusterContext) checkHAProxyGeneration(ctx context.Context) error {
	current := &marklogicv1.MarklogicCluster{}
	if err := cc.haproxyReader().Get(ctx, client.ObjectKeyFromObject(cc.MarklogicCluster), current); err != nil {
		return err
	}
	if current.UID != cc.MarklogicCluster.UID || current.Generation != cc.MarklogicCluster.Generation || current.DeletionTimestamp != nil {
		return errors.New("cluster changed during HAProxy reload")
	}
	return nil
}

func (cc *ClusterContext) ensureHAProxyReloaderResources(config map[string]string, owner metav1.OwnerReference) error {
	namespace := cc.MarklogicCluster.Namespace
	account := &corev1.ServiceAccount{
		ObjectMeta:                   metav1.ObjectMeta{Name: haproxyReloaderAccount, Namespace: namespace},
		AutomountServiceAccountToken: boolPtr(false),
	}
	AddOwnerRefToObject(account, owner)
	if err := cc.Client.Create(cc.Ctx, account); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating HAProxy reloader ServiceAccount: %w", err)
	}

	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: haproxyReloaderAccount, Namespace: namespace},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{""}, Resources: []string{"configmaps"}, ResourceNames: []string{haproxyReloadStatusMap},
				Verbs: []string{"patch"},
			},
			{
				// Reading the candidate through the API is much faster than waiting for the
				// kubelet to refresh the mounted copy.
				APIGroups: []string{""}, Resources: []string{"configmaps"}, ResourceNames: []string{haproxyCandidateConfigMap},
				Verbs: []string{"get"},
			},
		},
	}
	AddOwnerRefToObject(role, owner)
	if err := cc.Client.Create(cc.Ctx, role); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("creating HAProxy reloader Role: %w", err)
		}
		current := &rbacv1.Role{}
		if err := cc.haproxyReader().Get(cc.Ctx, client.ObjectKeyFromObject(role), current); err != nil {
			return err
		}
		if !equality.Semantic.DeepEqual(current.Rules, role.Rules) {
			current.Rules = role.Rules
			if err := cc.Client.Update(cc.Ctx, current); err != nil {
				return fmt.Errorf("updating HAProxy reloader Role: %w", err)
			}
		}
	}

	binding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: haproxyReloaderAccount, Namespace: namespace},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: haproxyReloaderAccount},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: haproxyReloaderAccount, Namespace: namespace}},
	}
	AddOwnerRefToObject(binding, owner)
	if err := cc.Client.Create(cc.Ctx, binding); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("creating HAProxy reloader RoleBinding: %w", err)
		}
		current := &rbacv1.RoleBinding{}
		if err := cc.haproxyReader().Get(cc.Ctx, client.ObjectKeyFromObject(binding), current); err != nil {
			return err
		}
		if !equality.Semantic.DeepEqual(current.RoleRef, binding.RoleRef) ||
			!equality.Semantic.DeepEqual(current.Subjects, binding.Subjects) {
			return errors.New("existing HAProxy reloader RoleBinding has unexpected role or subjects")
		}
	}

	for _, desired := range []*corev1.ConfigMap{
		generateHAProxyConfigMap(
			generateObjectMeta(haproxyCandidateConfigMap, namespace, cc.GetHAProxyLabels(cc.MarklogicCluster.Name), nil),
			owner,
			map[string]string{
				"phase": "idle", "transaction": "bootstrap", "attempt": "0", "hash": haproxyreload.Hash([]byte(config["haproxy.cfg"])),
				"haproxy.cfg": config["haproxy.cfg"], "previous.cfg": config["haproxy.cfg"],
			},
		),
		generateHAProxyConfigMap(
			generateObjectMeta(haproxyReloadStatusMap, namespace, cc.GetHAProxyLabels(cc.MarklogicCluster.Name), nil),
			owner, map[string]string{},
		),
	} {
		if err := cc.Client.Create(cc.Ctx, desired); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("creating HAProxy reload coordination ConfigMap %s: %w", desired.Name, err)
		}
	}
	return nil
}

const certificateVolumeName = "ssl-certificate"

func findVolume(volumes []corev1.Volume, name string) int {
	for i := range volumes {
		if volumes[i].Name == name {
			return i
		}
	}
	return -1
}

func secretProjection(secret *corev1.SecretVolumeSource) corev1.VolumeProjection {
	return corev1.VolumeProjection{Secret: &corev1.SecretProjection{
		LocalObjectReference: corev1.LocalObjectReference{Name: secret.SecretName},
		Items:                secret.Items,
	}}
}

// stagedCertificateVolume keeps what is mounted now (one Secret, or the projection of a
// rotation that is already staged) next to the new Secret, so pods that are replaced
// while the configuration still references the old certificate can read both.
func stagedCertificateVolume(previous, desired corev1.Volume) corev1.Volume {
	sources := []corev1.VolumeProjection{}
	switch {
	case previous.Secret != nil:
		sources = append(sources, secretProjection(previous.Secret))
	case previous.Projected != nil:
		sources = append(sources, previous.Projected.Sources...)
	}
	next := secretProjection(desired.Secret)
	known := false
	for _, source := range sources {
		known = known || equality.Semantic.DeepEqual(source, next)
	}
	if !known {
		sources = append(sources, next)
	}
	mode := int32(corev1.ProjectedVolumeSourceDefaultMode)
	return corev1.Volume{Name: desired.Name, VolumeSource: corev1.VolumeSource{
		Projected: &corev1.ProjectedVolumeSource{Sources: sources, DefaultMode: &mode},
	}}
}

func everyPodMounts(pods []corev1.Pod, volume corev1.Volume) bool {
	for _, pod := range pods {
		if findVolumeEqual(pod.Spec.Volumes, volume) < 0 {
			return false
		}
	}
	return true
}

func findVolumeEqual(volumes []corev1.Volume, want corev1.Volume) int {
	for i := range volumes {
		if equality.Semantic.DeepEqual(volumes[i], want) {
			return i
		}
	}
	return -1
}

// rotateHAProxyCertificate sequences a certificate Secret change so that no pod ever
// lacks a file its configuration references: stage both Secrets, wait until every pod
// mounts them, reload, and only then (the caller's normal deployment update) drop the
// old one. It reports handled when the reconcile must return the given result now.
func (cc *ClusterContext) rotateHAProxyCertificate(desired, live *appsv1.Deployment, configmap *corev1.ConfigMap,
	data map[string]string, hash string) (result.ReconcileResult, bool) {
	if equality.Semantic.DeepEqual(configmap.Data, data) {
		return result.Continue(), false
	}
	want := findVolume(desired.Spec.Template.Spec.Volumes, certificateVolumeName)
	previous := findVolume(live.Spec.Template.Spec.Volumes, certificateVolumeName)
	if want < 0 || previous < 0 || desired.Spec.Template.Spec.Volumes[want].Secret == nil {
		return result.Continue(), false
	}
	wanted, current := desired.Spec.Template.Spec.Volumes[want], live.Spec.Template.Spec.Volumes[previous]
	if current.Secret != nil && current.Secret.SecretName == wanted.Secret.SecretName &&
		equality.Semantic.DeepEqual(current.Secret.Items, wanted.Secret.Items) {
		return result.Continue(), false
	}
	staged := stagedCertificateVolume(current, wanted)
	if !equality.Semantic.DeepEqual(current, staged) {
		// Roll the pods onto the staged volume first.
		desired.Spec.Template.Spec.Volumes[want] = staged
		return result.Continue(), false
	}
	pods := &corev1.PodList{}
	if err := cc.haproxyReader().List(cc.Ctx, pods, client.InNamespace(cc.MarklogicCluster.Namespace),
		client.MatchingLabels(getHAProxySelectorLabels(cc.MarklogicCluster.Name))); err != nil {
		return cc.failHAProxyReload(err, hash), true
	}
	if !everyPodMounts(pods.Items, staged) {
		if err := cc.setHAProxyReloadStatus("Pending", hash, "Waiting for HAProxy certificate staging rollout"); err != nil {
			return result.Error(err), true
		}
		return result.RequeueSoon(5), true
	}
	if step := cc.reconcileHAProxyReload(configmap, data); step.Completed() {
		return step, true
	}
	return result.Continue(), false
}

// reloadBeforeRemovingHAProxyCertificate handles turning TLS off: the running
// configuration still references the certificate files, so it must stop doing so
// before the volume that holds them goes away.
func (cc *ClusterContext) reloadBeforeRemovingHAProxyCertificate(desired, live *appsv1.Deployment, configmap *corev1.ConfigMap,
	data map[string]string) (result.ReconcileResult, bool) {
	if getSSLConfig(cc.MarklogicCluster.Spec.HAProxy.Tls) != "" || equality.Semantic.DeepEqual(configmap.Data, data) ||
		findVolume(live.Spec.Template.Spec.Volumes, certificateVolumeName) < 0 {
		return result.Continue(), false
	}
	if !hasHAProxyReloader(live) {
		liveSpec := &live.Spec.Template.Spec
		desiredSpec := &desired.Spec.Template.Spec
		if len(liveSpec.Containers) == 0 || len(desiredSpec.Containers) < 2 {
			return result.Error(errors.New("cannot stage HAProxy TLS migration without the reloader sidecar")), true
		}
		var certificateMount *corev1.VolumeMount
		for i := range liveSpec.Containers[0].VolumeMounts {
			if liveSpec.Containers[0].VolumeMounts[i].Name == certificateVolumeName {
				certificateMount = &liveSpec.Containers[0].VolumeMounts[i]
				break
			}
		}
		if certificateMount == nil {
			return result.Error(errors.New("legacy HAProxy TLS deployment has no certificate mount")), true
		}
		if findVolume(desiredSpec.Volumes, certificateVolumeName) < 0 {
			desiredSpec.Volumes = append(desiredSpec.Volumes, *liveSpec.Volumes[findVolume(liveSpec.Volumes, certificateVolumeName)].DeepCopy())
		}
		appendCertificateMount := func(container *corev1.Container) {
			for _, mount := range container.VolumeMounts {
				if mount.Name == certificateVolumeName {
					return
				}
			}
			container.VolumeMounts = append(container.VolumeMounts, *certificateMount.DeepCopy())
		}
		appendCertificateMount(&desiredSpec.Containers[0])
		appendCertificateMount(&desiredSpec.Containers[1])
		return result.Continue(), false
	}
	if step := cc.reconcileHAProxyReload(configmap, data); step.Completed() {
		return step, true
	}
	return result.Continue(), false
}

func hasHAProxyReloader(deployment *appsv1.Deployment) bool {
	for _, container := range deployment.Spec.Template.Spec.Containers {
		if container.Name == "haproxy-reloader" {
			return true
		}
	}
	return false
}

func (cc *ClusterContext) ReconcileHAProxy() result.ReconcileResult {
	cr := cc.MarklogicCluster
	data := generateHAProxyConfigMapData(cc.Ctx, cr)
	hash := haproxyreload.Hash([]byte(data["haproxy.cfg"]))
	fail := func(err error) result.ReconcileResult {
		cc.ReqLogger.Error(err, "Reconciling hot-reload HAProxy resources")
		return result.Error(errors.Join(err, cc.setHAProxyReloadStatus("Failed", hash, err.Error())))
	}
	if err := validateHAProxySpec(cr); err != nil {
		return fail(err)
	}
	objectMeta := generateObjectMeta("marklogic-haproxy", cr.Namespace, cc.GetHAProxyLabels(cr.Name), cc.GetClusterAnnotations())
	key := types.NamespacedName{Name: objectMeta.Name, Namespace: cr.Namespace}
	desiredDeployment := cc.createHAProxyDeploymentDef(objectMeta)
	if err := cc.configureHAProxyHotReload(desiredDeployment); err != nil {
		return fail(err)
	}
	configmap := &corev1.ConfigMap{}
	if err := cc.haproxyReader().Get(cc.Ctx, key, configmap); apierrors.IsNotFound(err) {
		configmap = generateHAProxyConfigMap(objectMeta, marklogicClusterAsOwner(cr), data).DeepCopy()
		if configmap.Annotations == nil {
			configmap.Annotations = map[string]string{}
		}
		configmap.Annotations[haproxyBootstrapAnnotation] = "pending"
		if err := cc.Client.Create(cc.Ctx, configmap); err != nil {
			return fail(err)
		}
	} else if err != nil {
		return fail(err)
	}
	owner := marklogicClusterAsOwner(cr)
	if err := cc.ensureHAProxyReloaderResources(configmap.Data, owner); err != nil {
		return fail(err)
	}
	deployment := &appsv1.Deployment{}
	rolling := false
	if err := cc.Client.Get(cc.Ctx, key, deployment); apierrors.IsNotFound(err) {
		if err := patch.DefaultAnnotator.SetLastAppliedAnnotation(desiredDeployment); err != nil {
			return fail(err)
		}
		if err := cc.Client.Create(cc.Ctx, desiredDeployment); err != nil {
			return fail(err)
		}
		rolling = true
	} else if err != nil {
		return fail(err)
	} else {
		if step, handled := cc.rotateHAProxyCertificate(desiredDeployment, deployment, configmap, data, hash); handled {
			return step
		}
		if step, handled := cc.reloadBeforeRemovingHAProxyCertificate(desiredDeployment, deployment, configmap, data); handled {
			return step
		}
		diff, err := patch.DefaultPatchMaker.Calculate(deployment, desiredDeployment, patch.IgnoreStatusFields())
		if err != nil {
			return fail(err)
		}
		if !diff.IsEmpty() {
			if err := patch.DefaultAnnotator.SetLastAppliedAnnotation(desiredDeployment); err != nil {
				return fail(err)
			}
			desiredDeployment.ResourceVersion = deployment.ResourceVersion
			if err := cc.Client.Update(cc.Ctx, desiredDeployment); err != nil {
				return fail(err)
			}
			rolling = true
		}
	}
	service := &corev1.Service{}
	desiredService := cc.generateHaproxyServiceDef(objectMeta)
	AddOwnerRefToObject(desiredService, marklogicClusterAsOwner(cr))
	serviceMissing := false
	if err := cc.Client.Get(cc.Ctx, key, service); apierrors.IsNotFound(err) {
		serviceMissing = true
	} else if err != nil {
		return fail(err)
	}
	if rolling {
		if serviceMissing {
			if err := cc.Client.Create(cc.Ctx, desiredService); err != nil {
				return fail(err)
			}
		}
		if err := cc.setHAProxyReloadStatus("Pending", hash, "Waiting for the HAProxy deployment rollout"); err != nil {
			return result.Error(err)
		}
		return result.RequeueSoon(5)
	}
	if step := cc.reconcileHAProxyReload(configmap, data); step.Completed() {
		return step
	}
	if serviceMissing {
		if err := cc.Client.Create(cc.Ctx, desiredService); err != nil {
			return fail(err)
		}
	} else {
		before := service.DeepCopy()
		service.Spec.Ports = desiredService.Spec.Ports
		service.Spec.Selector = desiredService.Spec.Selector
		service.Labels = desiredService.Labels
		service.Annotations = desiredService.Annotations
		// A merge patch preserves allocated ClusterIPs and other API defaults.
		if !equality.Semantic.DeepEqual(before, service) {
			if err := cc.Client.Patch(cc.Ctx, service, client.MergeFrom(before)); err != nil {
				return fail(err)
			}
		}
	}
	return result.Continue()
}

// reconcileHAProxyReload coordinates a pod-local prepare/commit/rollback transaction.
// The active ConfigMap remains the last cluster-wide commit until all replicas apply.
func (cc *ClusterContext) reconcileHAProxyReload(configmap *corev1.ConfigMap, desired map[string]string) result.ReconcileResult {
	run, step := cc.loadReloadRun(configmap, desired)
	if run == nil {
		return step
	}
	for _, stage := range []func() (result.ReconcileResult, bool){
		run.enforceDeadline, run.waitForReplicas, run.supersedeStaleCommit,
	} {
		if step, handled := stage(); handled {
			return step
		}
	}
	if run.needsNewTransaction() {
		return run.startOrFinishTransaction()
	}
	if err := run.collectAcknowledgements(); err != nil {
		return run.fail(err)
	}
	switch run.phase() {
	case "prepare":
		return run.onPrepare()
	case "commit":
		return run.onCommit()
	case "rollback":
		return run.onRollback()
	case "abort":
		if err := run.idle(run.committed()); err != nil {
			return run.fail(fmt.Errorf("resetting aborted HAProxy candidate: %w", err))
		}
		return result.Continue()
	default:
		return run.fail(fmt.Errorf("unknown HAProxy reload phase %q", run.phase()))
	}
}

// reloadRun is one reconcile pass of the reload protocol. It reads the shared state once
// (the candidate, the replicas' acknowledgements and the pods) and walks through the stages
// in order; a stage either handles the pass, returning its result, or lets the next run.
type reloadRun struct {
	cc         *ClusterContext
	configmap  *corev1.ConfigMap // the committed configuration
	desired    map[string]string
	target     string // the desired haproxy.cfg
	hash       string
	generation string
	candidate  *corev1.ConfigMap
	statusMap  *corev1.ConfigMap
	pods       []corev1.Pod
	reports    map[string]haproxyreload.PodStatus // acknowledgements of the current transaction
}

func (r *reloadRun) phase() string         { return r.candidate.Data["phase"] }
func (r *reloadRun) transaction() string   { return r.candidate.Data["transaction"] }
func (r *reloadRun) candidateHash() string { return r.candidate.Data["hash"] }
func (r *reloadRun) committed() string     { return r.configmap.Data["haproxy.cfg"] }

func (r *reloadRun) fail(err error) result.ReconcileResult {
	return r.cc.failHAProxyReload(err, r.hash)
}

// wait records a Pending status and asks to be called again.
func (r *reloadRun) wait(message string, seconds int) result.ReconcileResult {
	if err := r.cc.setHAProxyReloadStatus("Pending", r.hash, message); err != nil {
		return result.Error(err)
	}
	return result.RequeueSoon(seconds)
}

// idle ends the transaction, leaving the candidate describing the given committed configuration.
func (r *reloadRun) idle(committedConfig string) error {
	return r.cc.idleHAProxyCandidate(r.candidate, committedConfig)
}

// requestRollback tells every replica to return to the previous configuration.
func (r *reloadRun) requestRollback() error {
	next := r.candidate.DeepCopy()
	setHAProxyPhase(next.Data, "rollback")
	return r.cc.Client.Update(r.cc.Ctx, next)
}

func (cc *ClusterContext) loadReloadRun(configmap *corev1.ConfigMap, desired map[string]string) (*reloadRun, result.ReconcileResult) {
	target := desired["haproxy.cfg"]
	r := &reloadRun{
		cc: cc, configmap: configmap, desired: desired, target: target,
		hash: haproxyreload.Hash([]byte(target)), generation: fmt.Sprint(cc.MarklogicCluster.Generation),
	}
	if err := cc.checkHAProxyGeneration(cc.Ctx); err != nil {
		return nil, r.fail(err)
	}
	namespace := cc.MarklogicCluster.Namespace
	// Uncached reads: acting on a stale candidate causes conflicting updates, and the
	// acknowledgements are written by other processes.
	r.candidate, r.statusMap = &corev1.ConfigMap{}, &corev1.ConfigMap{}
	if err := cc.haproxyReader().Get(cc.Ctx, types.NamespacedName{Name: haproxyCandidateConfigMap, Namespace: namespace}, r.candidate); err != nil {
		return nil, r.fail(fmt.Errorf("reading HAProxy reload candidate: %w", err))
	}
	if err := cc.haproxyReader().Get(cc.Ctx, types.NamespacedName{Name: haproxyReloadStatusMap, Namespace: namespace}, r.statusMap); err != nil {
		return nil, r.fail(fmt.Errorf("reading HAProxy reload acknowledgements: %w", err))
	}
	pods := &corev1.PodList{}
	if err := cc.haproxyReader().List(cc.Ctx, pods, client.InNamespace(namespace),
		client.MatchingLabels(getHAProxySelectorLabels(cc.MarklogicCluster.Name))); err != nil {
		return nil, r.fail(err)
	}
	r.pods = pods.Items
	return r, nil
}

// enforceDeadline bounds every non-idle phase: a wedged or unreachable sidecar never
// acknowledges, which readiness cannot detect.
func (r *reloadRun) enforceDeadline() (result.ReconcileResult, bool) {
	if phase := r.phase(); phase != "prepare" && phase != "commit" && phase != "rollback" {
		return result.Continue(), false
	}
	started, err := time.Parse(time.RFC3339, r.candidate.Data[haproxyPhaseStartedAt])
	if err != nil {
		// A transaction without a timestamp predates deadlines; start its clock now.
		stamped := r.candidate.DeepCopy()
		stamped.Data[haproxyPhaseStartedAt] = time.Now().UTC().Format(time.RFC3339)
		if err := r.cc.Client.Update(r.cc.Ctx, stamped); err != nil {
			return r.fail(fmt.Errorf("recording HAProxy phase start: %w", err)), true
		}
		r.candidate = stamped
		return result.Continue(), false
	}
	if time.Since(started) > haproxyPhaseTimeout {
		return r.cc.expireHAProxyPhase(r.candidate, r.configmap, r.pods, r.statusMap, r.hash)
	}
	return result.Continue(), false
}

func podIsReady(pod *corev1.Pod) bool {
	ready := false
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			ready = condition.Status == corev1.ConditionTrue
		}
	}
	return ready && pod.DeletionTimestamp == nil
}

// startupFailure explains why a pod's HAProxy container failed to start, if it did.
func startupFailure(pod *corev1.Pod) error {
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name != "haproxy" {
			continue
		}
		// The pod-local reloader recovers interrupted activations in running containers;
		// wait for it to restore readiness.
		if status.State.Running != nil && pod.DeletionTimestamp == nil {
			continue
		}
		terminated := status.State.Terminated
		if terminated == nil {
			terminated = status.LastTerminationState.Terminated
		}
		if terminated != nil && terminated.ExitCode != 0 {
			return fmt.Errorf("HAProxy pod %s failed to start (exit %d): %s", pod.Name, terminated.ExitCode, terminated.Message)
		}
	}
	return nil
}

// waitForReplicas holds the protocol until every desired replica is ready, and corrects a
// bootstrap configuration that HAProxy refuses to start with.
func (r *reloadRun) waitForReplicas() (result.ReconcileResult, bool) {
	ready := len(r.pods) == int(r.cc.MarklogicCluster.Spec.HAProxy.ReplicaCount) && len(r.pods) > 0
	var startupErr error
	for i := range r.pods {
		isReady := podIsReady(&r.pods[i])
		if !isReady && r.configmap.Annotations[haproxyBootstrapAnnotation] == "pending" {
			if err := startupFailure(&r.pods[i]); err != nil {
				startupErr = err
			}
		}
		ready = ready && isReady
	}
	switch {
	case ready:
		return result.Continue(), false
	case startupErr != nil && !equality.Semantic.DeepEqual(r.configmap.Data, r.desired):
		return r.correctBootstrap(), true
	case startupErr != nil:
		return r.fail(startupErr), true
	default:
		return r.wait("Waiting for all HAProxy replicas to be ready", 5), true
	}
}

// correctBootstrap replaces a bootstrap configuration that HAProxy cannot start with.
func (r *reloadRun) correctBootstrap() result.ReconcileResult {
	// Idle sidecars converge on the candidate, so it must describe the correction before
	// the startup ConfigMap does.
	if r.phase() == "idle" {
		idle := r.candidate.DeepCopy()
		idle.Data["hash"], idle.Data["haproxy.cfg"], idle.Data["previous.cfg"] = r.hash, r.target, r.target
		if err := r.cc.Client.Update(r.cc.Ctx, idle); err != nil {
			return r.fail(fmt.Errorf("correcting bootstrap HAProxy candidate: %w", err))
		}
	}
	next := r.configmap.DeepCopy()
	next.Data = r.desired
	if err := r.cc.Client.Update(r.cc.Ctx, next); err != nil {
		return r.fail(fmt.Errorf("correcting invalid bootstrap HAProxy configuration: %w", err))
	}
	return r.wait("Waiting for HAProxy to start with the corrected bootstrap configuration", 5)
}

// supersedeStaleCommit rolls back a commit that belongs to an older generation of the spec.
func (r *reloadRun) supersedeStaleCommit() (result.ReconcileResult, bool) {
	generation := r.candidate.Data["generation"]
	if r.transaction() == "" || generation == "" || generation == r.generation || r.phase() != "commit" {
		return result.Continue(), false
	}
	if err := r.requestRollback(); err != nil {
		return r.fail(fmt.Errorf("rolling back superseded HAProxy transaction: %w", err)), true
	}
	return r.wait("Rolling back a superseded HAProxy configuration before applying the latest generation", 2), true
}

// needsNewTransaction is true when nothing is in flight for the desired configuration.
func (r *reloadRun) needsNewTransaction() bool {
	phase := r.phase()
	return phase == "" || phase == "idle" || r.transaction() == "" || (r.candidateHash() != r.hash && phase != "rollback")
}

func (r *reloadRun) startOrFinishTransaction() result.ReconcileResult {
	if equality.Semantic.DeepEqual(r.configmap.Data, r.desired) {
		return r.finishConverged()
	}
	if reason, rejected := r.cc.rejectedHAProxyConfiguration(r.candidate, r.hash); rejected && r.phase() == "idle" {
		// Replicas keep running the previous configuration; applying the same
		// configuration again would only repeat the disruption.
		message := reason + " (not retried until the spec changes; delete ConfigMap " + haproxyCandidateConfigMap + " to retry now)"
		if err := r.cc.setHAProxyReloadStatus("Failed", r.hash, message); err != nil {
			return result.Error(err)
		}
		return result.Done()
	}
	return r.publishCandidate()
}

// finishConverged runs when the committed configuration already is the desired one: it
// succeeds only once every replica reports running it.
func (r *reloadRun) finishConverged() result.ReconcileResult {
	if r.phase() != "idle" {
		// The spec returned to the committed configuration while a transaction for another
		// one was left behind (for example a failed validation). Replicas resume
		// steady-state reporting only once the candidate is idle again.
		if err := r.idle(r.committed()); err != nil {
			return r.fail(fmt.Errorf("retiring an obsolete HAProxy transaction: %w", err))
		}
		return r.wait("Waiting for HAProxy replicas to report the committed configuration", 2)
	}
	// The ConfigMap only records what was committed; each replica must also report that it
	// runs it (for example after a container restart).
	if diverged := haproxyDivergedPods(r.pods, r.statusMap, r.hash); len(diverged) > 0 {
		return r.cc.waitForHAProxyConvergence(r.candidate, r.hash,
			"Waiting for HAProxy replicas to run the committed configuration: "+strings.Join(diverged, ", "))
	}
	if _, waiting := r.candidate.Annotations[haproxyConvergenceAnnotation]; waiting {
		cleared := r.candidate.DeepCopy()
		delete(cleared.Annotations, haproxyConvergenceAnnotation)
		if err := r.cc.Client.Update(r.cc.Ctx, cleared); err != nil {
			return r.fail(fmt.Errorf("clearing HAProxy convergence marker: %w", err))
		}
	}
	r.cc.pruneHAProxyStatus(r.statusMap, r.pods)
	if r.configmap.Annotations[haproxyBootstrapAnnotation] != "" {
		committed := r.configmap.DeepCopy()
		delete(committed.Annotations, haproxyBootstrapAnnotation)
		if err := r.cc.Client.Update(r.cc.Ctx, committed); err != nil {
			return r.fail(fmt.Errorf("clearing HAProxy bootstrap marker: %w", err))
		}
	}
	if err := r.cc.setHAProxyReloadStatus("Success", r.hash, "Configuration active on all HAProxy replicas"); err != nil {
		return result.Error(err)
	}
	return result.Continue()
}

// publishCandidate starts a transaction for the desired configuration.
func (r *reloadRun) publishCandidate() result.ReconcileResult {
	attempt, err := strconv.Atoi(r.candidate.Data["attempt"])
	if err != nil {
		return r.fail(fmt.Errorf("invalid HAProxy transaction attempt: %w", err))
	}
	attempt++
	next := r.candidate.DeepCopy()
	next.Data = map[string]string{
		"transaction": fmt.Sprintf("%s-%s-%d", r.generation, r.hash[:12], attempt),
		"generation":  r.generation, "attempt": strconv.Itoa(attempt), "hash": r.hash, "haproxy.cfg": r.target,
		"previous.cfg": r.committed(),
	}
	setHAProxyPhase(next.Data, "prepare")
	if err := r.cc.Client.Update(r.cc.Ctx, next); err != nil {
		return r.fail(fmt.Errorf("publishing HAProxy candidate: %w", err))
	}
	return r.wait("Validating candidate on all HAProxy replicas", 2)
}

// collectAcknowledgements gathers the replicas' reports for the current transaction.
func (r *reloadRun) collectAcknowledgements() error {
	r.reports = make(map[string]haproxyreload.PodStatus, len(r.pods))
	expectedHash := r.candidateHash()
	if r.phase() == "rollback" {
		expectedHash = haproxyreload.Hash([]byte(r.candidate.Data["previous.cfg"]))
	}
	for _, pod := range r.pods {
		var report haproxyreload.PodStatus
		if raw := r.statusMap.Data[pod.Name]; raw != "" {
			if err := json.Unmarshal([]byte(raw), &report); err != nil {
				return fmt.Errorf("invalid HAProxy reload acknowledgement from pod %s: %w", pod.Name, err)
			}
		}
		if report.Transaction == r.transaction() && (report.Hash == r.candidateHash() || report.Hash == expectedHash) {
			r.reports[pod.Name] = report
		}
	}
	return nil
}

func (r *reloadRun) allReported(want string) bool {
	for _, pod := range r.pods {
		if r.reports[pod.Name].Phase != want {
			return false
		}
	}
	return true
}

// describe names the first replica, in name order, that reported the given phase.
func (r *reloadRun) describe(prefix, phase string) (message string, found bool) {
	for _, name := range slices.Sorted(maps.Keys(r.reports)) {
		if report := r.reports[name]; report.Phase == phase {
			return fmt.Sprintf("%s on pod %s: %s", prefix, name, report.Message), true
		}
	}
	return prefix, false
}

func (r *reloadRun) onPrepare() result.ReconcileResult {
	if message, failed := r.describe("HAProxy candidate validation failed", "failed"); failed {
		if err := r.idle(r.committed()); err != nil {
			return r.fail(fmt.Errorf("aborting invalid HAProxy candidate: %w", err))
		}
		return r.fail(errors.New(message))
	}
	if !r.allReported("prepared") {
		return r.wait("Waiting for HAProxy candidate validation on all replicas", 2)
	}
	if err := r.cc.checkHAProxyGeneration(r.cc.Ctx); err != nil {
		return r.fail(err)
	}
	next := r.candidate.DeepCopy()
	setHAProxyPhase(next.Data, "commit")
	if err := r.cc.Client.Update(r.cc.Ctx, next); err != nil {
		return r.fail(fmt.Errorf("committing HAProxy candidate: %w", err))
	}
	return r.wait("Applying validated configuration on all HAProxy replicas", 2)
}

func (r *reloadRun) onCommit() result.ReconcileResult {
	if message, failed := r.describe("HAProxy activation failed", "failed"); failed {
		if err := r.requestRollback(); err != nil {
			return r.fail(fmt.Errorf("starting HAProxy rollback: %w", err))
		}
		return r.wait(message+"; rolling back replicas", 2)
	}
	if !r.allReported("applied") {
		return result.RequeueSoon(2)
	}
	if err := r.cc.checkHAProxyGeneration(r.cc.Ctx); err != nil {
		if updateErr := r.requestRollback(); updateErr != nil {
			return r.fail(errors.Join(err, updateErr))
		}
		return result.RequeueSoon(2)
	}
	if err := r.cc.commitHAProxyConfigMap(r.configmap, r.desired); err != nil {
		if updateErr := r.requestRollback(); updateErr != nil {
			return r.fail(errors.Join(fmt.Errorf("committing HAProxy ConfigMap: %w", err), updateErr))
		}
		message := fmt.Sprintf("committing HAProxy ConfigMap failed; rolling back replicas: %v", err)
		if statusErr := r.cc.setHAProxyReloadStatus("Pending", r.hash, message); statusErr != nil {
			return result.Error(errors.Join(err, statusErr))
		}
		return result.RequeueSoon(2)
	}
	if err := r.idle(r.target); err != nil {
		message := fmt.Sprintf("configuration is active; finalizing HAProxy transaction: %v", err)
		if statusErr := r.cc.setHAProxyReloadStatus("Pending", r.hash, message); statusErr != nil {
			return result.Error(errors.Join(err, statusErr))
		}
		return result.RequeueSoon(2)
	}
	if err := r.cc.setHAProxyReloadStatus("Success", r.hash, "Configuration active on all HAProxy replicas"); err != nil {
		return result.Error(err)
	}
	return result.Continue()
}

func (r *reloadRun) onRollback() result.ReconcileResult {
	if message, failed := r.describe("HAProxy rollback failed", "rollback-failed"); failed {
		// The sidecar keeps retrying; surface why this replica cannot be restored.
		if err := r.cc.setHAProxyReloadStatus("Failed", r.hash, message); err != nil {
			return result.Error(err)
		}
		return result.RequeueSoon(2)
	}
	if !r.allReported("rolledback") {
		return result.RequeueSoon(2)
	}
	if r.candidate.Data["generation"] != r.generation {
		if err := r.idle(r.committed()); err != nil {
			return r.fail(fmt.Errorf("resetting rolled-back HAProxy candidate: %w", err))
		}
		return result.RequeueSoon(1)
	}
	const restored = "; all replicas were restored to the previous configuration"
	if timeout := r.candidate.Data[haproxyFailureReason]; timeout != "" {
		// An unresponsive replica is a transient cause: retry with backoff instead of
		// requiring a manual reset once it recovers.
		if err := r.idle(r.committed()); err != nil {
			return r.fail(fmt.Errorf("resetting rolled-back HAProxy candidate: %w", err))
		}
		return r.fail(errors.New(timeout + restored))
	}
	message := "HAProxy activation failed" + restored
	if err := r.cc.rejectHAProxyCandidate(r.candidate, r.committed(), r.candidateHash(), message); err != nil {
		return r.fail(fmt.Errorf("resetting rolled-back HAProxy candidate: %w", err))
	}
	return r.fail(errors.New(message))
}

// haproxyDivergedPods lists replicas that have not reported running the given
// configuration. A replica counts as converged when its own report carries the
// desired hash after an activation, a rollback, or in steady state.
func haproxyDivergedPods(pods []corev1.Pod, statusMap *corev1.ConfigMap, hash string) []string {
	var diverged []string
	for _, pod := range pods {
		var report haproxyreload.PodStatus
		raw := statusMap.Data[pod.Name]
		if raw == "" || json.Unmarshal([]byte(raw), &report) != nil {
			diverged = append(diverged, pod.Name+" (no report)")
			continue
		}
		switch {
		case report.Hash == hash && (report.Phase == "active" || report.Phase == "applied" || report.Phase == "rolledback"):
		case report.Phase == "diverged" && report.Message != "":
			diverged = append(diverged, fmt.Sprintf("%s (%s)", pod.Name, strings.TrimSpace(report.Message)))
		default:
			diverged = append(diverged, pod.Name)
		}
	}
	sort.Strings(diverged)
	return diverged
}

// waitForHAProxyConvergence reports Pending while replicas converge and escalates
// to Failed, naming them, if they still have not after haproxyPhaseTimeout.
func (cc *ClusterContext) waitForHAProxyConvergence(candidate *corev1.ConfigMap, hash, message string) result.ReconcileResult {
	var started time.Time
	if recorded, ok := strings.CutPrefix(candidate.Annotations[haproxyConvergenceAnnotation], hash+"|"); ok {
		started, _ = time.Parse(time.RFC3339, recorded)
	}
	if started.IsZero() {
		started = time.Now()
		next := candidate.DeepCopy()
		if next.Annotations == nil {
			next.Annotations = map[string]string{}
		}
		next.Annotations[haproxyConvergenceAnnotation] = hash + "|" + started.UTC().Format(time.RFC3339)
		if err := cc.Client.Update(cc.Ctx, next); err != nil {
			return cc.failHAProxyReload(fmt.Errorf("recording HAProxy convergence start: %w", err), hash)
		}
	}
	if time.Since(started) > haproxyPhaseTimeout {
		return cc.failHAProxyReload(fmt.Errorf("%s (not converged after %s)", message, haproxyPhaseTimeout), hash)
	}
	if err := cc.setHAProxyReloadStatus("Pending", hash, message); err != nil {
		return result.Error(err)
	}
	return result.RequeueSoon(5)
}

func (cc *ClusterContext) failHAProxyReload(err error, hash string) result.ReconcileResult {
	if apierrors.IsConflict(err) {
		// An optimistic-concurrency conflict is not a failed reload: retry quietly.
		cc.ReqLogger.Info("HAProxy reload hit a concurrent update; retrying", "configurationHash", hash, "error", err.Error())
		return result.RequeueSoon(1)
	}
	cc.ReqLogger.Error(err, "HAProxy hot reload failed", "configurationHash", hash)
	return result.Error(errors.Join(err, cc.setHAProxyReloadStatus("Failed", hash, err.Error())))
}

// setHAProxyPhase moves a transaction to a phase and restarts its deadline.
func setHAProxyPhase(data map[string]string, phase string) {
	data["phase"] = phase
	data[haproxyPhaseStartedAt] = time.Now().UTC().Format(time.RFC3339)
}

// idleHAProxyCandidate ends a transaction, leaving the candidate describing the
// configuration that is committed on every replica.
func (cc *ClusterContext) idleHAProxyCandidate(candidate *corev1.ConfigMap, committedConfig string) error {
	next := candidate.DeepCopy()
	delete(next.Annotations, haproxyRejectedAnnotation)
	next.Data = map[string]string{
		"phase": "idle", "transaction": candidate.Data["transaction"], "generation": fmt.Sprint(cc.MarklogicCluster.Generation),
		"attempt": candidate.Data["attempt"], "hash": haproxyreload.Hash([]byte(committedConfig)),
		"haproxy.cfg": committedConfig, "previous.cfg": committedConfig,
	}
	return cc.Client.Update(cc.Ctx, next)
}

// rejectHAProxyCandidate ends a transaction whose activation failed after the
// replicas were restored, and records the rejected configuration so that it is not
// applied to live traffic again until the spec changes.
func (cc *ClusterContext) rejectHAProxyCandidate(candidate *corev1.ConfigMap, committedConfig, rejectedHash, message string) error {
	next := candidate.DeepCopy()
	next.Data = map[string]string{
		"phase": "idle", "transaction": candidate.Data["transaction"], "generation": fmt.Sprint(cc.MarklogicCluster.Generation),
		"attempt": candidate.Data["attempt"], "hash": haproxyreload.Hash([]byte(committedConfig)),
		"haproxy.cfg": committedConfig, "previous.cfg": committedConfig,
	}
	if next.Annotations == nil {
		next.Annotations = map[string]string{}
	}
	next.Annotations[haproxyRejectedAnnotation] = fmt.Sprintf("%s|%d|%s", rejectedHash, cc.MarklogicCluster.Generation, message)
	return cc.Client.Update(cc.Ctx, next)
}

// rejectedHAProxyConfiguration returns the recorded reason when this exact
// configuration was already rejected at the current generation.
func (cc *ClusterContext) rejectedHAProxyConfiguration(candidate *corev1.ConfigMap, hash string) (string, bool) {
	parts := strings.SplitN(candidate.Annotations[haproxyRejectedAnnotation], "|", 3)
	if len(parts) != 3 || parts[0] != hash || parts[1] != fmt.Sprint(cc.MarklogicCluster.Generation) {
		return "", false
	}
	return parts[2], true
}

// commitHAProxyConfigMap records the activated configuration as the committed one.
// A conflict must not be mistaken for a failed commit, which would roll back replicas
// that already run the new configuration, so it is retried against a fresh copy.
func (cc *ClusterContext) commitHAProxyConfigMap(current *corev1.ConfigMap, desired map[string]string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		next := current.DeepCopy()
		next.Data = desired
		delete(next.Annotations, haproxyBootstrapAnnotation)
		delete(next.Annotations, "kubectl.kubernetes.io/last-applied-configuration")
		err := cc.Client.Update(cc.Ctx, next)
		if apierrors.IsConflict(err) {
			fresh := &corev1.ConfigMap{}
			if getErr := cc.haproxyReader().Get(cc.Ctx, client.ObjectKeyFromObject(current), fresh); getErr != nil {
				return getErr
			}
			current = fresh
		}
		return err
	})
}

// pruneHAProxyStatus removes acknowledgements of pods that no longer exist, so the
// ConfigMap does not grow with every rollout. It is best effort: a merge patch only
// touches the stale keys and cannot conflict with sidecars writing their own entries.
func (cc *ClusterContext) pruneHAProxyStatus(statusMap *corev1.ConfigMap, pods []corev1.Pod) {
	live := make(map[string]bool, len(pods))
	for _, pod := range pods {
		live[pod.Name] = true
	}
	before := statusMap.DeepCopy()
	var stale []string
	for name := range statusMap.Data {
		if !live[name] {
			stale = append(stale, name)
			delete(statusMap.Data, name)
		}
	}
	if len(stale) == 0 {
		return
	}
	sort.Strings(stale)
	if err := cc.Client.Patch(cc.Ctx, statusMap, client.MergeFrom(before)); err != nil {
		cc.ReqLogger.Error(err, "pruning HAProxy reload acknowledgements of deleted pods", "pods", stale)
	}
}

// expireHAProxyPhase handles a phase that outlived its deadline while replicas
// still have not acknowledged it. It reports false when nothing is overdue, so
// normal processing continues (for example when every replica did acknowledge).
func (cc *ClusterContext) expireHAProxyPhase(candidate, configmap *corev1.ConfigMap, pods []corev1.Pod,
	statusMap *corev1.ConfigMap, hash string) (result.ReconcileResult, bool) {
	phase, transaction := candidate.Data["phase"], candidate.Data["transaction"]
	want := map[string]string{"prepare": "prepared", "commit": "applied", "rollback": "rolledback"}[phase]
	var waiting []string
	for _, pod := range pods {
		var report haproxyreload.PodStatus
		if raw := statusMap.Data[pod.Name]; raw != "" {
			_ = json.Unmarshal([]byte(raw), &report)
		}
		if report.Transaction == transaction {
			if report.Phase == want {
				continue
			}
			if phase != "rollback" && report.Phase == "failed" {
				// A reported failure is already handled by the normal transition.
				return result.Continue(), false
			}
		}
		waiting = append(waiting, pod.Name)
	}
	if len(waiting) == 0 {
		return result.Continue(), false
	}
	sort.Strings(waiting)
	pending := strings.Join(waiting, ", ")
	switch phase {
	case "prepare":
		if err := cc.idleHAProxyCandidate(candidate, configmap.Data["haproxy.cfg"]); err != nil {
			return cc.failHAProxyReload(fmt.Errorf("aborting timed-out HAProxy candidate: %w", err), hash), true
		}
		return cc.failHAProxyReload(fmt.Errorf("timed out after %s waiting for HAProxy pods to validate the candidate configuration: %s",
			haproxyPhaseTimeout, pending), hash), true
	case "commit":
		next := candidate.DeepCopy()
		setHAProxyPhase(next.Data, "rollback")
		next.Data[haproxyFailureReason] = fmt.Sprintf("timed out after %s waiting for HAProxy pods to apply the configuration: %s",
			haproxyPhaseTimeout, pending)
		if err := cc.Client.Update(cc.Ctx, next); err != nil {
			return cc.failHAProxyReload(fmt.Errorf("starting HAProxy rollback after timeout: %w", err), hash), true
		}
		if err := cc.setHAProxyReloadStatus("Pending", hash, next.Data[haproxyFailureReason]+"; rolling back replicas"); err != nil {
			return result.Error(err), true
		}
		return result.RequeueSoon(2), true
	default:
		// Replicas may still hold the candidate, so the transaction cannot be closed.
		return cc.failHAProxyReload(fmt.Errorf("timed out after %s waiting for HAProxy pods to restore the previous configuration: %s; "+
			"the sidecar keeps retrying", haproxyPhaseTimeout, pending), hash), true
	}
}

func (cc *ClusterContext) setHAProxyReloadStatus(phase, hash, message string) error {
	// Validation output is bounded before putting it into Kubernetes status/events.
	if len(message) > 4096 {
		message = message[:4096]
	}
	changed := false
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current := &marklogicv1.MarklogicCluster{}
		if err := cc.haproxyReader().Get(cc.Ctx, client.ObjectKeyFromObject(cc.MarklogicCluster), current); err != nil {
			return err
		}
		if current.UID != cc.MarklogicCluster.UID || current.Generation != cc.MarklogicCluster.Generation {
			return errors.New("cluster changed before recording HAProxy reload status")
		}
		old := current.Status.HAProxyReload
		if old != nil && old.Phase == phase && old.ConfigurationHash == hash && old.Message == message &&
			old.ObservedGeneration == current.Generation {
			return nil
		}
		current.Status.HAProxyReload = &marklogicv1.HAProxyReloadStatus{
			Phase: phase, ConfigurationHash: hash, ObservedGeneration: current.Generation,
			LastAttemptTime: metav1.Now(), Message: message,
		}
		conditionStatus := metav1.ConditionFalse
		if phase == "Success" {
			conditionStatus = metav1.ConditionTrue
		}
		meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
			Type: "HAProxyReloadReady", Status: conditionStatus, ObservedGeneration: current.Generation,
			Reason: "Reload" + phase, Message: message,
		})
		if err := cc.Client.Status().Update(cc.Ctx, current); err != nil {
			return err
		}
		changed = true
		return nil
	})
	if err != nil {
		return err
	}
	if changed {
		cc.ReqLogger.Info("HAProxy reload status", "phase", phase, "configurationHash", hash, "message", message)
		if cc.Recorder != nil {
			eventType := corev1.EventTypeNormal
			if phase == "Failed" {
				eventType = corev1.EventTypeWarning
			}
			cc.Recorder.Event(cc.MarklogicCluster, eventType, "HAProxyReload"+phase, strings.TrimSpace(message))
		}
	}
	return nil
}
