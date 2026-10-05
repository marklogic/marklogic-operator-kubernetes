package utils

import (
	"context"
	"fmt"
	"time"

	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/e2e-framework/klient"
)

const (
	operatorCredentialUsername   = "marklogic-kubernetes-operator"
	credentialRevisionAnnotation = "marklogic.progress.com/credential-revision"
)

// WaitForOperatorCredentialHandoff verifies the generated operator identity is active
// and that the StatefulSet has replaced its bootstrap-credential pod.
func WaitForOperatorCredentialHandoff(ctx context.Context, client klient.Client, namespace, clusterName, groupName string, timeout time.Duration) error {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		complete, err := checkOperatorCredentialHandoff(waitCtx, client, namespace, clusterName, groupName)
		if err != nil {
			return err
		}
		if complete {
			return nil
		}
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("timed out waiting for operator credential handoff in %s/%s: %w", namespace, groupName, waitCtx.Err())
		case <-ticker.C:
		}
	}
}

func checkOperatorCredentialHandoff(ctx context.Context, client klient.Client, namespace, clusterName, groupName string) (bool, error) {
	resources := client.Resources(namespace)
	group := &marklogicv1.MarklogicGroup{}
	if err := resources.Get(ctx, groupName, namespace, group); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("get MarklogicGroup %s/%s: %w", namespace, groupName, err)
	}

	operatorSecretName := clusterName + "-operator"
	if group.Status.CredentialSecretName != operatorSecretName {
		return false, nil
	}
	operatorSecret := &corev1.Secret{}
	if err := resources.Get(ctx, operatorSecretName, namespace, operatorSecret); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("get operator credential Secret %s/%s: %w", namespace, operatorSecretName, err)
	}
	if string(operatorSecret.Data["username"]) != operatorCredentialUsername || len(operatorSecret.Data["password"]) != 32 {
		return false, fmt.Errorf("operator credential Secret %s/%s has unexpected username or password length", namespace, operatorSecretName)
	}

	adminSecretName := group.Spec.SecretName
	if adminSecretName == "" {
		adminSecretName = clusterName + "-admin"
	}
	adminSecret := &corev1.Secret{}
	if err := resources.Get(ctx, adminSecretName, namespace, adminSecret); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("get retained admin Secret %s/%s: %w", namespace, adminSecretName, err)
	}

	statefulSet := &appsv1.StatefulSet{}
	if err := resources.Get(ctx, groupName, namespace, statefulSet); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("get StatefulSet %s/%s: %w", namespace, groupName, err)
	}
	if statefulSet.Spec.UpdateStrategy.Type != appsv1.OnDeleteStatefulSetStrategyType {
		return false, nil
	}
	revision := statefulSet.Spec.Template.Annotations[credentialRevisionAnnotation]
	if revision == "" {
		return false, nil
	}
	if !credentialSecretVolumesReady(statefulSet.Spec.Template.Spec.Volumes, operatorSecretName, adminSecretName) {
		return false, nil
	}

	pod := &corev1.Pod{}
	if err := resources.Get(ctx, groupName+"-0", namespace, pod); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("get pod %s/%s-0: %w", namespace, groupName, err)
	}
	if pod.Annotations[credentialRevisionAnnotation] != revision || !isPodReady(pod) {
		return false, nil
	}
	if !credentialSecretVolumesReady(pod.Spec.Volumes, operatorSecretName, adminSecretName) {
		return false, nil
	}
	return true, nil
}

func credentialSecretVolumesReady(volumes []corev1.Volume, operatorSecretName, adminSecretName string) bool {
	operatorSecretMounted := false
	for _, volume := range volumes {
		if volume.Secret == nil {
			continue
		}
		if volume.Secret.SecretName == adminSecretName {
			return false
		}
		if volume.Secret.SecretName == operatorSecretName {
			operatorSecretMounted = true
		}
	}
	return operatorSecretMounted
}

func isPodReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}
