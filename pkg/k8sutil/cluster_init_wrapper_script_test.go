package k8sutil

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestInitWrapperWatchdogReportsActualOutage(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("script test requires bash")
	}
	data, err := os.ReadFile("scripts/cluster-init-wrapper.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)
	start := strings.Index(script, "PORT_DOWN_COUNT=0\nPORT_DOWN_THRESHOLD=")
	if start < 0 {
		t.Fatal("watchdog section not found in cluster-init-wrapper.sh")
	}
	watchdog := script[start:]

	// Each probe result is "<exit status>:<seconds the probe took>".
	for _, tc := range []struct {
		name   string
		probes string
		want   []string
		absent []string
	}{
		{
			name:   "connection-refused",
			probes: "1:0 1:0 1:0 1:0 1:0 1:0",
			want:   []string{"failed 6 consecutive checks over 25s"},
		},
		{
			name:   "connection-accepted-but-hangs",
			probes: "1:6 1:6 1:6 1:6 1:6 1:6",
			want:   []string{"failed 6 consecutive checks over 61s"},
			absent: []string{"over 25s", "over 30s"},
		},
		{
			name:   "intermittent-recovery-resets-count-and-timer",
			probes: "1:0 1:0 0:0 1:0 1:0 1:0 1:0 1:0 1:0",
			want:   []string{"Port 8001 recovered after 2 down checks", "failed 6 consecutive checks over 25s"},
			absent: []string{"count 7/6"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			harness := `
PROBES=($PROBE_SEQUENCE)
PROBE_INDEX=0
SCRIPT_PID=$$
PID_FILE=/nonexistent
REAL_ML_PID=1
sleep() { SECONDS=$((SECONDS + $1)); }
kill() { return 0; }
ml_port_open() {
    local probe=${PROBES[PROBE_INDEX]:-1:0}
    PROBE_INDEX=$((PROBE_INDEX + 1))
    SECONDS=$((SECONDS + ${probe#*:}))
    return "${probe%%:*}"
}
SECONDS=0
` + watchdog
			cmd := exec.Command("bash", "-c", harness)
			cmd.Env = append(os.Environ(), "PROBE_SEQUENCE="+tc.probes)
			out, runErr := cmd.CombinedOutput()
			if runErr == nil {
				t.Fatalf("watchdog must exit non-zero after declaring a crash: %s", out)
			}
			for _, want := range tc.want {
				if !strings.Contains(string(out), want) {
					t.Fatalf("output missing %q:\n%s", want, out)
				}
			}
			for _, absent := range tc.absent {
				if strings.Contains(string(out), absent) {
					t.Fatalf("output must not contain %q:\n%s", absent, out)
				}
			}
			if n := strings.Count(string(out), "Declaring crash"); n != 1 {
				t.Fatalf("crash declared %d times, want 1:\n%s", n, out)
			}
		})
	}
}
