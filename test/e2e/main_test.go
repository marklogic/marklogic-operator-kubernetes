// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package e2e

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/e2e-framework/klient/conf"
	"sigs.k8s.io/e2e-framework/klient/wait"
	"sigs.k8s.io/e2e-framework/klient/wait/conditions"
	"sigs.k8s.io/e2e-framework/pkg/env"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/envfuncs"
	"sigs.k8s.io/e2e-framework/pkg/utils"
)

var (
	testEnv        env.Environment
	dockerImage    = os.Getenv("E2E_DOCKER_IMAGE")
	kustomizeVer   = os.Getenv("E2E_KUSTOMIZE_VERSION")
	ctrlgenVer     = os.Getenv("E2E_CONTROLLER_TOOLS_VERSION")
	marklogicImage = os.Getenv("E2E_MARKLOGIC_IMAGE_VERSION")
	kubernetesVer  = os.Getenv("E2E_KUBERNETES_VERSION")
	e2eScopeType   = os.Getenv("E2E_SCOPE_TYPE")
	e2eWatchNS     = os.Getenv("E2E_WATCH_NAMESPACE")
)

// scopeType and watchedNSs are set during TestMain setup and used by all tests.
var (
	scopeType  string
	watchedNSs []string
)

// allTestNamespaces is the union of every application namespace used across the e2e suite.
// Used to compute WATCH_NAMESPACE when running in namespace-scoped mode.
var allTestNamespaces = []string{
	"default",               // 2_marklogic_cluster_test.go
	"ednode",                // 3_ml_cluster_ednode_test.go
	"tls-self-signed",       // 4_tls_test.go
	"marklogic-tlsnamed",    // 4_tls_test.go
	"marklogic-tlsednode",   // 4_tls_test.go
	"haproxy-pathbased",     // 5_haproxy_test.go
	"haproxy-test",          // 5_haproxy_test.go
	"log-test",              // 6_log_collection_test.go
	"istio-ambient-test",    // 7_istio_ambient_test.go
	"istio-resilience-test", // 7_istio_ambient_test.go
	"istio-multinode-test",  // 7_istio_ambient_test.go
	"non-istio-test",        // 7_istio_ambient_test.go
}

const (
	namespace = "marklogic-operator-system"
)

func TestMain(m *testing.M) {
	testEnv = env.New()
	path := conf.ResolveKubeConfigFile()
	cfg, err := envconf.NewFromFlags()
	if err != nil {
		log.Fatalf("Failed to create config: %s", err)
	}
	cfg = cfg.WithKubeconfigFile(path)

	testEnv = env.NewWithConfig(cfg)

	log.Printf("Running tests with the following configurations: path=%s", path)

	log.Printf("Docker image: %s", dockerImage)
	log.Printf("Kustomize version: %s", kustomizeVer)
	log.Printf("Controller-gen version: %s", ctrlgenVer)
	log.Printf("MarkLogic image: %s", marklogicImage)
	log.Printf("Kubernetes version: %s", kubernetesVer)

	// Use Environment.Setup to configure pre-test setup
	testEnv.Setup(
		envfuncs.CreateNamespace(namespace),

		// install tool dependencies
		func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
			log.Println("Installing bin tools...")

			// change dir for Make file or it will fail
			if err := os.Chdir("../.."); err != nil {
				log.Printf("Unable to set working directory: %s", err)
				return ctx, err
			}
			wd, _ := os.Getwd()
			os.Setenv("GOBIN", wd+"/bin")
			os.Setenv("PATH", os.Getenv("PATH")+":"+os.Getenv("GOBIN"))

			if p := utils.RunCommand(fmt.Sprintf("go install sigs.k8s.io/kustomize/kustomize/v5@%s", kustomizeVer)); p.Err() != nil {
				log.Printf("Failed to install kustomize binary: %s: %s", p.Err(), p.Result())
				return ctx, p.Err()
			}
			if p := utils.RunCommand(fmt.Sprintf("go install sigs.k8s.io/controller-tools/cmd/controller-gen@%s", ctrlgenVer)); p.Err() != nil {
				log.Printf("Failed to install controller-gen binary: %s: %s", p.Err(), p.Result())
				return ctx, p.Err()
			}

			p := utils.RunCommand("kustomize version")
			log.Printf("Kustomize version: %s", p.Result())
			return ctx, nil
		},

		// generate and deploy resource configurations
		func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
			log.Println("Building source components...")

			c := utils.RunCommand("controller-gen --version")
			log.Printf("controller-gen: %s", c.Result())

			// Deploy components
			log.Println("Deploying controller-manager resources...")
			p := utils.RunCommand(`kubectl version`)
			log.Printf("Output of kubectl: %s", p.Result())
			p = utils.RunCommand(`make deploy`)
			log.Printf("Output of make deploy: %s", p.Result())
			if p.Err() != nil {
				log.Printf("Failed to deploy resource configurations: %s: %s", p.Err(), p.Result())
				return ctx, p.Err()
			}

			// wait for controller-manager to be ready
			log.Println("Waiting for controller-manager deployment to be available...")
			client := cfg.Client()
			if err := wait.For(
				conditions.New(client.Resources()).DeploymentConditionMatch(&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "marklogic-operator-controller-manager", Namespace: namespace}},
					appsv1.DeploymentProgressing,
					v1.ConditionTrue),
				wait.WithTimeout(3*time.Minute),
				wait.WithInterval(10*time.Second),
			); err != nil {
				log.Printf("Timed out while waiting for deployment: %s", err)
				return ctx, err
			}

			p = utils.RunCommand(`kubectl get nodes`)
			log.Printf("Kubernetes Nodes: %s", p.Result())

			return ctx, nil
		},

		// Configure namespace-scoped watch (E2E_SCOPE_TYPE=namespace) and/or insecure
		// metrics (E2E_METRICS_SECURE=false) by patching the live Deployment and metrics
		// Service so the cluster state matches what the test suite expects.
		//
		// `make deploy` always uses config/default which sets --metrics-bind-address=:8443
		// and --metrics-secure=true. When E2E_METRICS_SECURE=false this step patches both
		// the Deployment args/ports and the metrics Service port to :8080 so that
		// TestMetricsEndpoint's HTTP port-forward succeeds.
		//
		// NOTE: this step only patches WATCH_NAMESPACE to test reconciliation scoping.
		// The cluster-scoped RBAC (ClusterRole/ClusterRoleBinding) from `make deploy`
		// remains in place. A true namespace-scoped install would use Role/RoleBinding
		// per watched namespace (see config/rbac/role_namespaced.yaml); that RBAC path
		// is not exercised here.
		func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
			scopeType = e2eScopeType
			if scopeType == "" {
				scopeType = "cluster"
			}
			metricsSecure := os.Getenv("E2E_METRICS_SECURE") != "false" // default true
			log.Printf("Operator scope: %s  metrics-secure: %v", scopeType, metricsSecure)

			// Nothing to patch for a default cluster-scoped + secure-metrics deployment.
			if scopeType != "namespace" && metricsSecure {
				return ctx, nil
			}

			// -- WATCH_NAMESPACE --------------------------------------------------
			var watchNS string
			if scopeType == "namespace" {
				watchNS = e2eWatchNS
				if watchNS == "" {
					watchNS = strings.Join(allTestNamespaces, ",")
				}
				parts := strings.Split(watchNS, ",")
				for i, p := range parts {
					parts[i] = strings.TrimSpace(p)
				}
				watchedNSs = parts
				log.Printf("Namespace-scoped mode: WATCH_NAMESPACE=%s", watchNS)
			}

			client := cfg.Client()

			// -- Patch the Deployment ---------------------------------------------
			dep := &appsv1.Deployment{}
			if err := client.Resources().Get(ctx, "marklogic-operator-controller-manager", namespace, dep); err != nil {
				return ctx, fmt.Errorf("failed to get operator deployment: %w", err)
			}
			for i, c := range dep.Spec.Template.Spec.Containers {
				if c.Name != "manager" {
					continue
				}
				if watchNS != "" {
					filtered := make([]v1.EnvVar, 0, len(c.Env)+1)
					for _, e := range c.Env {
						if e.Name != "WATCH_NAMESPACE" {
							filtered = append(filtered, e)
						}
					}
					filtered = append(filtered, v1.EnvVar{Name: "WATCH_NAMESPACE", Value: watchNS})
					dep.Spec.Template.Spec.Containers[i].Env = filtered
				}
				if !metricsSecure {
					newArgs := make([]string, 0, len(c.Args))
					for _, arg := range c.Args {
						switch arg {
						case "--metrics-bind-address=:8443":
							newArgs = append(newArgs, "--metrics-bind-address=:8080")
						case "--metrics-secure=true":
							newArgs = append(newArgs, "--metrics-secure=false")
						default:
							newArgs = append(newArgs, arg)
						}
					}
					dep.Spec.Template.Spec.Containers[i].Args = newArgs
					newPorts := make([]v1.ContainerPort, 0, len(c.Ports))
					for _, p := range c.Ports {
						if p.ContainerPort == 8443 {
							newPorts = append(newPorts, v1.ContainerPort{ContainerPort: 8080, Protocol: v1.ProtocolTCP, Name: "http"})
						} else {
							newPorts = append(newPorts, p)
						}
					}
					dep.Spec.Template.Spec.Containers[i].Ports = newPorts
					log.Printf("Patching operator deployment to insecure metrics on :8080")
				}
				break
			}
			if err := client.Resources().Update(ctx, dep); err != nil {
				return ctx, fmt.Errorf("failed to patch operator deployment: %w", err)
			}

			// -- Patch the metrics Service port to match --------------------------
			if !metricsSecure {
				svc := &v1.Service{}
				if err := client.Resources().Get(ctx, "marklogic-operator-controller-manager-metrics-service", namespace, svc); err != nil {
					return ctx, fmt.Errorf("failed to get metrics Service: %w", err)
				}
				for i, p := range svc.Spec.Ports {
					if p.Port == 8443 {
						svc.Spec.Ports[i].Port = 8080
						svc.Spec.Ports[i].Name = "http"
						svc.Spec.Ports[i].TargetPort = intstr.FromInt32(8080)
					}
				}
				if err := client.Resources().Update(ctx, svc); err != nil {
					return ctx, fmt.Errorf("failed to patch metrics Service: %w", err)
				}
				log.Printf("Patched metrics Service to insecure port :8080")
			}

			// -- Wait for the operator to re-roll out -----------------------------
			if err := wait.For(
				conditions.New(client.Resources()).DeploymentConditionMatch(
					&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
						Name:      "marklogic-operator-controller-manager",
						Namespace: namespace,
					}},
					appsv1.DeploymentAvailable,
					v1.ConditionTrue,
				),
				wait.WithTimeout(3*time.Minute),
				wait.WithInterval(5*time.Second),
			); err != nil {
				return ctx, fmt.Errorf("operator rollout timed out: %w", err)
			}
			log.Printf("Operator re-deployed: scope=%s metrics-secure=%v", scopeType, metricsSecure)
			return ctx, nil
		},
	)

	// Use the Environment.Finish method to define clean up steps
	testEnv.Finish(
		func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
			log.Println("Finishing tests, cleaning cluster ...")
			utils.RunCommand(`bash -c "kustomize build config/default | kubectl delete -f -"`)
			return ctx, nil
		},
		envfuncs.DeleteNamespace(namespace),
	)

	// Use Environment.Run to launch the test
	os.Exit(testEnv.Run(m))
}

// skipIfNamespaceNotWatched skips t if the operator is running in namespace-scoped mode
// and ns is not in the configured WATCH_NAMESPACE list. This prevents false failures
// caused by the operator simply not reconciling resources in an unwatched namespace.
//
// Pass E2E_WATCH_NAMESPACE=<comma-separated namespaces> to control which namespaces
// are watched; when unset it defaults to all known test namespaces.
func skipIfNamespaceNotWatched(t *testing.T, ns string) {
	t.Helper()
	if scopeType != "namespace" {
		return
	}
	for _, n := range watchedNSs {
		if n == ns {
			return
		}
	}
	t.Skipf("namespace %q is not in WATCH_NAMESPACE %v; add it to E2E_WATCH_NAMESPACE to run this test in namespace-scoped mode", ns, watchedNSs)
}
