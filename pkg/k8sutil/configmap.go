// Copyright (c) 2024-2025 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

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
	if strings.TrimSpace(oc.MarklogicGroup.Spec.LogCollection.Inputs) != "" {
		fluentBitData["fluent-bit.yaml"] += "\n" + normalizeYAMLIndentation(oc.MarklogicGroup.Spec.LogCollection.Inputs, 4, 6)
	} else {
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
	}

	// Add FILTER sections
	fluentBitData["fluent-bit.yaml"] += `

  filters:`
	if strings.TrimSpace(oc.MarklogicGroup.Spec.LogCollection.Filters) != "" {
		fluentBitData["fluent-bit.yaml"] += "\n" + normalizeYAMLIndentation(oc.MarklogicGroup.Spec.LogCollection.Filters, 4, 6)
	} else {
		fluentBitData["fluent-bit.yaml"] += `
	    - name: modify
	      match: "*"
	      add:
	        - pod ${POD_NAME}
	        - namespace ${NAMESPACE}
	    - name: modify
	      match: kube.marklogic.logs.error
	      add:
	        - tag kube.marklogic.logs.error
	    - name: modify
	      match: kube.marklogic.logs.access
	      add:
	        - tag kube.marklogic.logs.access
	    - name: modify
	      match: kube.marklogic.logs.request
	      add:
	        - tag kube.marklogic.logs.request
	    - name: modify
	      match: kube.marklogic.logs.audit
	      add:
	        - tag kube.marklogic.logs.audit
	    - name: modify
	      match: kube.marklogic.logs.crash
	      add:
	        - tag kube.marklogic.logs.crash
		`
	}

	// Add OUTPUT sections
	fluentBitData["fluent-bit.yaml"] += `

  outputs:`
	// Handle user-defined outputs from LogCollection.Outputs
	if strings.TrimSpace(oc.MarklogicGroup.Spec.LogCollection.Outputs) != "" {
		fluentBitData["fluent-bit.yaml"] += "\n" + normalizeYAMLIndentation(oc.MarklogicGroup.Spec.LogCollection.Outputs, 4, 6)
	} else {
		// Default stdout output if none specified
		fluentBitData["fluent-bit.yaml"] += `
    - name: stdout
      match: "*"
      format: json_lines`
	}

	// Parsers in YAML format
	fluentBitData["parsers.yaml"] = `parsers:`
	if strings.TrimSpace(oc.MarklogicGroup.Spec.LogCollection.Parsers) != "" {
		fluentBitData["parsers.yaml"] += "\n" + normalizeYAMLIndentation(oc.MarklogicGroup.Spec.LogCollection.Parsers, 2, 4)
	} else {
		fluentBitData["parsers.yaml"] += `
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
	}

	return fluentBitData
}

// normalizeYAMLIndentation processes user-provided YAML content and adjusts indentation
// to match the target YAML structure. This is useful when embedding user YAML into templates.
//
// Parameters:
//   - yamlContent: The raw YAML content string to process
//   - listItemIndent: Number of spaces for YAML list items (lines starting with "- ")
//   - propertyIndent: Number of spaces for properties under list items
//
// Returns: A string with normalized indentation, ready to embed in a larger YAML structure
//
// Example:
//
//	input := "- name: loki\n  host: loki.svc\n  port: 3100"
//	output := normalizeYAMLIndentation(input, 4, 6)
func normalizeYAMLIndentation(yamlContent string, listItemIndent, propertyIndent int) string {
	// Purpose: Re-indent user supplied YAML fragments representing top-level lists of items
	// (filters, outputs, inputs, parsers) while supporting nested lists under a property key.
	// Rules:
	//   Top-level list items: listItemIndent spaces (e.g. 4)
	//   Properties under a list item: propertyIndent spaces (e.g. 6)
	//   Nested list items under a property that ends with ':' (e.g. add:, set:, rename:): propertyIndent + 2
	//   We ignore original indentation entirely for consistency.
	if yamlContent == "" {
		return ""
	}

	lines := strings.Split(yamlContent, "\n")
	processed := make([]string, 0, len(lines))
	var lastNonEmpty string
	inNestedList := false

	for _, raw := range lines {
		// Replace any tab with 4 spaces to avoid invalid YAML tokens
		raw = strings.ReplaceAll(raw, "\t", "    ")
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" { // skip blank lines
			continue
		}

		indent := 0
		isListItem := strings.HasPrefix(trimmed, "- ")

		// Detect transition into nested list context: previous line is a property ending with ':'
		// and current line is a list item
		if isListItem && strings.HasSuffix(lastNonEmpty, ":") && !strings.HasPrefix(lastNonEmpty, "- ") {
			inNestedList = true
		}

		// Exit nested list context when we encounter a property line (not a list item)
		// unless it's the parent property that started the list
		if !isListItem && !strings.HasSuffix(trimmed, ":") {
			inNestedList = false
		}

		// Also exit nested list when we hit a top-level list item (starts with '- name:')
		if isListItem && strings.HasPrefix(trimmed, "- name:") {
			inNestedList = false
		}

		switch {
		case isListItem && inNestedList:
			// nested list item (e.g. under add:, set:, rename:)
			indent = propertyIndent + 2
		case isListItem:
			// top-level list item
			indent = listItemIndent
		default:
			// property line (key: value or key:)
			indent = propertyIndent
		}

		processed = append(processed, strings.Repeat(" ", indent)+trimmed)
		lastNonEmpty = trimmed
	}
	return strings.Join(processed, "\n")
}
