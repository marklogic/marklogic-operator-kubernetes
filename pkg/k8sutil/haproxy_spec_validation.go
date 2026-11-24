// Copyright (c) 2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

package k8sutil

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
)

// The HAProxy configuration is generated with text/template, which does not escape, so every
// user-supplied string that reaches it must be a harmless token. These checks run before
// anything is rendered; a value that fails is reported with its JSON path and nothing is
// applied. The patterns are deliberately narrower than "what HAProxy might accept".
var (
	haproxyPathPattern       = regexp.MustCompile(`^/[A-Za-z0-9._~/-]{0,127}$`)
	dnsLabelPattern          = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	dnsSubdomainPattern      = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)
	secretKeyFilenamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,255}$`)
)

const maxHAProxyTimeoutSeconds = 2000000 // HAProxy stores timeouts in milliseconds in 32 bits

type specProblems []string

func (p *specProblems) add(path, format string, args ...any) {
	*p = append(*p, path+" "+fmt.Sprintf(format, args...))
}

func (p *specProblems) port(path string, value int32, allowZero bool) {
	if value < 0 || value > 65535 || (value == 0 && !allowZero) {
		p.add(path, "must be a port between %d and 65535 (got %d)", map[bool]int{true: 0, false: 1}[allowZero], value)
	}
}

func (p *specProblems) appServers(prefix string, servers []marklogicv1.AppServers) {
	for i, server := range servers {
		at := fmt.Sprintf("%s.appServers[%d]", prefix, i)
		p.port(at+".port", server.Port, false)
		p.port(at+".targetPort", server.TargetPort, true)
		if server.Path != "" && !haproxyPathPattern.MatchString(server.Path) {
			p.add(at+".path", "must start with / and use only A-Z a-z 0-9 . _ ~ / - (at most 128 characters)")
		}
	}
}

func (p *specProblems) tcpPorts(prefix string, tcp *marklogicv1.Tcpports) {
	if tcp == nil {
		return
	}
	for i, port := range tcp.Ports {
		at := fmt.Sprintf("%s.tcpPorts.ports[%d]", prefix, i)
		p.port(at+".port", port.Port, false)
		p.port(at+".targetPort", port.TargetPort, true)
	}
}

// validateHAProxySpec rejects values that would corrupt or inject into the generated HAProxy
// configuration, and reports every problem at once.
func validateHAProxySpec(cr *marklogicv1.MarklogicCluster) error {
	h := cr.Spec.HAProxy
	if h == nil {
		return nil
	}
	var p specProblems
	if cr.Spec.ClusterDomain != "" && (len(cr.Spec.ClusterDomain) > 253 || !dnsSubdomainPattern.MatchString(cr.Spec.ClusterDomain)) {
		p.add("spec.clusterDomain", "must be a DNS domain name such as cluster.local")
	}
	for i, group := range cr.Spec.MarkLogicGroups {
		if group == nil {
			continue
		}
		if !dnsLabelPattern.MatchString(group.Name) {
			p.add(fmt.Sprintf("spec.markLogicGroups[%d].name", i), "must be a DNS label (lower-case letters, digits and -, at most 63 characters)")
		}
		if group.HAProxy != nil {
			prefix := fmt.Sprintf("spec.markLogicGroups[%d].haproxy", i)
			p.appServers(prefix, group.HAProxy.AppServers)
			p.tcpPorts(prefix, group.HAProxy.TcpPorts)
		}
	}
	p.appServers("spec.haproxy", h.AppServers)
	p.tcpPorts("spec.haproxy", h.TcpPorts)
	p.port("spec.haproxy.frontendPort", h.FrontendPort, true)
	if h.Stats.Enabled {
		p.port("spec.haproxy.stats.port", h.Stats.Port, false)
	}
	for name, seconds := range map[string]int32{"client": h.Timeout.Client, "connect": h.Timeout.Connect, "server": h.Timeout.Server} {
		if seconds < 0 || seconds > maxHAProxyTimeoutSeconds {
			p.add("spec.haproxy.timeout."+name, "must be between 0 and %d seconds (got %d)", maxHAProxyTimeoutSeconds, seconds)
		}
	}
	if h.Tls != nil && h.Tls.Enabled {
		if len(h.Tls.SecretName) > 253 || !dnsSubdomainPattern.MatchString(h.Tls.SecretName) {
			p.add("spec.haproxy.tls.secretName", "must be a Secret name (a DNS subdomain)")
		}
		if name := h.Tls.CertFileName; !secretKeyFilenamePattern.MatchString(name) || name == "." || name == ".." {
			p.add("spec.haproxy.tls.certFileName", "must be a Secret key: 1-255 characters from A-Z a-z 0-9 . _ -")
		}
	}
	if err := validateHAProxyStatsAuth(cr); err != nil {
		p = append(p, err.Error())
	}
	if len(p) == 0 {
		return nil
	}
	// Map iteration order is random; sort so the message (and the status that carries it) is stable.
	slices.Sort(p)
	return fmt.Errorf("invalid HAProxy configuration: %s", strings.Join(p, "; "))
}
