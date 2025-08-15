package k8sutil

import (
	"strings"

	"github.com/cisco-open/k8s-objectmatcher/patch"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"

	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	"github.com/marklogic/marklogic-operator-kubernetes/pkg/result"
)

type serviceParameters struct {
	StsName     string
	Ports       []corev1.ServicePort
	Type        corev1.ServiceType
	Annotations map[string]string
}

func generateServiceParams(cr *marklogicv1.MarklogicGroup) serviceParameters {
	return serviceParameters{
		StsName:     cr.Spec.Name,
		Type:        cr.Spec.Service.Type,
		Ports:       cr.Spec.Service.AdditionalPorts,
		Annotations: cr.Spec.Service.Annotations,
	}
}

func generateServicePorts() []corev1.ServicePort {
	return []corev1.ServicePort{
		{
			Name:       "health-check",
			Port:       7997,
			TargetPort: intstr.FromInt(int(7997)),
			Protocol:   corev1.ProtocolTCP,
		},
		{
			Name:       "xdqp-port1",
			Port:       7998,
			TargetPort: intstr.FromInt(int(7998)),
			Protocol:   corev1.ProtocolTCP,
		},
		{
			Name:       "xdqp-port2",
			Port:       7999,
			TargetPort: intstr.FromInt(int(7999)),
			Protocol:   corev1.ProtocolTCP,
		},
		{
			Name:       "app-services",
			Port:       8000,
			TargetPort: intstr.FromInt(int(8000)),
			Protocol:   corev1.ProtocolTCP,
		},
		{
			Name:       "admin",
			Port:       8001,
			TargetPort: intstr.FromInt(int(8001)),
			Protocol:   corev1.ProtocolTCP,
		},
		{
			Name:       "manage",
			Port:       8002,
			TargetPort: intstr.FromInt(int(8002)),
			Protocol:   corev1.ProtocolTCP,
		},
	}
}

func generateServiceDef(serviceMeta metav1.ObjectMeta, ownerRef metav1.OwnerReference, params serviceParameters) *corev1.Service {
	var svcSpec corev1.ServiceSpec
	svcSpec = corev1.ServiceSpec{
		Selector: getSelectorLabels(params.StsName),
		Ports:    append(params.Ports, generateServicePorts()...),
	}
	if strings.HasSuffix(serviceMeta.Name, "-cluster") {
		svcSpec.Type = params.Type
	} else {
		svcSpec.ClusterIP = "None"
		svcSpec.PublishNotReadyAddresses = true
	}
	service := &corev1.Service{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Service",
			APIVersion: "v1",
		},
		ObjectMeta: serviceMeta,
		Spec:       svcSpec,
	}
	service.SetOwnerReferences(append(service.GetOwnerReferences(), ownerRef))
	return service
}

func (oc *OperatorContext) generateService(svcName string, cr *marklogicv1.MarklogicGroup) *corev1.Service {
	labels := oc.GetOperatorLabels(cr.Spec.Name)
	groupLabels := cr.Spec.Labels
	for key, value := range groupLabels {
		labels[key] = value
	}
	var svcParams serviceParameters = serviceParameters{}
	svcParams = generateServiceParams(cr)
	svcObjectMeta := generateObjectMeta(svcName, cr.Namespace, labels, svcParams.Annotations)
	service := generateServiceDef(svcObjectMeta, marklogicServerAsOwner(cr), svcParams)
	return service
}

func (oc *OperatorContext) ReconcileServices() result.ReconcileResult {
	logger := oc.ReqLogger
	logger.Info("service::Reconciling MarkLogic Service")
	client := oc.Client
	cr := oc.MarklogicGroup
	currentSvc := &corev1.Service{}
	headlessSvcName := cr.Spec.Name
	svcName := cr.Spec.Name + "-cluster"
	services := []string{headlessSvcName, svcName}
	for _, service := range services {
		svcNsName := types.NamespacedName{Name: service, Namespace: cr.Namespace}
		err := client.Get(oc.Ctx, svcNsName, currentSvc)
		svcDef := oc.generateService(service, cr)
		if err != nil {
			if errors.IsNotFound(err) {
				logger.Info("MarkLogic service not found, creating a new one")
				if err := patch.DefaultAnnotator.SetLastAppliedAnnotation(svcDef); err != nil {
					logger.Error(err, "Failed to set last applied annotation for MarkLogic service")
				}
				err = client.Create(oc.Ctx, svcDef)
				if err != nil {
					logger.Info("MarkLogic service creation has failed")
					return result.Error(err)
				}
				logger.Info("MarkLogic service creation is successful")
			} else {
				logger.Error(err, "MarkLogic service creation has failed")
				return result.Error(err)
			}
		} else {
			patchDiff, err := patch.DefaultPatchMaker.Calculate(currentSvc, svcDef,
				patch.IgnoreStatusFields(),
				patch.IgnoreVolumeClaimTemplateTypeMetaAndStatus(),
				patch.IgnoreField("kind"))

			if err != nil {
				logger.Error(err, "Error calculating patch")
				return result.Error(err)
			}
			if !patchDiff.IsEmpty() {
				logger.Info("MarkLogic service spec is different from the MarkLogicGroup spec, updating the service")
				currentSvc.Spec = svcDef.Spec
				currentSvc.ObjectMeta.Annotations = svcDef.ObjectMeta.Annotations
				currentSvc.ObjectMeta.Labels = svcDef.ObjectMeta.Labels
				if err := patch.DefaultAnnotator.SetLastAppliedAnnotation(currentSvc); err != nil {
					logger.Error(err, "Failed to set last applied annotation for MarkLogic service")
				}
				err := oc.Client.Update(oc.Ctx, currentSvc)
				if err != nil {
					logger.Error(err, "Error updating MarkLogic service")
					return result.Error(err)
				}
			} else {
				logger.Info("MarkLogic service spec is the same")
			}
		}
	}
	return result.Continue()
}
