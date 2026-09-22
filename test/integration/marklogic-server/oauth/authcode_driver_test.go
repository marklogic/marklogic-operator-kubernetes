// Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package oauth

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tlsfixture "github.com/marklogic/marklogic-operator-kubernetes/test/integration/marklogic-server/fixtures/tls"
)

const driverSecret = "never-log-this-session"
const driverState = "never-log-this-state"
const driverCode = "never-log-this-code"

type driverScenario struct {
	callbackStatus                                                                     int
	callbackBody                                                                       string
	wrongIdentity, restart, noCode, startOnly, negative                                bool
	failTLSAt                                                                          int32
	badCertificate                                                                     tls.Certificate
	startStatus                                                                        int
	missingHTTPOnly, missingPath, omittedPath, missingBackend, sameBackend, lostCookie bool
}

// Exercise the real shell and curl against a local HTTPS protocol fixture. This
// validates the driver, not MarkLogic's OAuth implementation or cloud affinity.
func exerciseAuthCodeDriver(t *testing.T, scenario driverScenario) (map[string]string, error) {
	t.Helper()
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl is required for local Authorization Code driver tests")
	}
	var server *httptest.Server
	var requests, handshakes atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		backend := "node-0"
		mode := "affinity"
		if scenario.negative {
			mode = "no-affinity"
		}
		if r.URL.Path == "/oauth/callback" && scenario.negative && !scenario.sameBackend {
			backend = "node-1"
		}
		if !scenario.missingBackend {
			w.Header().Set("X-Test-Backend", backend)
		}
		w.Header().Set("X-Test-Affinity", mode)
		w.Header().Set("X-Test-Session-Hash", fmt.Sprintf("%X", sha256.Sum256([]byte(driverSecret))))
		if cookie, err := r.Cookie("SessionID"); err == nil && !scenario.lostCookie {
			w.Header().Set("X-Test-Received-Session-Hash", fmt.Sprintf("%X", sha256.Sum256([]byte(cookie.Value))))
		}
		switch r.URL.Path {
		case "/identity.xqy":
			cookie, err := r.Cookie("authenticated")
			if err == nil && cookie.Value == driverSecret {
				identity := "alice"
				if scenario.wrongIdentity {
					identity = "bob"
				}
				fmt.Fprint(w, "oauth-user:"+identity)
				return
			}
			cookiePath := "/"
			if scenario.missingPath {
				cookiePath = "/wrong"
			}
			if scenario.omittedPath {
				cookiePath = ""
			}
			http.SetCookie(w, &http.Cookie{Name: "SessionID", Value: driverSecret, Path: cookiePath, HttpOnly: !scenario.missingHTTPOnly, Secure: true})
			status := scenario.startStatus
			if status == 0 {
				status = http.StatusFound
			}
			http.Redirect(w, r, server.URL+"/realms/test/protocol/openid-connect/auth?response_type=code&code_challenge=challenge&code_challenge_method=S256&state="+driverState, status)
		case "/realms/test/protocol/openid-connect/auth":
			http.SetCookie(w, &http.Cookie{Name: "KEYCLOAK_SESSION", Value: driverSecret, Path: "/", HttpOnly: true, Secure: true})
			fmt.Fprintf(w, `<form action="%s/realms/test/login-actions/authenticate?session_code=private&amp;execution=login"></form>`, server.URL)
		case "/realms/test/login-actions/authenticate":
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			if r.Form.Get("username") != "alice" || r.Form.Get("password") != "password" {
				t.Error("driver did not submit expected credentials")
			}
			if _, err := r.Cookie("KEYCLOAK_SESSION"); err != nil {
				t.Error("driver lost Keycloak cookie")
			}
			code := driverCode
			if scenario.noCode {
				code = ""
			}
			fmt.Fprintf(w, `<FORM ACTION="%s/oauth/callback"><INPUT NAME="code" VALUE="%s"/><INPUT NAME="state" VALUE="%s"/></FORM>`, server.URL, code, driverState)
		case "/oauth/callback":
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			if r.Form.Get("code") != driverCode || r.Form.Get("state") != driverState {
				t.Error("driver lost callback parameters")
			}
			_, err := r.Cookie("SessionID")
			if err != nil && !scenario.missingPath {
				t.Error("incorrect callback session cookie behavior")
			}
			if !scenario.negative {
				http.SetCookie(w, &http.Cookie{Name: "authenticated", Value: driverSecret, Path: "/", HttpOnly: true, Secure: true})
			}
			if scenario.restart {
				w.Header().Set("Location", server.URL+"/realms/test/protocol/openid-connect/auth?state="+driverState)
			} else if scenario.callbackStatus == 302 {
				w.Header().Set("Location", "/identity.xqy")
			}
			w.WriteHeader(scenario.callbackStatus)
			fmt.Fprint(w, scenario.callbackBody)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	})
	server = httptest.NewUnstartedServer(handler)
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.TLS = &tls.Config{GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
		if handshakes.Add(1) == scenario.failTLSAt {
			return &tls.Config{Certificates: []tls.Certificate{scenario.badCertificate}}, nil
		}
		return nil, nil
	}}
	server.StartTLS()
	defer server.Close()
	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	affinity := "enabled"
	if scenario.negative {
		affinity = "disabled"
	}
	mode := "full"
	if scenario.startOnly {
		mode = "start"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", authCodeFlowScript, "authcode-driver", server.URL+"/identity.xqy", "alice", "password", caPath, server.URL+"/realms/test/protocol/openid-connect/auth", server.URL+"/oauth/callback", mode, affinity)
	cmd.Env = append(os.Environ(), "NO_PROXY=127.0.0.1,localhost", "no_proxy=127.0.0.1,localhost", "TMPDIR="+dir)
	output, err := cmd.CombinedOutput()
	for _, secret := range []string{driverSecret, driverState, driverCode, "password"} {
		if strings.Contains(string(output), secret) {
			t.Fatalf("driver leaked authentication material")
		}
	}
	files, readErr := os.ReadDir(dir)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(files) != 1 || files[0].Name() != "ca.pem" {
		t.Error("driver left temporary session files behind")
	}
	if scenario.startOnly && requests.Load() != 1 {
		t.Errorf("pre-authentication test made %d requests, want 1", requests.Load())
	}
	return parseMarkers(string(output)), err
}

func TestAuthCodeDriver(t *testing.T) {
	for _, tc := range []struct {
		name     string
		scenario driverScenario
		valid    bool
	}{
		{"authenticated session", driverScenario{callbackStatus: 302}, true},
		{"callback 200", driverScenario{callbackStatus: 200}, true},
		{"forbidden callback", driverScenario{callbackStatus: 403}, false},
		{"wrong identity", driverScenario{callbackStatus: 302, wrongIdentity: true}, false},
		{"restarted authentication", driverScenario{callbackStatus: 302, restart: true}, false},
		{"missing authorization code", driverScenario{callbackStatus: 302, noCode: true}, false},
		{"cross-node state error", driverScenario{callbackStatus: 500, negative: true, callbackBody: "<dl><dt>XDMP-OAUTH: Novel OAuth state " + driverState + "; potential CSRF attack</dt></dl>"}, true},
		{"unrelated server failure", driverScenario{callbackStatus: 500, negative: true, callbackBody: "<dt>XDMP-INTERNAL: unrelated failure</dt>"}, false},
		{"error text outside error field", driverScenario{callbackStatus: 500, negative: true, callbackBody: "XDMP-OAUTH: Novel OAuth state; potential CSRF attack"}, false},
		{"pre-authentication only", driverScenario{startOnly: true}, true},
		{"initial 303 redirect", driverScenario{startOnly: true, startStatus: 303}, true},
		{"initial 303 completes authentication", driverScenario{startStatus: 303, callbackStatus: 302}, true},
		{"initial 303 cross-node rejection", driverScenario{startStatus: 303, callbackStatus: 500, negative: true, callbackBody: "<dt>XDMP-OAUTH: Novel OAuth state; potential CSRF attack</dt>"}, true},
		{"initial 307 rejected", driverScenario{startOnly: true, startStatus: 307}, false},
		{"HttpOnly required", driverScenario{startOnly: true, missingHTTPOnly: true}, false},
		{"explicit cookie path required", driverScenario{startOnly: true, omittedPath: true}, false},
		{"root cookie path required", driverScenario{startOnly: true, missingPath: true}, false},
		{"observed backend required", driverScenario{startOnly: true, missingBackend: true}, false},
		{"negative same backend cannot pass", driverScenario{negative: true, sameBackend: true, callbackStatus: 500, callbackBody: "<dt>XDMP-OAUTH: Novel OAuth state; potential CSRF attack</dt>"}, false},
		{"negative lost cookie cannot pass", driverScenario{negative: true, lostCookie: true, callbackStatus: 500, callbackBody: "<dt>XDMP-OAUTH: Novel OAuth state; potential CSRF attack</dt>"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := exerciseAuthCodeDriver(t, tc.scenario)
			if err != nil {
				t.Fatalf("driver failed: %v", err)
			}
			if tc.scenario.startOnly {
				err = validateAuthCodeStart(result)
			} else if tc.scenario.negative {
				err = validateAuthCodeRejection(result)
			} else {
				err = validateAuthCodeSuccess(result)
			}
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v markers=%v error=%v", tc.valid, result, err)
			}
		})
	}
}

func TestAuthCodeDriverRejectsUntrustedTLSAtEveryStage(t *testing.T) {
	resources, err := tlsfixture.BuildResources(tlsfixture.Config{Namespace: "local", CASecretName: "ca", TLSSecretName: "untrusted", DNSNames: []string{"localhost"}})
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := tls.X509KeyPair(resources.TLSSecret.Data["tls.crt"], resources.TLSSecret.Data["tls.key"])
	if err != nil {
		t.Fatal(err)
	}
	for stage := int32(1); stage <= 5; stage++ {
		t.Run("stage_"+strconv.Itoa(int(stage)), func(t *testing.T) {
			result, err := exerciseAuthCodeDriver(t, driverScenario{callbackStatus: 302, failTLSAt: stage, badCertificate: certificate})
			if err == nil {
				t.Fatal("driver accepted an untrusted TLS connection")
			}
			if validateAuthCodeSuccess(result) == nil {
				t.Fatal("TLS failure passed success validation")
			}
			if validateAuthCodeRejection(result) == nil {
				t.Fatal("TLS failure passed negative-case validation")
			}
		})
	}
}
