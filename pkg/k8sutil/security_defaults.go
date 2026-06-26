// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package k8sutil

import (
	corev1 "k8s.io/api/core/v1"
)

// getHAProxyPodSecurityContextOrDefault returns the provided pod security context,
// or a secure default if nil is provided.
func getHAProxyPodSecurityContextOrDefault(ctx *corev1.PodSecurityContext) *corev1.PodSecurityContext {
	if ctx != nil {
		return ctx
	}

	return &corev1.PodSecurityContext{
		FSGroup: int64Ptr(101),
	}
}

// getHAProxyContainerSecurityContextOrDefault returns the provided container security context,
// or a secure default if nil is provided.
func getHAProxyContainerSecurityContextOrDefault(ctx *corev1.SecurityContext) *corev1.SecurityContext {
	if ctx != nil {
		return ctx
	}

	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: boolPtr(false),
		ReadOnlyRootFilesystem:   boolPtr(false),
		Capabilities: &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
			Add:  []corev1.Capability{"NET_BIND_SERVICE"},
		},
	}
}

// getMarkLogicContainerSecurityContextOrDefault returns the provided container security context,
// or a secure default if nil is provided.
// Identity fields are intentionally left unset to avoid overriding pod-level
// runAs* settings when only podSecurityContext is provided.
func getMarkLogicContainerSecurityContextOrDefault(ctx *corev1.SecurityContext) *corev1.SecurityContext {
	if ctx != nil {
		return ctx
	}

	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: boolPtr(false),
		ReadOnlyRootFilesystem:   boolPtr(false),
		Capabilities: &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
		},
	}
}

// getFluentBitSecurityContextOrDefault returns the provided container security context,
// or a secure default if nil is provided.
func getFluentBitSecurityContextOrDefault(ctx *corev1.SecurityContext) *corev1.SecurityContext {
	if ctx != nil {
		return ctx
	}

	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: boolPtr(false),
		ReadOnlyRootFilesystem:   boolPtr(true),
		Capabilities: &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
		},
	}
}

// Helper functions for pointer creation
func int64Ptr(v int64) *int64 {
	return &v
}

func boolPtr(v bool) *bool {
	return &v
}
