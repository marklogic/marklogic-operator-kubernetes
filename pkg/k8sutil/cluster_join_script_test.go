package k8sutil

import (
	"archive/zip"
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
		{"truncated-local-xml", "truncated", true, false, 0, 0, -1},
		{"group-creation-race", "standalone", false, false, 1, 1, -1},
		{"tls-partial-join", "standalone", false, true, 1, 1, -1},
		{"tls-joined", "joined", false, true, 0, 0, -1},
		{"truncated-zip", "standalone", true, false, 0, 0, -1},
		{"corrupt-zip", "standalone", true, false, 0, 0, -1},
		{"incomplete-host-record", "incomplete", true, false, 0, 0, -1},
		// The MarkLogic server image has python3 but no unzip.
		{"partial-join-python3", "standalone", false, false, 1, 1, -1},
		{"truncated-zip-python3", "standalone", true, false, 0, 0, -1},
		{"corrupt-zip-python3", "standalone", true, false, 0, 0, -1},
		{"no-zip-validator", "standalone", true, false, 0, 0, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			scenario := strings.TrimSuffix(tc.name, "-python3")
			var zipTools []string // nil keeps the host PATH
			switch {
			case strings.HasSuffix(tc.name, "-python3"):
				zipTools = []string{"python3"}
			case tc.name == "no-zip-validator":
				scenario = "partial-join"
				zipTools = []string{}
			}
			zipFile, err := os.Create(filepath.Join(dir, "cluster.zip"))
			if err != nil {
				t.Fatal(err)
			}
			zipWriter := zip.NewWriter(zipFile)
			zipEntry, err := zipWriter.Create("configuration.xml")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := zipEntry.Write([]byte("<configuration/>")); err != nil {
				t.Fatal(err)
			}
			if err := zipWriter.Close(); err != nil {
				t.Fatal(err)
			}
			if err := zipFile.Close(); err != nil {
				t.Fatal(err)
			}
			if scenario == "corrupt-zip" {
				archive, err := os.ReadFile(filepath.Join(dir, "cluster.zip"))
				if err != nil {
					t.Fatal(err)
				}
				archive[30+len("configuration.xml")] ^= 0xff
				if err := os.WriteFile(filepath.Join(dir, "cluster.zip"), archive, 0600); err != nil {
					t.Fatal(err)
				}
			}
			self := realHostRecord("200", "joiner.example")
			bootstrap := realHostRecord("100", "bootstrap.example")
			body := self
			if tc.state == "joined" {
				body += bootstrap
			}
			if tc.state == "foreign" {
				body += realHostRecord("300", "other.example")
			}
			xml := realHostsDocument(body)
			if err := os.WriteFile(filepath.Join(dir, "hosts-joined.xml"),
				[]byte(realHostsDocument(self+bootstrap)), 0600); err != nil {
				t.Fatal(err)
			}
			if tc.state == "invalid" {
				xml = "not XML"
			}
			if tc.state == "truncated" {
				xml = `<hosts xmlns="http://marklogic.com/xdmp/hosts">` + self +
					`<host><host-id>100`
			}
			if tc.state == "incomplete" {
				xml = `<hosts xmlns="http://marklogic.com/xdmp/hosts">` + self +
					`<host><host-id>100</host-id></host></hosts>`
			}
			if err := os.WriteFile(filepath.Join(dir, "hosts.xml"), []byte(xml), 0600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("bash", "-c", joinScriptHarness)
			cmd.Env = append(os.Environ(), "HELPER="+helper, "CASE="+scenario, "TEST_DIR="+dir,
				"ZIP_FIXTURE="+filepath.Join(dir, "cluster.zip"))
			if zipTools != nil {
				cmd.Env = append(cmd.Env, "PATH="+restrictedPath(t, zipTools...))
			}
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
                truncated-zip) printf 'PK\003\004configuration' > "$output"; printf 200; return 0 ;;
                *) cat "$ZIP_FIXTURE" > "$output"; printf 200; return 0 ;;
            esac ;;
        *localhost:8001/admin/v1/timestamp) body=before ;;
        *localhost:8001/admin/v1/cluster-config)
            echo INSTALL >> "$TEST_DIR/trace"
            code=202
            if [[ "$CASE" == install-fails ]]; then
                code=500
            elif [[ "$CASE" != install-not-committed ]]; then
                cat "$TEST_DIR/hosts-joined.xml" > "$TEST_DIR/hosts.xml"
            fi ;;
        *) echo "Unexpected URL: $url" >&2; return 99 ;;
    esac
    printf '%s' "$body" > "$output"
    printf '%s' "$code"
}
join_cluster_checked joiner.example
`

// realHostsDocument and realHostRecord mirror the layout of hosts.xml written
// by MarkLogic 12: an XML declaration, root attributes, indentation, extra
// host children, self-closing elements and a multi-line certificate.
func realHostsDocument(hosts string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>` + "\n" +
		`<hosts xsi:schemaLocation="http://marklogic.com/xdmp/hosts hosts.xsd" xml:lang="zxx" ` +
		`timestamp="17951564878147570" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" ` +
		`xmlns="http://marklogic.com/xdmp/hosts">` + "\n" + hosts + "</hosts>\n"
}

func realHostRecord(id, name string) string {
	return "  <host>\n" +
		"    <host-id>" + id + "</host-id>\n" +
		"    <host-name>" + name + "</host-name>\n" +
		"    <host-mode>normal</host-mode>\n" +
		"    <host-mode-description/>\n" +
		"    <group>4248465848067994579</group>\n" +
		"    <bind-port>7999</bind-port>\n" +
		"    <connect-port>7999</connect-port>\n" +
		"    <ssl-certificate>-----BEGIN CERTIFICATE-----\n" +
		"MIICzjCCAbagAwIBAgIJANeviKtsLGemMA0GCSqGSIb3DQEBCwUAMB8xHTAbBgNV\n" +
		"-----END CERTIFICATE-----\n" +
		"</ssl-certificate>\n" +
		"    <ssl-certificate-old/>\n" +
		"    <ssl-certificate-new/>\n" +
		"    <zone/>\n" +
		"  </host>\n"
}

// restrictedPath returns a PATH holding only the tools the join script and
// harness need, plus extra, so tests can simulate an image without unzip.
func restrictedPath(t *testing.T, extra ...string) string {
	t.Helper()
	dir := t.TempDir()
	tools := append([]string{"bash", "cat", "grep", "head", "mktemp", "paste", "rm", "sed",
		"sleep", "touch", "tr", "wc"}, extra...)
	for _, tool := range tools {
		path, err := exec.LookPath(tool)
		if err != nil {
			t.Skipf("test requires %s", tool)
		}
		if err := os.Symlink(path, filepath.Join(dir, tool)); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}
