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
	labels := getCommonLabels(cr.Spec.Name)
	annotations := getCommonAnnotations()
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

// configMap for fluent bit
func (oc *OperatorContext) ReconcileFluentBitConfigMap() result.ReconcileResult {
	logger := oc.ReqLogger
	client := oc.Client
	cr := oc.MarklogicGroup

	logger.Info("Reconciling Fluent Bit ConfigMap")
	labels := getFluentBitLabels(cr.Spec.Name)
	annotations := map[string]string{}
	configMapName := "fluent-bit"
	objectMeta := generateObjectMeta(configMapName, cr.Namespace, labels, annotations)
	nsName := types.NamespacedName{Name: objectMeta.Name, Namespace: objectMeta.Namespace}
	configmap := &corev1.ConfigMap{}
	err := client.Get(oc.Ctx, nsName, configmap)
	if err != nil {
		if errors.IsNotFound(err) {
			logger.Info("Fluent Bit ConfigMap is not found, creating a new one")
			fluentBitDef := oc.generateFluentBitDef(objectMeta, marklogicServerAsOwner(cr))
			err = oc.createConfigMap(fluentBitDef)
			if err != nil {
				logger.Info("Fluent Bit configmap creation is failed")
				return result.Error(err)
			}
			logger.Info("Fluent Bit configmap creation is successful")
			// result.Continue()
		} else {
			logger.Error(err, "Fluent Bit configmap creation is failed")
			return result.Error(err)
		}
	}

	return result.Continue()
}

func (oc *OperatorContext) generateFluentBitDef(configMapMeta metav1.ObjectMeta, ownerRef metav1.OwnerReference) *corev1.ConfigMap {

	fluentBitData := oc.getFluentBitData()
	fluentBitConfigmap := &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{
			Kind:       "ConfigMap",
			APIVersion: "v1",
		},
		ObjectMeta: configMapMeta,
		Data:       fluentBitData,
	}
	fluentBitConfigmap.SetOwnerReferences(append(fluentBitConfigmap.GetOwnerReferences(), ownerRef))
	return fluentBitConfigmap
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

func (cc *ClusterContext) createConfigMapForCC(configMap *corev1.ConfigMap) error {
	logger := cc.ReqLogger
	client := cc.Client
	err := client.Create(cc.Ctx, configMap)
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
		configMapData[fileName] = string(fileData)
	}
	return configMapData
}

func (oc *OperatorContext) getFluentBitData() map[string]string {
	fluentBitData := make(map[string]string)

	fluentBitData["fluent-bit.conf"] = `
[SERVICE]
	Flush 5
	Log_Level info
	Daemon off
	Parsers_File parsers.conf

@INCLUDE inputs.conf
@INCLUDE filters.conf
@INCLUDE outputs.conf
`

	fluentBitData["inputs.conf"] = ""

	if oc.MarklogicGroup.Spec.LogCollection.Files.ErrorLogs {
		errorLog := `
[INPUT]
	Name tail
	Path /var/opt/MarkLogic/Logs/*ErrorLog.txt
	Read_from_head true
	Tag kube.marklogic.logs.error
	Path_Key path
	Parser error_parser
	Mem_Buf_Limit 4MB
`
		fluentBitData["inputs.conf"] += errorLog
	}

	if oc.MarklogicGroup.Spec.LogCollection.Files.AccessLogs {
		accessLog := `
[INPUT]
	Name tail
	Path /var/opt/MarkLogic/Logs/*AccessLog.txt
	Read_from_head true
	tag kube.marklogic.logs.access
	Path_Key path
	Parser access_parser
	Mem_Buf_Limit 4MB
`
		fluentBitData["inputs.conf"] += accessLog
	}

	if oc.MarklogicGroup.Spec.LogCollection.Files.RequestLogs {
		requestLog := `
[INPUT]
	Name tail
	Path /var/opt/MarkLogic/Logs/*RequestLog.txt
	Read_from_head true
	tag kube.marklogic.logs.request
	Path_Key path
	Parser json_parser
	Mem_Buf_Limit 4MB
`
		fluentBitData["inputs.conf"] += requestLog
	}

	if oc.MarklogicGroup.Spec.LogCollection.Files.CrashLogs {
		crashLog := `
[INPUT]
	Name tail
	Path /var/opt/MarkLogic/Logs/CrashLog.txt
	Read_from_head true
	tag kube.marklogic.logs.crash
	Path_Key path
	Mem_Buf_Limit 4MB
`
		fluentBitData["inputs.conf"] += crashLog
	}

	if oc.MarklogicGroup.Spec.LogCollection.Files.AuditLogs {
		auditLog := `
[INPUT]
	Name tail
	Path /var/opt/MarkLogic/Logs/AuditLog.txt
	Read_from_head true
	tag kube.marklogic.logs.audit
	Path_Key path
	Mem_Buf_Limit 4MB
`
		fluentBitData["inputs.conf"] += auditLog
	}

	fluentBitData["outputs.conf"] = oc.MarklogicGroup.Spec.LogCollection.Outputs

	fluentBitData["filters.conf"] = `
[FILTER]
	Name modify
	Match *
	Add pod ${POD_NAME}
	Add namespace ${NAMESPACE}

[FILTER]
	Name modify
	Match kube.marklogic.logs.error
	Add tag kube.marklogic.logs.error

[FILTER]
	Name modify
	Match kube.marklogic.logs.access
	Add tag kube.marklogic.logs.access

[FILTER]
	Name modify
	Match kube.marklogic.logs.request
	Add tag kube.marklogic.logs.request

[FILTER]
	Name modify
	Match kube.marklogic.logs.audit
	Add tag kube.marklogic.logs.audit

[FILTER]
	Name modify
	Match kube.marklogic.logs.crash
	Add tag kube.marklogic.logs.crash
`

	fluentBitData["parsers.conf"] = `
[PARSER]
	Name error_parser
	Format regex
	Regex ^(?<time>(.+?)(?=[a-zA-Z]))(?<log_level>(.+?)(?=:))(.+?)(?=[a-zA-Z])(?<log>.*)
	Time_Key time 
	Time_Format %Y-%m-%d %H:%M:%S.%L

[PARSER]
	Name access_parser
	Format regex
	Regex ^(?<host>[^ ]*)(.+?)(?<=\- )(?<user>(.+?)(?=\[))(.+?)(?<=\[)(?<time>(.+?)(?=\]))(.+?)(?<=")(?<request>[^\ ]+[^\"]+)(.+?)(?=\d)(?<response_code>[^\ ]*)(.+?)(?=\d|-)(?<response_obj_size>[^\ ]*)(.+?)(?=")(?<request_info>.*)
	Time_Key time 
	Time_Format %d/%b/%Y:%H:%M:%S %z

[PARSER]
	Name json_parser
	Format json
	Time_Key time
	Time_Format %Y-%m-%dT%H:%M:%S%z
`

	return fluentBitData
}
