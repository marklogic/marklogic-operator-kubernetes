// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package backup

import (
	"fmt"

	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
	"github.com/marklogic/marklogic-operator-kubernetes/test/integration/marklogic-server/fixtures/marklogiccluster"
	tlsfixture "github.com/marklogic/marklogic-operator-kubernetes/test/integration/marklogic-server/fixtures/tls"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func serverHost(namespace string, ordinal int) string {
	return fmt.Sprintf("%s-%d.%s.%s.svc.cluster.local", clusterName, ordinal, clusterName, namespace)
}

func buildObjects(c config, namespace, storageClass, password string) ([]runtime.Object, error) {
	certs, err := tlsfixture.BuildClusterResources(tlsfixture.ClusterConfig{
		Namespace: namespace, CASecretName: "backup-ca", Servers: []tlsfixture.ServerConfig{
			{TLSSecretName: "backup-server-0-tls", DNSNames: []string{serverHost(namespace, 0)}},
			{TLSSecretName: "backup-server-1-tls", DNSNames: []string{serverHost(namespace, 1)}},
		},
	})
	if err != nil {
		return nil, err
	}
	cluster, err := marklogiccluster.Build(marklogiccluster.Config{Namespace: namespace, Name: clusterName, Image: c.Image, StorageClass: storageClass, AdminUsername: "admin", AdminPassword: password, CASecretName: certs.CASecret.Name, CertificateSecretNames: []string{certs.TLSSecrets[0].Name, certs.TLSSecrets[1].Name}})
	if err != nil {
		return nil, err
	}
	// Backup/status/restore all use the same coordinator directly, without OAuth
	// infrastructure or a load balancer that could route status to another host.
	cluster.Spec.HAProxy.Enabled = false
	cluster.Spec.ObjectStorage = &marklogicv1.ObjectStorageConfig{AWS: &marklogicv1.AWSObjectStorage{AuthType: marklogicv1.ObjectStorageAuthSecret, SecretName: credentialSecret}}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: credentialSecret, Namespace: namespace}, StringData: map[string]string{"accessKey": c.AWS.AccessKey, "secretKey": c.AWS.SecretKey}}
	if c.AWS.SessionToken != "" {
		secret.StringData["sessionToken"] = c.AWS.SessionToken
	}
	// The generated password contains no quotes/newlines. Mount it instead of
	// sending it as a kubectl exec argument or logging an authentication response.
	auth := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "backup-client-auth", Namespace: namespace}, StringData: map[string]string{"curl.conf": fmt.Sprintf("user = \"admin:%s\"\ndigest\n", password)}}
	no := false
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: clientName, Namespace: namespace}, Spec: corev1.PodSpec{
		AutomountServiceAccountToken: &no, RestartPolicy: corev1.RestartPolicyNever,
		Containers: []corev1.Container{{Name: "curl", Image: "curlimages/curl:8.12.1", Command: []string{"sleep", "infinity"}, VolumeMounts: []corev1.VolumeMount{{Name: "ca", MountPath: "/etc/backup/ca", ReadOnly: true}, {Name: "auth", MountPath: "/etc/backup/auth", ReadOnly: true}, {Name: "work", MountPath: "/work"}}}},
		Volumes: []corev1.Volume{
			{Name: "ca", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: certs.CASecret.Name}}},
			{Name: "auth", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: auth.Name}}},
			{Name: "work", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		},
	}}
	return []runtime.Object{certs.CASecret, certs.TLSSecrets[0], certs.TLSSecrets[1], secret, auth, cluster, pod}, nil
}
