package e2ehelm

import (
	"os"
	"strings"
	"testing"

	"github.com/marklogic/marklogic-operator-kubernetes/test/utils"
)

func TestVolumeAttributesClassNamespaceScoped(t *testing.T) {
	if !strings.EqualFold(os.Getenv("E2E_VAC_ENABLED"), "true") {
		t.Skip("set E2E_VAC_ENABLED=true on an isolated VAC-capable CSI cluster")
	}
	trackTest(t)
	const namespace = "ml-ns-vac"
	watched := false
	for _, candidate := range strings.Split(watchedNamespaces, ",") {
		watched = watched || strings.TrimSpace(candidate) == namespace
	}
	if !watched {
		t.Fatalf("VAC suite namespace %s must be included in E2E_WATCHED_NAMESPACES before Helm installation", namespace)
	}
	options, err := utils.VACOptionsFromEnvironment(namespace, marklogicImage, adminUsername, adminPassword)
	if err != nil {
		t.Fatal(err)
	}
	testEnv.Test(t, utils.VACCoreFeatures(options)...)
}
