package k8sutil

import (
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/cisco-open/k8s-objectmatcher/patch"
	databasev1alpha1 "github.com/marklogic/marklogic-kubernetes-operator/api/v1alpha1"
	"github.com/marklogic/marklogic-kubernetes-operator/pkg/result"
)

func generateIngressDef(ingressMeta metav1.ObjectMeta, ownerRef metav1.OwnerReference, cr *databasev1alpha1.MarklogicCluster) *networkingv1.Ingress {
	pathType := networkingv1.PathTypePrefix
	var ingressRules []networkingv1.IngressRule
	for _, appServer := range cr.Spec.HAProxy.AppServers {
		ingressRules = append(ingressRules, networkingv1.IngressRule{
			Host: cr.Spec.HAProxy.Ingress.Host,
			IngressRuleValue: networkingv1.IngressRuleValue{
				HTTP: &networkingv1.HTTPIngressRuleValue{
					Paths: []networkingv1.HTTPIngressPath{
						{Path: appServer.Path,
							PathType: &pathType,
							Backend: networkingv1.IngressBackend{
								Service: &networkingv1.IngressServiceBackend{
									Name: "marklogic-haproxy",
									Port: networkingv1.ServiceBackendPort{
										Number: cr.Spec.HAProxy.FrontendPort,
									},
								},
							},
						},
					},
				},
			},
		})
	}
	for _, additionalHost := range cr.Spec.HAProxy.Ingress.AdditionalHosts {
		ingressRules = append(ingressRules, additionalHost)
	}

	ingressSpec := networkingv1.IngressSpec{
		IngressClassName: &cr.Spec.HAProxy.Ingress.IngressClassName,
		Rules:            ingressRules,
	}
	if cr.Spec.HAProxy.Ingress.TLS != nil {
		ingressSpec.TLS = cr.Spec.HAProxy.Ingress.TLS
	}
	ingressDef := &networkingv1.Ingress{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Ingress",
			APIVersion: "v1",
		},
		ObjectMeta: ingressMeta,
		Spec:       ingressSpec,
	}
	ingressDef.SetOwnerReferences(append(ingressDef.GetOwnerReferences(), ownerRef))
	return ingressDef
}

func (cc *ClusterContext) getIngress(namespace string, ingressName string) (*networkingv1.Ingress, error) {
	logger := cc.ReqLogger

	var ingress = &networkingv1.Ingress{}
	err := cc.Client.Get(cc.Ctx, types.NamespacedName{Name: ingressName, Namespace: namespace}, ingress)
	if err != nil {
		logger.Info("MarkLogic Ingress get action failed")
		return nil, err
	}
	logger.Info("MarkLogic Ingress get action is successful")
	return ingress, nil
}

func generateIngress(ingressName string, cr *databasev1alpha1.MarklogicCluster) *networkingv1.Ingress {
	labels := getMarkLogicLabels(cr.GetObjectMeta().GetName())
	annotations := cr.Spec.HAProxy.Ingress.Annotations
	ingressObjectMeta := generateObjectMeta(ingressName, cr.Namespace, labels, annotations)
	ingress := generateIngressDef(ingressObjectMeta, marklogicClusterAsOwner(cr), cr)
	return ingress
}

func (cc *ClusterContext) ReconcileIngress() result.ReconcileResult {
	logger := cc.ReqLogger
	logger.Info("Ingress::Reconciling MarkLogic Ingress")
	client := cc.Client
	cr := cc.MarklogicCluster
	ingressName := cr.ObjectMeta.Name
	currentIngress, err := cc.getIngress(cr.Namespace, ingressName)
	ingressDef := generateIngress(ingressName, cr)
	if err != nil {
		if errors.IsNotFound(err) {
			logger.Info("MarkLogic Ingress not found, creating a new one")
			err = client.Create(cc.Ctx, ingressDef)
			if err != nil {
				logger.Info("MarkLogic Ingress creation has failed")
				return result.Error(err)
			}
			logger.Info("MarkLogic Ingress creation is successful")
			cc.Recorder.Event(cc.MarklogicCluster, "Normal", "IngressCreated", "MarkLogic Ingress creation is successful")
		} else {
			logger.Error(err, "MarkLogic Ingress creation has failed")
			return result.Error(err)
		}
	} else {
		logger.Info("MarkLogic Ingress already exists")
		patchDiff, err := patch.DefaultPatchMaker.Calculate(currentIngress, ingressDef,
			patch.IgnoreStatusFields(),
			patch.IgnoreVolumeClaimTemplateTypeMetaAndStatus(),
			patch.IgnoreField("kind"))
		if err != nil {
			logger.Error(err, "Error calculating patch")
			return result.Error(err)
		}
		if !patchDiff.IsEmpty() {
			logger.Info("MarkLogic Ingress spec is different from the input Ingress spec, updating the Ingress")
			logger.Info(patchDiff.String())
			err := cc.Client.Update(cc.Ctx, ingressDef)
			if err != nil {
				logger.Error(err, "Error updating Ingress")
				return result.Error(err)
			}
		} else {
			logger.Info("MarkLogic Ingress spec is the same as the input Ingress spec")

		}
		logger.Info("MarkLogic Ingress is updated")
	}
	return result.Continue()
}

func (cc *ClusterContext) createIngress(namespace string) error {
	logger := cc.ReqLogger
	client := cc.Client
	cr := cc.MarklogicCluster
	ingressName := cr.ObjectMeta.Name + "-ingress"
	ingress := generateIngress(ingressName, cr)
	err := client.Create(cc.Ctx, ingress)
	if err != nil {
		logger.Error(err, "MarkLogic ingress creation has failed")
		return err
	}
	logger.Info("MarkLogic service ingress is successful")
	return nil
}
