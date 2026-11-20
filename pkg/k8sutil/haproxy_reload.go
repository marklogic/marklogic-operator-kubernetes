// Copyright (c) 2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package k8sutil

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

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
	haproxyCandidateConfigMap  = "marklogic-haproxy-candidate"
	haproxyReloadStatusMap     = haproxyreload.StatusMap
	haproxyReloaderAccount     = "marklogic-haproxy-reloader"
)

func (cc *ClusterContext) configureHAProxyHotReload(deployment *appsv1.Deployment) error {
	image := cc.HAProxyReloaderImage
	if image == "" {
		image = os.Getenv("HAPROXY_RELOADER_IMAGE")
	}
	if image == "" {
		name, namespace := os.Getenv("POD_NAME"), os.Getenv("POD_NAMESPACE")
		if name == "" || namespace == "" {
			return errors.New("hot reload requires POD_NAME and POD_NAMESPACE, or HAPROXY_RELOADER_IMAGE when running the operator locally")
		}
		pod := &corev1.Pod{}
		if err := cc.haproxyReader().Get(cc.Ctx, types.NamespacedName{Name: name, Namespace: namespace}, pod); err != nil {
			return fmt.Errorf("discovering operator image for HAProxy reloader: %w", err)
		}
		for _, container := range pod.Spec.Containers {
			if container.Name == "manager" {
				image = container.Image
				break
			}
		}
		if image == "" {
			return errors.New("operator pod has no manager container image")
		}
	}
	template := &deployment.Spec.Template
	container := &template.Spec.Containers[0]
	container.TerminationMessagePolicy = corev1.TerminationMessageFallbackToLogsOnError
	container.Command = []string{haproxyreload.Executable, "start"}
	container.VolumeMounts[0] = corev1.VolumeMount{
		Name: "haproxy-config", MountPath: "/usr/local/etc/haproxy", ReadOnly: true,
	}
	runtimeMount := corev1.VolumeMount{Name: "haproxy-runtime", MountPath: haproxyreload.RuntimeDir}
	container.VolumeMounts = append(container.VolumeMounts, runtimeMount)
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
	template.Spec.Containers = append(template.Spec.Containers, corev1.Container{
		Name:    "haproxy-reloader",
		Image:   image,
		Command: []string{haproxyreload.Executable, "watch"},
		Env: []corev1.EnvVar{
			{Name: "POD_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}},
			{Name: "POD_NAMESPACE", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"}}},
		},
		VolumeMounts: []corev1.VolumeMount{runtimeMount, candidateMount, apiMount},
		Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
			"cpu": resource.MustParse("10m"), "memory": resource.MustParse("32Mi"),
		}},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: boolPtr(false),
			ReadOnlyRootFilesystem:   boolPtr(true),
			RunAsNonRoot:             boolPtr(true),
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
		VolumeMounts: []corev1.VolumeMount{runtimeMount},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: boolPtr(false), ReadOnlyRootFilesystem: boolPtr(true),
			RunAsNonRoot:   boolPtr(true),
			Capabilities:   &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
	}}
	template.Spec.Volumes = append(template.Spec.Volumes, corev1.Volume{
		Name: "haproxy-runtime", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
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
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{""}, Resources: []string{"configmaps"}, ResourceNames: []string{haproxyReloadStatusMap},
			Verbs: []string{"patch"},
		}},
	}
	AddOwnerRefToObject(role, owner)
	if err := cc.Client.Create(cc.Ctx, role); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("creating HAProxy reloader Role: %w", err)
		}
		current := &rbacv1.Role{}
		if err := cc.Client.Get(cc.Ctx, client.ObjectKeyFromObject(role), current); err != nil {
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
		if err := cc.Client.Get(cc.Ctx, client.ObjectKeyFromObject(binding), current); err != nil {
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

func (cc *ClusterContext) ReconcileHAProxy() result.ReconcileResult {
	cr := cc.MarklogicCluster
	data := generateHAProxyConfigMapData(cc.Ctx, cr)
	hash := haproxyreload.Hash([]byte(data["haproxy.cfg"]))
	fail := func(err error) result.ReconcileResult {
		cc.ReqLogger.Error(err, "Reconciling hot-reload HAProxy resources")
		return result.Error(errors.Join(err, cc.setHAProxyReloadStatus("Failed", hash, err.Error())))
	}
	objectMeta := generateObjectMeta("marklogic-haproxy", cr.Namespace, cc.GetHAProxyLabels(cr.Name), cc.GetClusterAnnotations())
	key := types.NamespacedName{Name: objectMeta.Name, Namespace: cr.Namespace}
	desiredDeployment := cc.createHAProxyDeploymentDef(objectMeta)
	if err := cc.configureHAProxyHotReload(desiredDeployment); err != nil {
		return fail(err)
	}
	configmap := &corev1.ConfigMap{}
	if err := cc.Client.Get(cc.Ctx, key, configmap); apierrors.IsNotFound(err) {
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
	if err := cc.ensureHAProxyReloaderResources(data, owner); err != nil {
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
		if getSSLConfig(cr.Spec.HAProxy.Tls) == "" && !equality.Semantic.DeepEqual(configmap.Data, data) {
			for _, volume := range deployment.Spec.Template.Spec.Volumes {
				if volume.Name == "ssl-certificate" {
					if step := cc.reconcileHAProxyReload(configmap, data); step.Completed() {
						return step
					}
					break
				}
			}
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
	target := desired["haproxy.cfg"]
	hash := haproxyreload.Hash([]byte(target))
	fail := func(err error) result.ReconcileResult {
		cc.ReqLogger.Error(err, "HAProxy hot reload failed", "configurationHash", hash)
		return result.Error(errors.Join(err, cc.setHAProxyReloadStatus("Failed", hash, err.Error())))
	}
	if err := cc.checkHAProxyGeneration(cc.Ctx); err != nil {
		return fail(err)
	}
	key := types.NamespacedName{Name: haproxyCandidateConfigMap, Namespace: cc.MarklogicCluster.Namespace}
	candidate := &corev1.ConfigMap{}
	if err := cc.Client.Get(cc.Ctx, key, candidate); err != nil {
		return fail(fmt.Errorf("reading HAProxy reload candidate: %w", err))
	}
	statusMap := &corev1.ConfigMap{}
	if err := cc.Client.Get(cc.Ctx, types.NamespacedName{Name: haproxyReloadStatusMap, Namespace: cc.MarklogicCluster.Namespace}, statusMap); err != nil {
		return fail(fmt.Errorf("reading HAProxy reload acknowledgements: %w", err))
	}

	pods := &corev1.PodList{}
	if err := cc.haproxyReader().List(cc.Ctx, pods, client.InNamespace(cc.MarklogicCluster.Namespace),
		client.MatchingLabels(getHAProxySelectorLabels(cc.MarklogicCluster.Name))); err != nil {
		return fail(err)
	}
	ready := len(pods.Items) == int(cc.MarklogicCluster.Spec.HAProxy.ReplicaCount) && len(pods.Items) > 0
	var startupValidationErr error
	for i := range pods.Items {
		pod := &pods.Items[i]
		isReady := false
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodReady {
				isReady = condition.Status == corev1.ConditionTrue
			}
		}
		isReady = isReady && pod.DeletionTimestamp == nil
		if !isReady && configmap.Annotations[haproxyBootstrapAnnotation] == "pending" {
			for _, status := range pod.Status.ContainerStatuses {
				if status.Name != "haproxy" {
					continue
				}
				terminated := status.State.Terminated
				if terminated == nil {
					terminated = status.LastTerminationState.Terminated
				}
				if terminated != nil && terminated.ExitCode != 0 {
					startupValidationErr = fmt.Errorf("HAProxy pod %s failed to start (exit %d): %s", pod.Name, terminated.ExitCode, terminated.Message)
				}
			}
		}
		ready = ready && isReady
	}
	if !ready {
		if startupValidationErr != nil && !equality.Semantic.DeepEqual(configmap.Data, desired) {
			next := configmap.DeepCopy()
			next.Data = desired
			if err := cc.Client.Update(cc.Ctx, next); err != nil {
				return fail(fmt.Errorf("correcting invalid bootstrap HAProxy configuration: %w", err))
			}
			if err := cc.setHAProxyReloadStatus("Pending", hash, "Waiting for HAProxy to start with the corrected bootstrap configuration"); err != nil {
				return result.Error(err)
			}
			return result.RequeueSoon(5)
		}
		if startupValidationErr != nil {
			return fail(startupValidationErr)
		}
		if err := cc.setHAProxyReloadStatus("Pending", hash, "Waiting for all HAProxy replicas to be ready"); err != nil {
			return result.Error(err)
		}
		return result.RequeueSoon(5)
	}

	phase := candidate.Data["phase"]
	transaction := candidate.Data["transaction"]
	candidateHash := candidate.Data["hash"]
	currentGeneration := fmt.Sprint(cc.MarklogicCluster.Generation)
	if transaction != "" && candidate.Data["generation"] != "" && candidate.Data["generation"] != currentGeneration {
		if phase == "commit" || phase == "rollback" {
			if phase == "commit" {
				next := candidate.DeepCopy()
				next.Data["phase"] = "rollback"
				if err := cc.Client.Update(cc.Ctx, next); err != nil {
					return fail(fmt.Errorf("rolling back superseded HAProxy transaction: %w", err))
				}
			}
			if err := cc.setHAProxyReloadStatus("Pending", hash, "Rolling back a superseded HAProxy configuration before applying the latest generation"); err != nil {
				return result.Error(err)
			}
			return result.RequeueSoon(2)
		}
	}
	if phase == "" || phase == "idle" || transaction == "" ||
		(candidateHash != hash && phase != "rollback") {
		next := candidate.DeepCopy()
		if equality.Semantic.DeepEqual(configmap.Data, desired) {
			if configmap.Annotations[haproxyBootstrapAnnotation] != "" {
				committed := configmap.DeepCopy()
				delete(committed.Annotations, haproxyBootstrapAnnotation)
				if err := cc.Client.Update(cc.Ctx, committed); err != nil {
					return fail(fmt.Errorf("clearing HAProxy bootstrap marker: %w", err))
				}
			}
			if err := cc.setHAProxyReloadStatus("Success", hash, "Configuration active on all HAProxy replicas"); err != nil {
				return result.Error(err)
			}
			return result.Continue()
		}
		attempt, err := strconv.Atoi(candidate.Data["attempt"])
		if err != nil {
			return fail(fmt.Errorf("invalid HAProxy transaction attempt: %w", err))
		}
		attempt++
		next.Data = map[string]string{
			"phase": "prepare", "transaction": fmt.Sprintf("%s-%s-%d", currentGeneration, hash[:12], attempt),
			"generation": currentGeneration, "attempt": strconv.Itoa(attempt), "hash": hash, "haproxy.cfg": target,
			"previous.cfg": configmap.Data["haproxy.cfg"],
		}
		if err := cc.Client.Update(cc.Ctx, next); err != nil {
			return fail(fmt.Errorf("publishing HAProxy candidate: %w", err))
		}
		if err := cc.setHAProxyReloadStatus("Pending", hash, "Validating candidate on all HAProxy replicas"); err != nil {
			return result.Error(err)
		}
		return result.RequeueSoon(2)
	}

	reports := make(map[string]haproxyreload.PodStatus, len(pods.Items))
	for _, pod := range pods.Items {
		var report haproxyreload.PodStatus
		if raw := statusMap.Data[pod.Name]; raw != "" {
			if err := json.Unmarshal([]byte(raw), &report); err != nil {
				return fail(fmt.Errorf("invalid HAProxy reload acknowledgement from pod %s: %w", pod.Name, err))
			}
		}
		expectedHash := candidateHash
		if phase == "rollback" {
			expectedHash = haproxyreload.Hash([]byte(candidate.Data["previous.cfg"]))
		}
		if report.Transaction == transaction && (report.Hash == candidateHash || report.Hash == expectedHash) {
			reports[pod.Name] = report
		}
	}
	allAre := func(want string) bool {
		for _, pod := range pods.Items {
			if reports[pod.Name].Phase != want {
				return false
			}
		}
		return true
	}
	anyAre := func(phases ...string) bool {
		for _, report := range reports {
			for _, want := range phases {
				if report.Phase == want {
					return true
				}
			}
		}
		return false
	}
	setCandidateIdle := func(committedConfig string) error {
		next := candidate.DeepCopy()
		next.Data = map[string]string{
			"phase": "idle", "transaction": transaction, "generation": currentGeneration,
			"attempt": candidate.Data["attempt"], "hash": haproxyreload.Hash([]byte(committedConfig)),
			"haproxy.cfg": committedConfig, "previous.cfg": committedConfig,
		}
		return cc.Client.Update(cc.Ctx, next)
	}

	switch phase {
	case "prepare":
		if anyAre("failed") {
			message := "HAProxy candidate validation failed"
			for pod, report := range reports {
				if report.Phase == "failed" {
					message = fmt.Sprintf("HAProxy candidate validation failed on pod %s: %s", pod, report.Message)
					break
				}
			}
			if err := setCandidateIdle(configmap.Data["haproxy.cfg"]); err != nil {
				return fail(fmt.Errorf("aborting invalid HAProxy candidate: %w", err))
			}
			return fail(errors.New(message))
		}
		if !allAre("prepared") {
			if err := cc.setHAProxyReloadStatus("Pending", hash, "Waiting for HAProxy candidate validation on all replicas"); err != nil {
				return result.Error(err)
			}
			return result.RequeueSoon(2)
		}
		if err := cc.checkHAProxyGeneration(cc.Ctx); err != nil {
			return fail(err)
		}
		next := candidate.DeepCopy()
		next.Data["phase"] = "commit"
		if err := cc.Client.Update(cc.Ctx, next); err != nil {
			return fail(fmt.Errorf("committing HAProxy candidate: %w", err))
		}
		if err := cc.setHAProxyReloadStatus("Pending", hash, "Applying validated configuration on all HAProxy replicas"); err != nil {
			return result.Error(err)
		}
		return result.RequeueSoon(2)
	case "commit":
		if anyAre("failed") {
			next := candidate.DeepCopy()
			next.Data["phase"] = "rollback"
			if err := cc.Client.Update(cc.Ctx, next); err != nil {
				return fail(fmt.Errorf("starting HAProxy rollback: %w", err))
			}
			message := "HAProxy activation failed"
			for pod, report := range reports {
				if report.Phase == "failed" {
					message = fmt.Sprintf("HAProxy activation failed on pod %s: %s", pod, report.Message)
					break
				}
			}
			if err := cc.setHAProxyReloadStatus("Pending", hash, message+"; rolling back replicas"); err != nil {
				return result.Error(err)
			}
			return result.RequeueSoon(2)
		}
		if !allAre("applied") {
			return result.RequeueSoon(2)
		}
		if err := cc.checkHAProxyGeneration(cc.Ctx); err != nil {
			next := candidate.DeepCopy()
			next.Data["phase"] = "rollback"
			if updateErr := cc.Client.Update(cc.Ctx, next); updateErr != nil {
				return fail(errors.Join(err, updateErr))
			}
			return result.RequeueSoon(2)
		}
		nextConfig := configmap.DeepCopy()
		nextConfig.Data = desired
		delete(nextConfig.Annotations, haproxyBootstrapAnnotation)
		if err := cc.Client.Update(cc.Ctx, nextConfig); err != nil {
			next := candidate.DeepCopy()
			next.Data["phase"] = "rollback"
			if updateErr := cc.Client.Update(cc.Ctx, next); updateErr != nil {
				return fail(errors.Join(fmt.Errorf("committing HAProxy ConfigMap: %w", err), updateErr))
			}
			message := fmt.Sprintf("committing HAProxy ConfigMap failed; rolling back replicas: %v", err)
			if statusErr := cc.setHAProxyReloadStatus("Pending", hash, message); statusErr != nil {
				return result.Error(errors.Join(err, statusErr))
			}
			return result.RequeueSoon(2)
		}
		if err := setCandidateIdle(target); err != nil {
			message := fmt.Sprintf("configuration is active; finalizing HAProxy transaction: %v", err)
			if statusErr := cc.setHAProxyReloadStatus("Pending", hash, message); statusErr != nil {
				return result.Error(errors.Join(err, statusErr))
			}
			return result.RequeueSoon(2)
		}
		if err := cc.setHAProxyReloadStatus("Success", hash, "Configuration active on all HAProxy replicas"); err != nil {
			return result.Error(err)
		}
		return result.Continue()
	case "rollback":
		if anyAre("rollback-failed") {
			return result.RequeueSoon(2)
		}
		if !allAre("rolledback") {
			return result.RequeueSoon(2)
		}
		if err := setCandidateIdle(configmap.Data["haproxy.cfg"]); err != nil {
			return fail(fmt.Errorf("resetting rolled-back HAProxy candidate: %w", err))
		}
		if candidate.Data["generation"] == currentGeneration {
			return fail(errors.New("HAProxy activation failed; all replicas were restored to the previous configuration"))
		}
		return result.RequeueSoon(1)
	case "abort":
		if err := setCandidateIdle(configmap.Data["haproxy.cfg"]); err != nil {
			return fail(fmt.Errorf("resetting aborted HAProxy candidate: %w", err))
		}
		return result.Continue()
	default:
		return fail(fmt.Errorf("unknown HAProxy reload phase %q", phase))
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
