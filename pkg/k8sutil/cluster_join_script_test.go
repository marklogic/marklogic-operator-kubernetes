package k8sutil

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestClusterJoinScript(t *testing.T) {
	for _, tool := range []string{"bash"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("script integration test requires %s", tool)
		}
	}
	helper, err := filepath.Abs("scripts/cluster-join.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, state        string
		fail, tls          bool
		installs, restarts int
		downloads          int // expected bootstrap cluster-config requests; -1 skips the check
	}{
		{"partial-join", "standalone", false, false, 1, 1, -1},
		{"joined", "joined", false, false, 0, 0, -1},
		{"bootstrap-unavailable", "joined", true, false, 0, 0, -1},
		{"download-fails", "standalone", true, false, 0, 0, 2},
		{"unauthorized", "standalone", true, false, 0, 0, 1},
		{"forbidden", "standalone", true, false, 0, 0, 1},
		{"transport-fails", "standalone", true, false, 0, 0, -1},
		{"invalid-zip", "standalone", true, false, 0, 0, -1},
		{"install-fails", "standalone", true, false, 2, 0, -1},
		{"install-not-committed", "standalone", true, false, 1, 1, -1},
		{"different-cluster", "foreign", true, false, 0, 0, -1},
		{"invalid-local-xml", "invalid", true, false, 0, 0, -1},
		{"group-creation-race", "standalone", false, false, 1, 1, -1},
		{"tls-partial-join", "standalone", false, true, 1, 1, -1},
		{"tls-joined", "joined", false, true, 0, 0, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			self := `<host><host-id>200</host-id><host-name>joiner.example</host-name></host>`
			bootstrap := `<host><host-id>100</host-id><host-name>bootstrap.example</host-name></host>`
			body := self
			if tc.state == "joined" {
				body += bootstrap
			}
			if tc.state == "foreign" {
				body += `<host><host-id>300</host-id><host-name>other.example</host-name></host>`
			}
			xml := `<hosts xmlns="http://marklogic.com/xdmp/hosts">` + body + `</hosts>`
			if tc.state == "invalid" {
				xml = "not XML"
			}
			if err := os.WriteFile(filepath.Join(dir, "hosts.xml"), []byte(xml), 0600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("bash", "-c", joinScriptHarness)
			cmd.Env = append(os.Environ(), "HELPER="+helper, "CASE="+tc.name, "TEST_DIR="+dir)
			if tc.tls {
				cmd.Env = append(cmd.Env, "TEST_PROTOCOL=https", "TEST_HTTPS_OPTION=-k")
			}
			out, runErr := cmd.CombinedOutput()
			if (runErr != nil) != tc.fail {
				t.Fatalf("error=%v; want failure=%v\n%s", runErr, tc.fail, out)
			}
			trace, _ := os.ReadFile(filepath.Join(dir, "trace"))
			if n := strings.Count(string(trace), "INSTALL\n"); n != tc.installs {
				t.Fatalf("installs=%d want %d; %s", n, tc.installs, trace)
			}
			if n := strings.Count(string(trace), "RESTART\n"); n != tc.restarts {
				t.Fatalf("restarts=%d want %d; %s", n, tc.restarts, trace)
			}
			if tc.downloads >= 0 {
				if n := strings.Count(string(trace), "bootstrap.example:8001/admin/v1/cluster-config\n"); n != tc.downloads {
					t.Fatalf("bootstrap cluster-config requests=%d want %d; %s", n, tc.downloads, trace)
				}
			}
			if tc.tls && strings.Contains(string(trace), "http://bootstrap.example") {
				t.Fatalf("TLS join used plaintext to reach bootstrap: %s", trace)
			}
			if tc.fail && strings.Contains(string(out), "Cluster join completed") {
				t.Fatalf("failure reported success: %s", out)
			}
		})
	}
}

const joinScriptHarness = `
source "$HELPER"
N_RETRY=2
RETRY_INTERVAL=0
MARKLOGIC_DATA_DIR=$TEST_DIR
MARKLOGIC_BOOTSTRAP_HOST=bootstrap.example
MARKLOGIC_GROUP=dnode
MARKLOGIC_ADMIN_USERNAME=test
MARKLOGIC_ADMIN_PASSWORD=test
HTTP_PROTOCOL=${TEST_PROTOCOL:-http}
HTTPS_OPTION=${TEST_HTTPS_OPTION:-}
info() { echo "$*"; }
error() { echo "$*" >&2; }
restart_check() { echo RESTART >> "$TEST_DIR/trace"; }
curl() {
    local output url code=200 body='<ok/>'
    while (($#)); do
        case "$1" in
            -o) output=$2; shift 2 ;;
            --connect-timeout|--max-time|-w|--user|-H|-X|--data-urlencode|--data-binary) shift 2 ;;
            http://*|https://*) url=$1; shift ;;
            *) shift ;;
        esac
    done
    echo "$url" >> "$TEST_DIR/trace"
    case "$url" in
        *bootstrap.example:8001/admin/v1/server-config)
            body='<host xmlns="http://marklogic.com/manage"><host-id>100</host-id></host>' ;;
        */manage/v2/hosts/*)
            [[ "$CASE" == bootstrap-unavailable ]] && code=503 ;;
        */manage/v2/groups/*)
            if [[ "$CASE" == group-creation-race && ! -f "$TEST_DIR/group-seen" ]]; then
                touch "$TEST_DIR/group-seen"; code=404
            fi ;;
        *localhost:8001/admin/v1/server-config)
            body='<host><host-id>200</host-id><host-name>joiner.example</host-name></host>' ;;
        *bootstrap.example:8001/admin/v1/cluster-config)
            case "$CASE" in
                download-fails) code=500 ;;
                unauthorized) code=401 ;;
                forbidden) code=403 ;;
                transport-fails) printf 000; return 28 ;;
                invalid-zip) body='<error/>' ;;
                *) printf 'PK\003\004configuration' > "$output"; printf 200; return 0 ;;
            esac ;;
        *localhost:8001/admin/v1/timestamp) body=before ;;
        *localhost:8001/admin/v1/cluster-config)
            echo INSTALL >> "$TEST_DIR/trace"
            code=202
            if [[ "$CASE" == install-fails ]]; then
                code=500
            elif [[ "$CASE" != install-not-committed ]]; then
                echo '<hosts xmlns="http://marklogic.com/xdmp/hosts"><host><host-id>200</host-id><host-name>joiner.example</host-name></host><host><host-id>100</host-id><host-name>bootstrap.example</host-name></host></hosts>' > "$TEST_DIR/hosts.xml"
            fi ;;
        *) echo "Unexpected URL: $url" >&2; return 99 ;;
    esac
    printf '%s' "$body" > "$output"
    printf '%s' "$code"
}
join_cluster_checked joiner.example
`
