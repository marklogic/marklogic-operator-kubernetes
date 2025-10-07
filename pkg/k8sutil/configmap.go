package k8sutil

import (
	"embed"
	"strings"

	"github.com/marklogic/marklogic-operator-kubernetes/pkg/result"
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
	labels := oc.GetOperatorLabels(cr.Spec.Name)
	annotations := oc.GetOperatorAnnotations()
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

	// Main YAML configuration file
	fluentBitData["fluent-bit.yaml"] = `service:
  flush: 5
  log_level: info
  daemon: off
  parsers_file: parsers.yaml

pipeline:
  inputs:`

	// Add INPUT sections based on enabled log types
	if oc.MarklogicGroup.Spec.LogCollection.Files.ErrorLogs {
		fluentBitData["fluent-bit.yaml"] += `
    - name: tail
      path: /var/opt/MarkLogic/Logs/*ErrorLog.txt
      read_from_head: true
      tag: kube.marklogic.logs.error
      path_key: path
      parser: error_parser
      mem_buf_limit: 4MB`
	}

	if oc.MarklogicGroup.Spec.LogCollection.Files.AccessLogs {
		fluentBitData["fluent-bit.yaml"] += `
    - name: tail
      path: /var/opt/MarkLogic/Logs/*AccessLog.txt
      read_from_head: true
      tag: kube.marklogic.logs.access
      path_key: path
      parser: access_parser
      mem_buf_limit: 4MB`
	}

	if oc.MarklogicGroup.Spec.LogCollection.Files.RequestLogs {
		fluentBitData["fluent-bit.yaml"] += `
    - name: tail
      path: /var/opt/MarkLogic/Logs/*RequestLog.txt
      read_from_head: true
      tag: kube.marklogic.logs.request
      path_key: path
      parser: json_parser
      mem_buf_limit: 4MB`
	}

	if oc.MarklogicGroup.Spec.LogCollection.Files.CrashLogs {
		fluentBitData["fluent-bit.yaml"] += `
    - name: tail
      path: /var/opt/MarkLogic/Logs/CrashLog.txt
      read_from_head: true
      tag: kube.marklogic.logs.crash
      path_key: path
      mem_buf_limit: 4MB`
	}

	if oc.MarklogicGroup.Spec.LogCollection.Files.AuditLogs {
		fluentBitData["fluent-bit.yaml"] += `
    - name: tail
      path: /var/opt/MarkLogic/Logs/AuditLog.txt
      read_from_head: true
      tag: kube.marklogic.logs.audit
      path_key: path
      mem_buf_limit: 4MB`
	}

	// Add FILTER sections
	fluentBitData["fluent-bit.yaml"] += `

  filters:
    - name: modify
      match: "*"
      add: pod ${POD_NAME}
      add: namespace ${NAMESPACE}
    - name: modify
      match: kube.marklogic.logs.error
      add: tag kube.marklogic.logs.error
    - name: modify
      match: kube.marklogic.logs.access
      add: tag kube.marklogic.logs.access
    - name: modify
      match: kube.marklogic.logs.request
      add: tag kube.marklogic.logs.request
    - name: modify
      match: kube.marklogic.logs.audit
      add: tag kube.marklogic.logs.audit
    - name: modify
      match: kube.marklogic.logs.crash
      add: tag kube.marklogic.logs.crash

  outputs:`

	// Handle user-defined outputs from LogCollection.Outputs
	if oc.MarklogicGroup.Spec.LogCollection.Outputs != "" {
		// Process user-defined outputs, adjusting indentation to match pipeline level
		outputs := oc.MarklogicGroup.Spec.LogCollection.Outputs
		// Split into lines and process indentation
		lines := strings.Split(outputs, "\n")
		processedLines := make([]string, 0, len(lines))
		for _, line := range lines {
			if strings.TrimSpace(line) == "" {
				continue // Skip empty lines
			}
			// Count leading spaces in original line
			leadingSpaces := len(line) - len(strings.TrimLeft(line, " "))
			content := strings.TrimLeft(line, " ")
			if content != "" {
				// For YAML list items starting with "-", use 4 spaces base indentation
				// For properties under list items, use 6 spaces base indentation
				var newIndent int
				const yamlListItemIndent = 4
				const yamlPropertyIndent = 6
				if strings.HasPrefix(content, "- ") {
					newIndent = yamlListItemIndent // List items at pipeline outputs level
				} else {
					newIndent = yamlPropertyIndent // Properties under list items
				}
				// Add any additional relative indentation beyond the first level
				const baseIndentOffset = 2
				if leadingSpaces > baseIndentOffset {
					newIndent += leadingSpaces - baseIndentOffset
				}
				processedLines = append(processedLines, strings.Repeat(" ", newIndent)+content)
			}
		}
		fluentBitData["fluent-bit.yaml"] += "\n" + strings.Join(processedLines, "\n")
	} else {
		// Default stdout output if none specified
		fluentBitData["fluent-bit.yaml"] += `
    - name: stdout
      match: "*"
      format: json_lines`
	}

	// Parsers in YAML format
	fluentBitData["parsers.yaml"] = `parsers:
  - name: error_parser
    format: regex
    regex: ^(?<time>(.+?)(?=[a-zA-Z]))(?<log_level>(.+?)(?=:))(.+?)(?=[a-zA-Z])(?<log>.*)
    time_key: time
    time_format: "%Y-%m-%d %H:%M:%S.%L"

  - name: access_parser
    format: regex
    regex: ^(?<host>[^ ]*)(.+?)(?<=\- )(?<user>(.+?)(?=\[))(.+?)(?<=\[)(?<time>(.+?)(?=\]))(.+?)(?<=")(?<request>[^\ ]+[^\"]+)(.+?)(?=\d)(?<response_code>[^\ ]*)(.+?)(?=\d|-)(?<response_obj_size>[^\ ]*)(.+?)(?=")(?<request_info>.*)
    time_key: time
    time_format: "%d/%b/%Y:%H:%M:%S %z"

  - name: json_parser
    format: json
    time_key: time
    time_format: "%Y-%m-%dT%H:%M:%S%z"`

	return fluentBitData
}
