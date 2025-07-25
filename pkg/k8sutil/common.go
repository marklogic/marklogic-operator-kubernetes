package k8sutil

import (
	"math/rand"
	"time"

	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// generateTypeMeta generates the TyeMeta
func generateTypeMeta(resourceKind string, apiVersion string) metav1.TypeMeta {
	return metav1.TypeMeta{
		Kind:       resourceKind,
		APIVersion: apiVersion,
	}
}

// generateObjectMeta generates the ObjectMeta
func generateObjectMeta(name string, namespace string, labels map[string]string, annotations map[string]string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:        name,
		Namespace:   namespace,
		Labels:      labels,
		Annotations: annotations,
	}
}

func AddOwnerRefToObject(obj metav1.Object, ownerRef metav1.OwnerReference) {
	obj.SetOwnerReferences(append(obj.GetOwnerReferences(), ownerRef))
}

func marklogicServerAsOwner(cr *marklogicv1.MarklogicGroup) metav1.OwnerReference {
	trueVar := true
	return metav1.OwnerReference{
		APIVersion: cr.APIVersion,
		Kind:       cr.Kind,
		Name:       cr.Name,
		UID:        cr.UID,
		Controller: &trueVar,
	}
}

func LabelSelectors(labels map[string]string) *metav1.LabelSelector {
	return &metav1.LabelSelector{MatchLabels: labels}
}

func getSelectorLabels(name string) map[string]string {
	selectorLabels := map[string]string{
		"app.kubernetes.io/name":       "marklogic",
		"app.kubernetes.io/instance":   name,
		"app.kubernetes.io/managed-by": "marklogic-operator",
		"app.kubernetes.io/component":  "database",
	}
	return selectorLabels
}

func getHAProxySelectorLabels(name string) map[string]string {
	selectorLabels := map[string]string{
		"app.kubernetes.io/name":       "marklogic",
		"app.kubernetes.io/instance":   name,
		"app.kubernetes.io/managed-by": "marklogic-operator",
		"app.kubernetes.io/component":  "haproxy",
	}
	return selectorLabels
}

func getFluentBitLabels(name string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":     "fluent-bit",
		"app.kubernetes.io/instance": name,
	}
}

func marklogicClusterAsOwner(cr *marklogicv1.MarklogicCluster) metav1.OwnerReference {
	trueVar := true
	return metav1.OwnerReference{
		APIVersion: cr.APIVersion,
		Kind:       cr.Kind,
		Name:       cr.Name,
		UID:        cr.UID,
		Controller: &trueVar,
	}
}

func setOperatorInternalStatus(oc *OperatorContext, newState marklogicv1.InternalState) error {
	oc.ReqLogger.Info("common::setOperatorProgressStatus")
	currentState := oc.MarklogicGroup.Status.MarklogicGroupStatus

	if currentState == newState {
		// no need to change.
		return nil
	}

	patch := client.MergeFrom(oc.MarklogicGroup.DeepCopy())
	oc.MarklogicGroup.Status.MarklogicGroupStatus = newState

	if err := oc.Client.Status().Patch(oc.Ctx, oc.MarklogicGroup, patch); err != nil {
		oc.ReqLogger.Error(err, "error updating the MarkLogic Operator Internal status")
		return err
	}

	return nil
}

func generateRandomAlphaNumeric(length int) string {
	const charset = "abcdefghijklmnopqrstuvwxyz" +
		"ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

	seededRand := rand.New(rand.NewSource(time.Now().UnixNano()))
	result := make([]byte, length)
	for i := range result {
		result[i] = charset[seededRand.Intn(len(charset))]
	}
	return string(result)
}
