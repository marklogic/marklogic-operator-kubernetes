package e2e

import (
	"os"
	"strings"
	"testing"

	"github.com/marklogic/marklogic-operator-kubernetes/test/utils"
)

func TestVolumeAttributesClassClusterScoped(t *testing.T) {
	if !strings.EqualFold(os.Getenv("E2E_VAC_ENABLED"), "true") {
		t.Skip("set E2E_VAC_ENABLED=true on an isolated VAC-capable CSI cluster")
	}
	trackTest(t)
	options, err := utils.VACOptionsFromEnvironment("ml-vac", marklogicImage, "admin", "Admin@8001")
	if err != nil {
		t.Fatal(err)
	}
	testEnv.Test(t, utils.VACCoreFeatures(options)...)
}
