// Copyright (c) 2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

// Package manifests checks the shipped RBAC manifests, which are defined in four places that
// must stay consistent: the generated kustomize ClusterRole and namespaced Role, the Helm
// chart template, and the script that regenerates the chart.
package manifests

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

const reloaderRBACName = "marklogic-haproxy-reloader"

func repoFile(t *testing.T, elem ...string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(append([]string{"..", ".."}, elem...)...))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// rbacRuleChunks splits manifest text into its "- apiGroups:" rule blocks that concern
// Roles and RoleBindings.
func rbacRuleChunks(text string) []string {
	var chunks []string
	for _, chunk := range strings.Split(text, "- apiGroups:")[1:] {
		if strings.Contains(chunk, "rbac.authorization.k8s.io") && strings.Contains(chunk, "- rolebindings") {
			if end := strings.Index(chunk, "\n- "); end >= 0 {
				chunk = chunk[:end]
			}
			chunks = append(chunks, chunk)
		}
	}
	return chunks
}

// checkRoleRules verifies the operator may create Roles and RoleBindings but only read or
// modify the single one it manages.
func checkRoleRules(t *testing.T, where string, rules []rbacv1.PolicyRule) {
	t.Helper()
	var created, restricted bool
	for _, rule := range rules {
		if !slices.Contains(rule.APIGroups, "rbac.authorization.k8s.io") ||
			(!slices.Contains(rule.Resources, "roles") && !slices.Contains(rule.Resources, "rolebindings")) {
			continue
		}
		if len(rule.ResourceNames) == 0 {
			if !slices.Equal(rule.Verbs, []string{"create"}) {
				t.Errorf("%s: unrestricted access to Roles/RoleBindings beyond create (%v) would let the operator modify anyone's RBAC", where, rule.Verbs)
			}
			created = true
			continue
		}
		if !slices.Equal(rule.ResourceNames, []string{reloaderRBACName}) {
			t.Errorf("%s: unexpected object names %v", where, rule.ResourceNames)
		}
		for _, verb := range rule.Verbs {
			if !slices.Contains([]string{"get", "update", "patch"}, verb) {
				t.Errorf("%s: verb %q is not needed on the managed object", where, verb)
			}
		}
		restricted = len(rule.Verbs) == 3 && slices.Contains(rule.Resources, "roles") && slices.Contains(rule.Resources, "rolebindings")
	}
	if !created || !restricted {
		t.Errorf("%s: missing the create rule (%v) or the named get/update/patch rule (%v)", where, created, restricted)
	}
}

func TestKustomizeRolesRestrictRBACAccessToTheManagedObjects(t *testing.T) {
	var cluster rbacv1.ClusterRole
	if err := yaml.Unmarshal([]byte(repoFile(t, "config", "rbac", "role.yaml")), &cluster); err != nil {
		t.Fatal(err)
	}
	checkRoleRules(t, "config/rbac/role.yaml", cluster.Rules)
	var namespaced rbacv1.Role
	if err := yaml.Unmarshal([]byte(strings.SplitN(repoFile(t, "config", "rbac", "role_namespaced.yaml"), "\n---", 2)[0]), &namespaced); err != nil {
		t.Fatal(err)
	}
	checkRoleRules(t, "config/rbac/role_namespaced.yaml", namespaced.Rules)
}

func TestHelmChartAndItsGeneratorGrantTheSameRestrictedAccess(t *testing.T) {
	for _, file := range [][]string{
		{"charts", "marklogic-operator-kubernetes", "templates", "manager-rbac.yaml"},
		{"hack", "helmify-post-process.sh"},
	} {
		name := filepath.Join(file...)
		chunks := rbacRuleChunks(repoFile(t, file...))
		if len(chunks) != 4 { // a create rule and a named rule, for both the cluster and the namespaced role
			t.Fatalf("%s: expected 4 Role/RoleBinding rule blocks, found %d", name, len(chunks))
		}
		creates, named := 0, 0
		for _, chunk := range chunks {
			switch {
			case strings.Contains(chunk, "resourceNames:"):
				named++
				for _, want := range []string{"- " + reloaderRBACName, "- get", "- patch", "- update"} {
					if !strings.Contains(chunk, want) {
						t.Errorf("%s: named rule lacks %q:\n%s", name, want, chunk)
					}
				}
				for _, forbidden := range []string{"- list", "- watch", "- delete", "- create", `"*"`} {
					if strings.Contains(chunk, forbidden) {
						t.Errorf("%s: named rule must not grant %s:\n%s", name, forbidden, chunk)
					}
				}
			default:
				creates++
				for _, forbidden := range []string{"- get", "- list", "- watch", "- update", "- patch", "- delete"} {
					if strings.Contains(chunk, forbidden) {
						t.Errorf("%s: the unrestricted rule must only create, but grants %s:\n%s", name, forbidden, chunk)
					}
				}
			}
		}
		if creates != 2 || named != 2 {
			t.Errorf("%s: expected 2 create rules and 2 named rules, got %d and %d", name, creates, named)
		}
	}
}

func TestControllerMarkersMatchTheGeneratedClusterRole(t *testing.T) {
	// If the markers change without regenerating (make manifests), the shipped role drifts.
	markers := repoFile(t, "internal", "controller", "marklogiccluster_controller.go")
	if !strings.Contains(markers, "resources=roles;rolebindings,verbs=create") ||
		!strings.Contains(markers, "resourceNames="+reloaderRBACName+",verbs=get;update;patch") {
		t.Fatal("the kubebuilder markers no longer describe the restricted Role/RoleBinding access; update them and run `make manifests`")
	}
}
