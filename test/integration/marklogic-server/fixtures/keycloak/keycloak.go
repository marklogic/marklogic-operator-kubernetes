// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package keycloak

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/marklogic/marklogic-operator-kubernetes/test/integration/marklogic-server/testutil"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const (
	Image    = "quay.io/keycloak/keycloak:26.0.7"
	Name     = "keycloak"
	Realm    = "marklogic-oauth"
	ClientID = "marklogic-oauth-client"
	// AuthCodeClientID / AuthCodeClientSecret identify a dedicated confidential
	// client used by the Authorization Code flow. MarkLogic 12.1 authenticates at
	// the token endpoint with a client secret, so the Authorization Code client
	// must be confidential while the public ClientID above keeps serving the
	// resource-server (direct access grant) flows.
	AuthCodeClientID     = "marklogic-oauth-authcode-client"
	AuthCodeClientSecret = "marklogic-oauth-authcode-secret"
	TestUsername         = "oauth-test-user"
	TestUserPassword     = "oauth-test-password"
	AdminUsername        = "oauth-test-admin"
	AdminPassword        = "oauth-test-admin-password"
	RealmConfigMapKey    = "realm-marklogic-oauth.json"
)

type Config struct {
	Namespace     string
	RedirectURI   string
	IssuerURL     string
	TLSSecretName string
}

type Resources struct {
	AdminSecret    *corev1.Secret
	RealmConfigMap *corev1.ConfigMap
	Deployment     *appsv1.Deployment
	Service        *corev1.Service
}

// Deploy applies the Keycloak fixture resources and waits for OpenID discovery to become ready.
func Deploy(t *testing.T, config Config) {
	t.Helper()
	resources, err := BuildResources(config)
	if err != nil {
		t.Fatal(err)
	}
	testutil.ApplyObjects(t, resources.AdminSecret, resources.RealmConfigMap, resources.Deployment, resources.Service)
	testutil.WaitForDeploymentAvailable(t, config.Namespace, Name, 3*time.Minute)
}

func BuildResources(config Config) (Resources, error) {
	if config.Namespace == "" {
		return Resources{}, fmt.Errorf("namespace is required")
	}
	if config.IssuerURL == "" {
		return Resources{}, fmt.Errorf("issuer URL is required")
	}
	if config.TLSSecretName == "" {
		return Resources{}, fmt.Errorf("TLS Secret name is required")
	}

	realmImport, err := json.Marshal(realmDefinition{RedirectURI: config.RedirectURI})
	if err != nil {
		return Resources{}, fmt.Errorf("marshal Keycloak realm import: %w", err)
	}
	labels := map[string]string{"app.kubernetes.io/name": Name}
	replicas := int32(1)

	return Resources{
		AdminSecret: &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: Name + "-admin", Namespace: config.Namespace},
			StringData: map[string]string{
				"username": AdminUsername,
				"password": AdminPassword,
			},
		},
		RealmConfigMap: &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: Name + "-realm", Namespace: config.Namespace},
			Data:       map[string]string{RealmConfigMapKey: string(realmImport)},
		},
		Deployment: &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: Name, Namespace: config.Namespace, Labels: labels},
			Spec: appsv1.DeploymentSpec{
				Replicas: &replicas,
				Selector: &metav1.LabelSelector{MatchLabels: labels},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: labels},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{
							Name:    Name,
							Image:   Image,
							Command: []string{"/opt/keycloak/bin/kc.sh"},
							Args: []string{
								"start",
								"--import-realm",
								"--http-enabled=false",
								"--https-port=8443",
								"--https-certificate-file=/etc/x509/https/tls.crt",
								"--https-certificate-key-file=/etc/x509/https/tls.key",
								"--hostname=" + config.IssuerURL,
							},
							Env: []corev1.EnvVar{
								{Name: "KC_BOOTSTRAP_ADMIN_USERNAME", ValueFrom: secretValue("username")},
								{Name: "KC_BOOTSTRAP_ADMIN_PASSWORD", ValueFrom: secretValue("password")},
							},
							Ports: []corev1.ContainerPort{{Name: "https", ContainerPort: 8443}},
							ReadinessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/realms/" + Realm + "/.well-known/openid-configuration", Port: intstr.FromInt(8443), Scheme: corev1.URISchemeHTTPS}},
							},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "realm-import", MountPath: "/opt/keycloak/data/import", ReadOnly: true},
								{Name: "https", MountPath: "/etc/x509/https", ReadOnly: true},
							},
						}},
						Volumes: []corev1.Volume{
							{
								Name: "realm-import",
								VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{Name: Name + "-realm"},
								}},
							},
							{Name: "https", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: config.TLSSecretName}}},
						},
					},
				},
			},
		},
		Service: &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: Name, Namespace: config.Namespace, Labels: labels},
			Spec: corev1.ServiceSpec{
				Selector: labels,
				Ports:    []corev1.ServicePort{{Name: "https", Port: 8443, TargetPort: intstr.FromInt(8443)}},
			},
		},
	}, nil
}

func secretValue(key string) *corev1.EnvVarSource {
	return &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: Name + "-admin"},
		Key:                  key,
	}}
}

type realmDefinition struct {
	Realm       string          `json:"realm"`
	Enabled     bool            `json:"enabled"`
	RedirectURI string          `json:"-"`
	Clients     []realmClient   `json:"clients"`
	Users       []realmTestUser `json:"users"`
}

func (definition realmDefinition) MarshalJSON() ([]byte, error) {
	type payload realmDefinition
	client := realmClient{ClientID: ClientID, Enabled: true, Protocol: "openid-connect", PublicClient: true, DirectAccessGrantsEnabled: true, StandardFlowEnabled: true}
	// A confidential client for the Authorization Code flow: MarkLogic 12.1 sends
	// a client secret when exchanging the authorization code at the token endpoint.
	authCodeClient := realmClient{ClientID: AuthCodeClientID, Enabled: true, Protocol: "openid-connect", PublicClient: false, Secret: AuthCodeClientSecret, StandardFlowEnabled: true}
	if definition.RedirectURI != "" {
		client.RedirectURIs = []string{definition.RedirectURI}
		authCodeClient.RedirectURIs = []string{definition.RedirectURI}
	}
	return json.Marshal(payload{
		Realm:   Realm,
		Enabled: true,
		Clients: []realmClient{client, authCodeClient},
		Users:   []realmTestUser{{Username: TestUsername, FirstName: "OAuth", LastName: "Test", Email: TestUsername + "@example.test", Enabled: true, EmailVerified: true, Credentials: []realmCredential{{Type: "password", Value: TestUserPassword, Temporary: false}}}},
	})
}

type realmClient struct {
	ClientID                  string   `json:"clientId"`
	Enabled                   bool     `json:"enabled"`
	Protocol                  string   `json:"protocol"`
	PublicClient              bool     `json:"publicClient"`
	Secret                    string   `json:"secret,omitempty"`
	DirectAccessGrantsEnabled bool     `json:"directAccessGrantsEnabled"`
	StandardFlowEnabled       bool     `json:"standardFlowEnabled"`
	RedirectURIs              []string `json:"redirectUris,omitempty"`
}

type realmTestUser struct {
	Username      string            `json:"username"`
	FirstName     string            `json:"firstName"`
	LastName      string            `json:"lastName"`
	Email         string            `json:"email"`
	Enabled       bool              `json:"enabled"`
	EmailVerified bool              `json:"emailVerified"`
	Credentials   []realmCredential `json:"credentials"`
}

type realmCredential struct {
	Type      string `json:"type"`
	Value     string `json:"value"`
	Temporary bool   `json:"temporary"`
}
