package k8sutil

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"

	databasev1alpha1 "github.com/marklogic/marklogic-kubernetes-operator/api/v1alpha1"
	"github.com/marklogic/marklogic-kubernetes-operator/pkg/result"
)

type serviceParameters struct {
	Ports       []corev1.ServicePort
	Type        corev1.ServiceType
	Annotations map[string]string
}

func generateHeadlessServiceDef(serviceMeta metav1.ObjectMeta, ownerRef metav1.OwnerReference) *corev1.Service {
	service := &corev1.Service{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Service",
			APIVersion: "v1",
		},
		ObjectMeta: serviceMeta,
		Spec: corev1.ServiceSpec{
			Selector:                 serviceMeta.GetLabels(),
			Ports:                    generateServicePorts(),
			ClusterIP:                "None",
			PublishNotReadyAddresses: true,
		},
	}
	service.SetOwnerReferences(append(service.GetOwnerReferences(), ownerRef))
	return service
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

func generateServiceParams(cr *databasev1alpha1.MarklogicGroup) serviceParameters {
	return serviceParameters{
		Type:        cr.Spec.Service.Type,
		Ports:       cr.Spec.Service.AdditionalPorts,
		Annotations: cr.Spec.Service.Annotations,
	}
}

func generateServiceDef(serviceMeta metav1.ObjectMeta, ownerRef metav1.OwnerReference, params serviceParameters) *corev1.Service {
	service := &corev1.Service{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Service",
			APIVersion: "v1",
		},
		ObjectMeta: serviceMeta,
		Spec: corev1.ServiceSpec{
			Selector: serviceMeta.GetLabels(),
			Ports:    append(params.Ports, generateServicePorts()...),
			Type:     params.Type,
		},
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
	serviceDef := generateHeadlessServiceDef(serviceMeta, ownerDef)
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

func (oc *OperatorContext) ReconcileServices() result.ReconcileResult {
	logger := oc.ReqLogger
	logger.Info("service::Reconciling MarkLogic Service")
	client := oc.Client
	cr := oc.MarklogicGroup
	labels := getMarkLogicLabels(cr.Spec.Name)
	svcParams := generateServiceParams(cr)
	svcName := cr.Spec.Name + "-cluster"
	svcObjectMeta := generateObjectMeta(svcName, cr.Namespace, labels, svcParams.Annotations)
	headlessSvcAnnotations := map[string]string{}
	headlessSvcObjectMeta := generateObjectMeta(cr.Name, cr.Namespace, labels, headlessSvcAnnotations)
	namespace := cr.Namespace
	svcNsName := types.NamespacedName{Name: svcObjectMeta.Name, Namespace: svcObjectMeta.Namespace}
	headlessSvcNsName := types.NamespacedName{Name: headlessSvcObjectMeta.Name, Namespace: headlessSvcObjectMeta.Namespace}
	service := &corev1.Service{}
	err := client.Get(oc.Ctx, headlessSvcNsName, service)
	if err != nil {
		logger.Info("MarkLogic headless service not found")
		if errors.IsNotFound(err) {
			logger.Info("MarkLogic headless service not found, creating a new one")
			headlessServiceDef := generateHeadlessServiceDef(headlessSvcObjectMeta, marklogicServerAsOwner(cr))
			err = oc.createService(namespace, headlessServiceDef)
			if err != nil {
				logger.Info("MarkLogic headless service creation is failed")
				return result.Error(err)
			}
			logger.Info("MarkLogic headless service creation is successful")
		} else {
			logger.Error(err, "MarkLogic headless service creation is failed")
			return result.Error(err)
		}
	}
	err = client.Get(oc.Ctx, svcNsName, service)
	if err != nil {
		logger.Info("MarkLogic service is not found")
		if errors.IsNotFound(err) {
			logger.Info("MarkLogic service not found, creating a new one")
			serviceDef := generateServiceDef(svcObjectMeta, marklogicServerAsOwner(cr), svcParams)
			err = oc.createService(namespace, serviceDef)
			if err != nil {
				logger.Info("MarkLogic service creation is failed")
				return result.Error(err)
			}
			logger.Info("MarkLogic service creation is successful")
			// result.Continue()
		} else {
			logger.Error(err, "MarkLogic service creation is failed")
			return result.Error(err)
		}
	}
	return result.Continue()
}

func (oc *OperatorContext) createService(namespace string, service *corev1.Service) error {
	logger := oc.ReqLogger
	client := oc.Client
	err := client.Create(oc.Ctx, service)
	if err != nil {
		logger.Error(err, "MarkLogic service creation is failed")
		return err
	}
	logger.Info("MarkLogic service creation is successful")
	return nil
}
