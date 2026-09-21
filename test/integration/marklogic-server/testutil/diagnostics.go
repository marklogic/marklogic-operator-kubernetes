// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package testutil

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type containerDiagnostic struct {
	Name     string `json:"name"`
	Image    string `json:"image"`
	ImageID  string `json:"imageID,omitempty"`
	Ready    bool   `json:"ready"`
	Restarts int32  `json:"restarts"`
	Reason   string `json:"reason,omitempty"`
	ExitCode *int32 `json:"exitCode,omitempty"`
}
type podDiagnostic struct {
	Name       string                `json:"name"`
	Node       string                `json:"node,omitempty"`
	Phase      corev1.PodPhase       `json:"phase"`
	Containers []containerDiagnostic `json:"containers"`
}
type eventDiagnostic struct {
	Object  string `json:"object"`
	Type    string `json:"type"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
	Count   int32  `json:"count"`
}
type diagnostics struct {
	Pods   []podDiagnostic   `json:"pods"`
	Events []eventDiagnostic `json:"events"`
	Errors []string          `json:"errors,omitempty"`
	Logs   []string          `json:"logs,omitempty"`
}

func summarizePod(pod corev1.Pod) podDiagnostic {
	result := podDiagnostic{Name: pod.Name, Node: pod.Spec.NodeName, Phase: pod.Status.Phase}
	for _, status := range append(append([]corev1.ContainerStatus{}, pod.Status.InitContainerStatuses...), pod.Status.ContainerStatuses...) {
		entry := containerDiagnostic{Name: status.Name, Image: status.Image, ImageID: status.ImageID, Ready: status.Ready, Restarts: status.RestartCount}
		if status.State.Waiting != nil {
			entry.Reason = status.State.Waiting.Reason
		}
		if status.State.Terminated != nil {
			entry.Reason = status.State.Terminated.Reason
			code := status.State.Terminated.ExitCode
			entry.ExitCode = &code
		}
		result.Containers = append(result.Containers, entry)
	}
	return result
}

// collectDiagnostics has a total deadline so diagnostics cannot indefinitely
// delay namespace cleanup. It never serializes pod specs or complete manifests.
func (r *Run) collectDiagnostics(t *testing.T) {
	t.Helper()
	if r.report == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	result := diagnostics{}
	addError := func(operation string, err error) {
		result.Errors = append(result.Errors, r.report.redactor.text(operation+": "+err.Error()))
	}
	pods, err := r.client.CoreV1().Pods(r.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		addError("list pods", err)
	} else {
		for _, pod := range pods.Items {
			result.Pods = append(result.Pods, summarizePod(pod))
		}
	}
	events, err := r.client.CoreV1().Events(r.Namespace).List(ctx, metav1.ListOptions{Limit: 200})
	if err != nil {
		addError("list events", err)
	} else {
		for _, event := range events.Items {
			message := r.report.redactor.text(event.Message)
			if len(message) > 4096 {
				message = message[:4096] + " [truncated]"
			}
			result.Events = append(result.Events, eventDiagnostic{Object: event.InvolvedObject.Kind + "/" + event.InvolvedObject.Name, Type: event.Type, Reason: event.Reason, Message: message, Count: event.Count})
		}
	}
	if pods != nil {
		for _, pod := range pods.Items {
			for _, container := range summarizePod(pod).Containers {
				for _, previous := range []bool{false, true} {
					if previous && container.Restarts == 0 {
						continue
					}
					if ctx.Err() != nil {
						break
					}
					name := pod.Name + "-" + container.Name
					if previous {
						name += "-previous"
					}
					name += ".log"
					content, err := r.readLog(ctx, pod.Name, container.Name, previous)
					if err != nil {
						addError(name, err)
						continue
					}
					if err := os.WriteFile(filepath.Join(r.report.dir, name), []byte(r.report.redactor.text(content)), 0600); err != nil {
						addError("write "+name, err)
					} else {
						result.Logs = append(result.Logs, name)
					}
				}
			}
		}
	}
	if ctx.Err() != nil {
		addError("diagnostics deadline", ctx.Err())
	}
	if err := writeJSON(r.report.dir, "diagnostics.json", result); err != nil {
		t.Errorf("Write diagnostics summary: %v", err)
	}
	t.Logf("Saved diagnostics to %s (%d collection errors)", r.report.dir, len(result.Errors))
}

func (r *Run) readLog(parent context.Context, pod, container string, previous bool) (string, error) {
	ctx, cancel := context.WithTimeout(parent, 20*time.Second)
	defer cancel()
	tail, limit := int64(200), int64(64*1024)
	stream, err := r.client.CoreV1().Pods(r.Namespace).GetLogs(pod, &corev1.PodLogOptions{Container: container, Previous: previous, TailLines: &tail, LimitBytes: &limit}).Stream(ctx)
	if err != nil {
		return "", err
	}
	defer stream.Close()
	contents, err := io.ReadAll(io.LimitReader(stream, limit))
	if err != nil {
		return "", fmt.Errorf("read log: %w", err)
	}
	return string(contents), nil
}
