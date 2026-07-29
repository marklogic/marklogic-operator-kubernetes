// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package oauthclient

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	DefaultImage  = "curlimages/curl:8.12.1"
	DefaultName   = "oauth-client"
	CAPath        = "/etc/oauth-test/ca/cacert.pem"
	CookieJarPath = "/work/cookies.txt"
)

type Config struct {
	Namespace    string
	Name         string
	Image        string
	CASecretName string
}

// Build creates an idle curl client Pod with a persistent cookie jar and the test CA mounted.
func Build(config Config) (*corev1.Pod, error) {
	if config.Namespace == "" {
		return nil, fmt.Errorf("namespace is required")
	}
	if config.CASecretName == "" {
		return nil, fmt.Errorf("CA Secret name is required")
	}
	name := config.Name
	if name == "" {
		name = DefaultName
	}
	image := config.Image
	if image == "" {
		image = DefaultImage
	}
	labels := map[string]string{"app.kubernetes.io/name": name}

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: config.Namespace, Labels: labels},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name:    "curl",
				Image:   image,
				Command: []string{"sleep", "infinity"},
				Env:     []corev1.EnvVar{{Name: "CURL_CA_BUNDLE", Value: CAPath}},
				VolumeMounts: []corev1.VolumeMount{
					{Name: "test-ca", MountPath: "/etc/oauth-test/ca", ReadOnly: true},
					{Name: "work", MountPath: "/work"},
				},
			}},
			Volumes: []corev1.Volume{
				{Name: "test-ca", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: config.CASecretName}}},
				{Name: "work", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			},
		},
	}, nil
}
