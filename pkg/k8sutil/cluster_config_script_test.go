package k8sutil

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

func TestCurlRetryValidateReturnsShellStatus(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("script test requires bash")
	}
	script, err := os.ReadFile("scripts/cluster-config.sh")
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(script), "function curl_retry_validate {")
	if start < 0 {
		t.Fatal("curl_retry_validate not found in cluster-config.sh")
	}
	end := strings.Index(string(script)[start:], "\n}\n")
	if end < 0 {
		t.Fatal("end of curl_retry_validate not found in cluster-config.sh")
	}
	function := string(script)[start : start+end+3]

	for _, tc := range []struct {
		name, returnError, mockCode, expected string
		mockExit                              int
		wantStatus                            string // empty means the script must exit before reporting
	}{
		{"expected-200", "false", "200", "200", 0, "0"},
		{"expected-202", "false", "202", "202", 0, "0"},
		{"transport-failure", "false", "000", "200", 28, "1"},
		{"unexpected-500", "false", "500", "200", 0, "1"},
		{"unexpected-404", "false", "404", "200", 0, "1"},
		{"success-code-for-other-expectation", "false", "200", "202", 0, "1"},
		{"exit-on-failure", "true", "500", "200", 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			harness := `
N_RETRY=2
RETRY_INTERVAL=0
error() { echo "$1" >&2; [[ "$2" == exit ]] && exit 1; }
curl() { printf '%s' "$MOCK_CODE"; return "$MOCK_EXIT"; }
` + function + `
curl_retry_validate "$RETURN_ERROR" http://example.invalid/ "$EXPECTED"
echo "status=$?"
`
			cmd := exec.Command("bash", "-c", harness)
			cmd.Env = append(os.Environ(), "RETURN_ERROR="+tc.returnError, "MOCK_CODE="+tc.mockCode,
				"MOCK_EXIT="+strconv.Itoa(tc.mockExit), "EXPECTED="+tc.expected)
			out, _ := cmd.CombinedOutput()
			reported := strings.Contains(string(out), "status=")
			if tc.wantStatus == "" {
				if reported {
					t.Fatalf("script must terminate on failure: %s", out)
				}
				return
			}
			if !strings.Contains(string(out), "status="+tc.wantStatus) {
				t.Fatalf("want status=%s, output: %s", tc.wantStatus, out)
			}
		})
	}
}
