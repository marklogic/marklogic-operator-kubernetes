package k8sutil

import (
	"embed"
	"github.com/marklogic/marklogic-kubernetes-operator/pkg/result"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

//go:embed scripts/*
var scriptsFolder embed.FS

func (oc *OperatorContext) ReconcileConfigMap() result.ReconcileResult {
	logger := oc.ReqLogger
	client := oc.Client
	cr := oc.MarklogicGroup

	logger.Info("Reconciling MarkLogic ConfigMap")
	labels := getMarkLogicLabels(cr.Spec.Name)
	annotations := map[string]string{}
	configMapName := cr.Spec.Name + "-scripts"
	objectMeta := generateObjectMeta(configMapName, cr.Namespace, labels, annotations)
	nsName := types.NamespacedName{Name: objectMeta.Name, Namespace: objectMeta.Namespace}
	configmap := &corev1.ConfigMap{}
	err := client.Get(oc.Ctx, nsName, configmap)
	if err != nil {
		if errors.IsNotFound(err) {
			logger.Info("MarkLogic sripts ConfigMap is not found, creating a new one")
			configmapDef := oc.generateConfigMapDef(objectMeta, marklogicServerAsOwner(cr))
			err = oc.createConfigMap(configmapDef)
			if err != nil {
				logger.Info("MarkLogic scripts configmap creation is failed")
				return result.Error(err)
			}
			logger.Info("MarkLogic scripts configmap creation is successful")
			// result.Continue()
		} else {
			logger.Error(err, "MarkLogic scripts configmap creation is failed")
			return result.Error(err)
		}
	}

	return result.Continue()
}

func (oc *OperatorContext) generateConfigMapDef(configMapMeta metav1.ObjectMeta, ownerRef metav1.OwnerReference) *corev1.ConfigMap {

	configMapData := oc.getScriptsForConfigMap()
	configmap := &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{
			Kind:       "ConfigMap",
			APIVersion: "v1",
		},
		ObjectMeta: configMapMeta,
		Data:       configMapData,
	}
	configmap.SetOwnerReferences(append(configmap.GetOwnerReferences(), ownerRef))
	return configmap
}

func (oc *OperatorContext) createConfigMap(configMap *corev1.ConfigMap) error {
	logger := oc.ReqLogger
	client := oc.Client
	err := client.Create(oc.Ctx, configMap)
	if err != nil {
		logger.Error(err, "MarkLogic script configmap creation is failed")
		return err
	}
	logger.Info("MarkLogic script configmap creation is successful")
	return nil
}

func (oc *OperatorContext) getScriptsForConfigMap() map[string]string {
	configMapData := make(map[string]string)
	logger := oc.ReqLogger
	files, err := scriptsFolder.ReadDir("scripts")
	if err != nil {
		logger.Error(err, "Error reading scripts directory")
	}
	for _, file := range files {
		logger.Info(file.Name())
		fileName := file.Name()
		fileData, err := scriptsFolder.ReadFile("scripts/" + fileName)
		if err != nil {
			logger.Error(err, "Error reading file")
		}
		logger.Info(string(fileData))
		configMapData[fileName] = string(fileData)
	}
	return configMapData
}

