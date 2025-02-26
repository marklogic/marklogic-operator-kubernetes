package k8sutil

import (
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"

	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	"github.com/marklogic/marklogic-operator-kubernetes/pkg/result"
)

type serviceParameters struct {
	Ports       []corev1.ServicePort
	Type        corev1.ServiceType
	Annotations map[string]string
}

func generateServiceParams(cr *marklogicv1.MarklogicGroup) serviceParameters {
	return serviceParameters{
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
			TargetPort: intstr.FromInt(int(7997)),
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
		Selector: serviceMeta.GetLabels(),
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

func (oc *OperatorContext) getService(namespace string, serviceName string) (*corev1.Service, error) {
	logger := oc.ReqLogger

	var serviceInfo *corev1.Service
	err := oc.Client.Get(oc.Ctx, types.NamespacedName{Name: serviceName, Namespace: namespace}, serviceInfo)
	if err != nil {
		logger.Info("MarkLogic service get action is failed")
		return nil, err
	}
	logger.Info("MarkLogic service get action is successful")
	return serviceInfo, nil
}

func (oc *OperatorContext) CreateOrUpdateService(namespace string, serviceMeta metav1.ObjectMeta, ownerDef metav1.OwnerReference) error {
	logger := oc.ReqLogger
	serviceDef := generateServiceDef(serviceMeta, ownerDef, serviceParameters{})
	_, err := oc.getService(namespace, serviceMeta.Name)
	if err != nil {
		if errors.IsNotFound(err) {
			logger.Info("MarkLogic service is not found, creating a new one")
			err = oc.createService(namespace, serviceDef)
			if err != nil {
				logger.Info("MarkLogic service creation is failed")
				return err
			}
			logger.Info("MarkLogic service creation is successful")
			return nil
		}
		return err
	}
	return nil
}

func generateService(svcName string, cr *marklogicv1.MarklogicGroup) *corev1.Service {
	labels := getCommonLabels(cr.Spec.Name)
	var svcParams serviceParameters = serviceParameters{}
	svcObjectMeta := generateObjectMeta(svcName, cr.Namespace, labels, svcParams.Annotations)
	svcParams = generateServiceParams(cr)
	service := generateServiceDef(svcObjectMeta, marklogicServerAsOwner(cr), svcParams)
	return service
}

func (oc *OperatorContext) ReconcileServices() result.ReconcileResult {
	logger := oc.ReqLogger
	logger.Info("service::Reconciling MarkLogic Service")
	client := oc.Client
	cr := oc.MarklogicGroup
	svc := &corev1.Service{}
	headlessSvcName := cr.Spec.Name
	svcName := cr.Spec.Name + "-cluster"
	services := []string{headlessSvcName, svcName}
	for _, service := range services {
		svcNsName := types.NamespacedName{Name: service, Namespace: cr.Namespace}
		err := client.Get(oc.Ctx, svcNsName, svc)
		if err != nil {
			if errors.IsNotFound(err) {
				logger.Info("MarkLogic service not found, creating a new one")
				svc = generateService(service, cr)
				err = client.Create(oc.Ctx, svc)
				if err != nil {
					logger.Info("MarkLogic service creation has failed")
					return result.Error(err)
				}
				logger.Info("MarkLogic service creation is successful")
			} else {
				logger.Error(err, "MarkLogic service creation has failed")
				return result.Error(err)
			}
		}
	}
	return result.Continue()
}

func (oc *OperatorContext) createService(namespace string, service *corev1.Service) error {
	logger := oc.ReqLogger
	client := oc.Client
	err := client.Create(oc.Ctx, service)
	if err != nil {
		logger.Error(err, "MarkLogic service creation has failed")
		return err
	}
	logger.Info("MarkLogic service creation is successful")
	return nil
}
