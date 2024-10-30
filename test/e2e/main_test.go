package e2e

import (
	"context"
	"fmt"
	"log"
	"os"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/e2e-framework/klient/wait"
	"sigs.k8s.io/e2e-framework/klient/wait/conditions"
	"sigs.k8s.io/e2e-framework/pkg/env"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/envfuncs"
	"sigs.k8s.io/e2e-framework/support/kind"
	"sigs.k8s.io/e2e-framework/support/utils"
)

var (
	testEnv env.Environment
)

const (
	dockerImage    = "ml-marklogic-operator-dev.bed-artifactory.bedford.progress.com/marklogic-kubernetes-operator:0.0.2"
	kustomizeVer   = "v5.5.0"
	ctrlgenVer     = "v0.16.5"
	namespace      = "marklogic-operator-system"
	marklogicImage = "marklogicdb/marklogic-db:11.2.0-ubi-rootless"
	kubernetesVer  = "v1.30.4"
)

func TestMain(m *testing.M) {
	testEnv = env.New()
	kindClusterName := "test-cluster"
	kindCluster := kind.NewCluster(kindClusterName)

	// Use Environment.Setup to configure pre-test setup
	testEnv.Setup(
		envfuncs.CreateClusterWithConfig(kindCluster, kindClusterName, "kind-config.yaml", kind.WithImage("kindest/node:"+kubernetesVer)),
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

			// generate manifest files
			log.Println("Generate manifests...")
			wd, _ := os.Getwd()
			log.Print(wd) // Output current working directory
			if p := utils.RunCommand(`controller-gen rbac:roleName=manager-role crd webhook paths="./..." output:crd:artifacts:config=config/crd/bases`); p.Err() != nil {
				log.Printf("Failed to generate manifests: %s: %s", p.Err(), p.Result())
				return ctx, p.Err()
			}

			// generate api objects
			log.Println("Generate API objects...")
			if p := utils.RunCommand(`controller-gen object:headerFile="hack/boilerplate.go.txt" paths="./..."`); p.Err() != nil {
				log.Printf("Failed to generate API objects: %s: %s", p.Err(), p.Result())
				return ctx, p.Err()
			}

			// Build docker image
			log.Println("Building docker image...")
			if p := utils.RunCommand(fmt.Sprintf("docker build -t %s .", dockerImage)); p.Err() != nil {
				log.Printf("Failed to build docker image: %s: %s", p.Err(), p.Result())
				return ctx, p.Err()
			}

			// Load docker image into kind
			log.Println("Loading docker image into kind cluster...")
			if err := kindCluster.LoadImage(ctx, dockerImage); err != nil {
				log.Printf("Failed to load image into kind: %s", err)
				return ctx, err
			}

			// Load MarkLogic image into kind
			log.Println("Loading docker image into kind cluster...")
			if err := kindCluster.LoadImage(ctx, marklogicImage); err != nil {
				log.Printf("Failed to load image into kind: %s", err)
				return ctx, err
			}

			// Deploy components
			log.Println("Deploying controller-manager resources...")
			p := utils.RunCommand(`kubectl version`)
			log.Printf("Output of kubectl: %s", p.Result())
			p = utils.RunCommand(`bash -c "kustomize build config/default | kubectl apply --server-side -f -"`)
			log.Printf("Output: %s", p.Result())
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
			log.Printf("Kind Nodes: %s", p.Result())

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
		// envfuncs.ExportClusterLogs(kindClusterName, "../../logs"),
		envfuncs.DestroyCluster(kindClusterName),
	)

	// Use Environment.Run to launch the test
	os.Exit(testEnv.Run(m))
}
