package k8sutil

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/example/marklogic-operator/pkg/result"
)

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
			TargetPort: intstr.FromInt(int(7997)),
			Protocol:   corev1.ProtocolTCP,
		},
		{
			Name:       "app-services",
			Port:       8000,
			TargetPort: intstr.FromInt(int(7997)),
			Protocol:   corev1.ProtocolTCP,
		},
		{
			Name:       "admin",
			Port:       8001,
			TargetPort: intstr.FromInt(int(7997)),
			Protocol:   corev1.ProtocolTCP,
		},
		{
			Name:       "manage",
			Port:       8002,
			TargetPort: intstr.FromInt(int(7997)),
			Protocol:   corev1.ProtocolTCP,
		},
	}
}

func (oc *OperatorContext) getService(namespace string, serviceName string) (*corev1.Service, error) {
	logger := oc.ReqLogger
	getOpts := metav1.GetOptions{
		TypeMeta: generateTypeMeta("Service", "v1"),
	}
	serviceInfo, err := generateK8sClient().CoreV1().Services(namespace).Get(context.TODO(), serviceName, getOpts)
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

func (oc *OperatorContext) ReconcileSrvices() result.ReconcileResult {
	logger := oc.ReqLogger
	client := oc.Client
	cr := oc.MarklogicGroup

	logger.Info("service::CheckHeadlessServer")
	labels := getMarkLogicLabels(cr.Spec.Name)
	annotations := map[string]string{}
	objectMeta := generateObjectMeta(cr.Spec.Name, cr.Namespace, labels, annotations)
	namespace := objectMeta.Namespace
	nsName := types.NamespacedName{Name: objectMeta.Name, Namespace: objectMeta.Namespace}
	service := &corev1.Service{}
	err := client.Get(oc.Ctx, nsName, service)
	if err != nil {
		if errors.IsNotFound(err) {
			logger.Info("MarkLogic service is not found, creating a new one")
			serviceDef := generateHeadlessServiceDef(objectMeta, marklogicServerAsOwner(cr))
			err = oc.createService(namespace, serviceDef)
			if err != nil {
				logger.Info("MarkLogic service creation is failed")
				return result.Error(err)
			}
			logger.Info("MarkLogic service creation is successful")
			result.Continue()
		} else {
			logger.Error(err, "MarkLogic headless service creation is failed")
			return result.Error(err)
		}
	}

	return result.Continue()
}

func (oc *OperatorContext) createService(namespace string, service *corev1.Service) error {
	logger := oc.ReqLogger
	_, err := generateK8sClient().CoreV1().Services(namespace).Create(context.TODO(), service, metav1.CreateOptions{})
	if err != nil {
		logger.Error(err, "MarkLogic service creation is failed")
		return err
	}
	logger.Info("MarkLogic service creation is successful")
	return nil
}
