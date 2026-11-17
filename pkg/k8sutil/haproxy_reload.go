// Copyright (c) 2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package k8sutil

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/cisco-open/k8s-objectmatcher/patch"
	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	"github.com/marklogic/marklogic-operator-kubernetes/pkg/haproxyreload"
	"github.com/marklogic/marklogic-operator-kubernetes/pkg/result"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type HAProxyExecutor func(context.Context, *corev1.Pod, string, []byte) ([]byte, error)

const haproxyBootstrapAnnotation = "marklogic.progress.com/haproxy-bootstrap"

func NewHAProxyExecutor(config *rest.Config) (HAProxyExecutor, error) {
	kube, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context, pod *corev1.Pod, operation string, data []byte) ([]byte, error) {
		// The helper may spend a second timeout recovering a failed activation.
		ctx, cancel := context.WithTimeout(ctx, 2*haproxyreload.Timeout+5*time.Second)
		defer cancel()
		request := kube.CoreV1().RESTClient().Post().Resource("pods").Name(pod.Name).
			Namespace(pod.Namespace).SubResource("exec").
			VersionedParams(&corev1.PodExecOptions{
				Container: "haproxy", Command: []string{haproxyreload.Executable, operation},
				Stdin: data != nil, Stdout: true, Stderr: true,
			}, scheme.ParameterCodec)
		executor, err := remotecommand.NewSPDYExecutor(config, "POST", request.URL())
		if err != nil {
			return nil, err
		}
		var stdout, stderr bytes.Buffer
		stream := remotecommand.StreamOptions{Stdout: &stdout, Stderr: &stderr}
		if data != nil {
			stream.Stdin = bytes.NewReader(data)
		}
		if err := executor.StreamWithContext(ctx, stream); err != nil {
			return nil, fmt.Errorf("%s in HAProxy pod %s: %w: %s", operation, pod.Name, err, stderr.String())
		}
		return stdout.Bytes(), nil
	}, nil
}

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
	template.Spec.InitContainers = []corev1.Container{{
		Name: "install-haproxy-reloader", Image: image,
		Command:      []string{"/haproxy-reloader", "install"},
		VolumeMounts: []corev1.VolumeMount{runtimeMount},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: boolPtr(false), ReadOnlyRootFilesystem: boolPtr(true),
			Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
	}}
	template.Spec.Volumes = append(template.Spec.Volumes, corev1.Volume{
		Name: "haproxy-runtime", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
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

// reconcileHAProxyReload keeps the ConfigMap as the last all-replica commit.
// Until activation is confirmed on every replica, replacement pods start from it.
func (cc *ClusterContext) reconcileHAProxyReload(configmap *corev1.ConfigMap, desired map[string]string) result.ReconcileResult {
	target := []byte(desired["haproxy.cfg"])
	hash := haproxyreload.Hash(target)
	fail := func(err error) result.ReconcileResult {
		cc.ReqLogger.Error(err, "HAProxy hot reload failed", "configurationHash", hash)
		return result.Error(errors.Join(err, cc.setHAProxyReloadStatus("Failed", hash, err.Error())))
	}
	if cc.HAProxyExec == nil {
		return fail(errors.New("HAProxy exec client is not configured"))
	}
	pods := &corev1.PodList{}
	if err := cc.haproxyReader().List(cc.Ctx, pods, client.InNamespace(cc.MarklogicCluster.Namespace),
		client.MatchingLabels(getHAProxySelectorLabels(cc.MarklogicCluster.Name))); err != nil {
		return fail(err)
	}
	ready := len(pods.Items) == int(cc.MarklogicCluster.Spec.HAProxy.ReplicaCount) && len(pods.Items) > 0
	bootstrapFailed := configmap.Annotations[haproxyBootstrapAnnotation] == "pending" && ready
	var startupErr error
	for i := range pods.Items {
		pod := &pods.Items[i]
		conditionReady := false
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
				conditionReady = true
			}
		}
		validationFailed := false
		if !conditionReady {
			for _, status := range pod.Status.ContainerStatuses {
				if status.Name != "haproxy" {
					continue
				}
				terminated := status.State.Terminated
				if terminated == nil {
					terminated = status.LastTerminationState.Terminated
				}
				if terminated != nil && terminated.ExitCode != 0 {
					startupErr = fmt.Errorf("HAProxy pod %s failed to start (exit %d): %s", pod.Name, terminated.ExitCode, terminated.Message)
					validationFailed = strings.Contains(terminated.Message, "HAProxy validation failed")
				}
			}
		}
		bootstrapFailed = bootstrapFailed && pod.DeletionTimestamp == nil && validationFailed
		ready = ready && pod.DeletionTimestamp == nil && conditionReady
	}
	if bootstrapFailed && !equality.Semantic.DeepEqual(configmap.Data, desired) {
		if err := cc.checkHAProxyGeneration(cc.Ctx); err != nil {
			return fail(err)
		}
		next := configmap.DeepCopy()
		next.Data = desired
		if err := cc.Client.Update(cc.Ctx, next); err != nil {
			return fail(fmt.Errorf("correcting HAProxy bootstrap ConfigMap: %w", err))
		}
		if err := cc.setHAProxyReloadStatus("Pending", hash, "Waiting for HAProxy to start with the corrected bootstrap configuration"); err != nil {
			return result.Error(err)
		}
		return result.RequeueSoon(5)
	}
	if startupErr != nil {
		return fail(startupErr)
	}
	if !ready {
		if err := cc.setHAProxyReloadStatus("Pending", hash, "Waiting for all HAProxy replicas to be ready"); err != nil {
			return result.Error(err)
		}
		return result.RequeueSoon(5)
	}
	sort.Slice(pods.Items, func(i, j int) bool { return pods.Items[i].Name < pods.Items[j].Name })
	previous := []byte(configmap.Data["haproxy.cfg"])
	previousHash := haproxyreload.Hash(previous)
	states := make(map[string]string, len(pods.Items))
	rollback := func(cause error) result.ReconcileResult {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(cc.Ctx), time.Duration(len(pods.Items))*(2*haproxyreload.Timeout+5*time.Second))
		defer cancel()
		errs := []error{cause}
		for i := range pods.Items {
			pod := &pods.Items[i]
			if states[pod.Name] == previousHash {
				continue
			}
			if _, err := cc.HAProxyExec(ctx, pod, "apply", previous); err != nil {
				errs = append(errs, fmt.Errorf("rollback of pod %s failed: %w", pod.Name, err))
			}
		}
		return fail(errors.Join(errs...))
	}
	needsReload := false
	for i := range pods.Items {
		pod := &pods.Items[i]
		output, err := cc.HAProxyExec(cc.Ctx, pod, "status", nil)
		if err != nil {
			return rollback(err)
		}
		var status haproxyreload.Status
		if err := json.Unmarshal(output, &status); err != nil {
			return rollback(fmt.Errorf("decoding HAProxy pod %s status: %w", pod.Name, err))
		}
		if status.Hash == "" || status.WorkerPID <= 0 {
			return rollback(fmt.Errorf("HAProxy pod %s returned incomplete status", pod.Name))
		}
		states[pod.Name] = status.Hash
		needsReload = needsReload || status.Hash != hash
	}
	if needsReload {
		for i := range pods.Items {
			if _, err := cc.HAProxyExec(cc.Ctx, &pods.Items[i], "validate", target); err != nil {
				return rollback(err)
			}
		}
		if err := cc.checkHAProxyGeneration(cc.Ctx); err != nil {
			return rollback(err)
		}
		for i := range pods.Items {
			pod := &pods.Items[i]
			if states[pod.Name] == hash {
				continue
			}
			// A transport failure may happen after activation; roll this pod back too.
			states[pod.Name] = ""
			if _, err := cc.HAProxyExec(cc.Ctx, pod, "apply", target); err != nil {
				return rollback(err)
			}
			states[pod.Name] = hash
		}
	}
	if err := cc.checkHAProxyGeneration(cc.Ctx); err != nil {
		return rollback(err)
	}
	if !bytes.Equal(previous, target) || configmap.Annotations[haproxyBootstrapAnnotation] != "" {
		next := configmap.DeepCopy()
		next.Data = desired
		delete(next.Annotations, haproxyBootstrapAnnotation)
		if err := cc.Client.Update(cc.Ctx, next); err != nil {
			return rollback(fmt.Errorf("committing HAProxy ConfigMap: %w", err))
		}
	}
	if err := cc.setHAProxyReloadStatus("Success", hash, "Configuration active on all HAProxy replicas"); err != nil {
		return result.Error(err)
	}
	return result.Continue()
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
