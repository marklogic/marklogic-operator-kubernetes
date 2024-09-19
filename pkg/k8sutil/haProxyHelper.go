package k8sutil

import (
	"bytes"
	"strings"
	"text/template"

	"github.com/marklogic/marklogic-kubernetes-operator/api/v1alpha1"
	databasev1alpha1 "github.com/marklogic/marklogic-kubernetes-operator/api/v1alpha1"
)

// generates frontend config for HAProxy depending on pathBasedRouting flag
// if pathBasedRouting is disabled, it will generate a frontend for each appServer
// otherwise, it will generate a single frontend with path based routing
func generateFrontendConfig(grpCR *databasev1alpha1.MarklogicGroup) string {

	var frontEndDef string
	var data map[string]interface{}
	var result string
	pathBasedRouting := grpCR.Spec.HAProxyConfig.PathBasedRouting
	allAppServers := append(grpCR.Spec.HAProxyConfig.DefaultAppServers, grpCR.Spec.HAProxyConfig.AdditionalAppServers...)
	if pathBasedRouting {
		frontEndDef = `
frontend marklogic-{{ $.ClusterOrGroup}}
  mode http
  option httplog
  bind :{{ $.FrontendPort}}
  http-request set-header Host marklogic:{{ $.FrontendPort}}
  http-request set-header REFERER http://marklogic:{{ $.FrontendPort}}
  http-request set-header X-ML-QC-Path {{ index .DefaultAppServersPath 0}}
  http-request set-header X-ML-ADM-Path {{ index .DefaultAppServersPath 1}}
  http-request set-header X-ML-MNG-Path {{ index .DefaultAppServersPath 2}}
  {{ range $appServer := .AllAppServers }}
  use_backend marklogic-{{ $.ClusterOrGroup}}-{{ $appServer.Port }} if { path {{ $appServer.Path }} } || { path_beg {{ $appServer.Path }}/ }
  {{ end }}`

		data = map[string]interface{}{
			"AllAppServers":         allAppServers,
			"DefaultAppServersPath": getPathList(grpCR.Spec.HAProxyConfig.DefaultAppServers),
			"FrontendPort":          80,
			"ClusterOrGroup":        grpCR.Spec.GroupConfig.Name,
		}
		result = parseConfigDef(frontEndDef, data) + "\n"

	} else {

		frontEndDef = `
frontend marklogic-{{ $.ClusterOrGroup}}-{{ $.Port}}
  mode http
  {{- if $.HaproxyTlsEnabled }}
  bind :{{ $.Port }} ssl crt /usr/local/etc/ssl/{{ $.CertFileName }}
  {{- else }}
  bind :{{ $.Port }}
  {{- end }}
  log-format "%ci:%cp [%tr] %ft %b/%s %TR/%Tw/%Tc/%Tr/%Ta %ST %B %CC %CS %tsc %ac/%fc/%bc/%sc/%rc %sq/%bq %hr %hs %{+Q}r"
  default_backend marklogic-{{ $.ClusterOrGroup}}-{{ $.Port}}`

		for _, appServer := range allAppServers {
			data = map[string]interface{}{
				"Port":              appServer.Port,
				"HaproxyTlsEnabled": false,
				"ClusterOrGroup":    grpCR.Spec.GroupConfig.Name,
				"CertFileName":      "HAProxyConfig/CertFileName",
				"GroupName":         grpCR.Spec.Name,
			}
			result += parseConfigDef(frontEndDef, data) + "\n"
		}
	}

	return result
}

// generates backend config for HAProxy depending on pathBasedRouting flag and appServers
func generateBackendConfig(grpCR *databasev1alpha1.MarklogicGroup) string {

	pathBasedRouting := grpCR.Spec.HAProxyConfig.PathBasedRouting

	backEndDef := `
backend marklogic-{{.ClusterOrGroup}}-{{.Port}}
  mode http
  balance leastconn
  option forwardfor
  http-request replace-path {{.Path}}(/)?(.*) /\2
  cookie haproxy insert indirect nocache maxidle 30m maxlife 4h
  stick-table type string len 32 size 10k expire 4h
  stick store-response res.cook(HostId)
  stick store-response res.cook(SessionId)
  stick match req.cook(HostId)
  stick match req.cook(SessionId)
  default-server check
  {{ range $replica := .Replicas }}
  server ml-{{ $.GroupName}}-{{ $.Port}}-{{$replica}} {{ $.GroupName}}-{{$replica}}.{{ $.GroupName}}.default.svc.cluster.local:{{ $.Port}} resolvers dns init-addr none cookie {{ $.GroupName}}-{{ $.Port}}-{{$replica}}
  {{ end }}
`

	if !pathBasedRouting {
		rm := `http-request replace-path {{.Path}}(/)?(.*) /\2`
		backEndDef = strings.Replace(backEndDef, rm, "", -1)
		backEndDef = strings.TrimSpace(backEndDef)
	}

	allAppServers := append(grpCR.Spec.HAProxyConfig.DefaultAppServers, grpCR.Spec.HAProxyConfig.AdditionalAppServers...)
	replicas := generateReplicaArray(int(*grpCR.Spec.Replicas))
	var result string
	var data map[string]interface{}

	for _, appServer := range allAppServers {
		data = map[string]interface{}{
			"Path":           appServer.Path,
			"Port":           appServer.Port,
			"GroupName":      grpCR.Spec.Name,
			"ClusterOrGroup": grpCR.Spec.GroupConfig.Name,
			"Replicas":       replicas,
		}
		result += parseConfigDef(backEndDef, data) + "\n"
	}

	return result
}

// generates the stats config for HAProxy
func generateStatsConfig(grpCR *databasev1alpha1.MarklogicGroup) string {
	statsDef := `
frontend stats
  mode http
  bind *:{{ $.StatsPort }}
  stats enable
  http-request use-service prometheus-exporter if { path /metrics }
  stats uri /
  {{- if $.StatsAuth }}
  stats auth {{ $.StatsUsername }}:{{ $.StatsPassword }}
  {{- end }}
  stats refresh 10s
  stats admin if LOCALHOST`

	data := map[string]interface{}{
		"StatsPort":     grpCR.Spec.HAProxyConfig.Stats.Port,
		"StatsAuth":     grpCR.Spec.HAProxyConfig.Stats.Auth.Enabled,
		"StatsUsername": grpCR.Spec.HAProxyConfig.Stats.Auth.Username,
		"StatsPassword": grpCR.Spec.HAProxyConfig.Stats.Auth.Password,
	}
	return parseConfigDef(statsDef, data)
}

// generates the tcp config for HAProxy
func generateTcpConfig(grpCR *databasev1alpha1.MarklogicGroup) string {

	replicas := generateReplicaArray(int(*grpCR.Spec.Replicas))
	tcpDef := `
  {{- range $tcpPort := .Ports }}
  listen marklogic-TCP-{{$tcpPort.Port}}
  bind :{{ $tcpPort.Port }}
  mode tcp
  balance leastconn
  {{ range $replica := $.Replicas }}
  server {{ printf "ml-%s-%v-%v" $.GroupName $tcpPort.Port $replica }} {{ $.GroupName }}-{{ $replica }}.{{ $.HeadlessServiceName }}.{{ $.Namespace }}.svc.{{ $.ClusterDomain }}:{{ $tcpPort.Port }} check resolvers dns init-addr none
  {{- end }}
  {{- end }}
`
	data := map[string]interface{}{
		"Ports":               grpCR.Spec.HAProxyConfig.TcpPorts.Ports,
		"Replicas":            replicas,
		"GroupName":           grpCR.Spec.Name,
		"HeadlessServiceName": grpCR.Spec.Name,
		"Namespace":           "default",
		"ClusterDomain":       "cluster.local",
	}
	return parseConfigDef(tcpDef, data)
}

// parses the given template with the given data
func parseConfigDef(configDef string, data map[string]interface{}) string {
	templ := template.Must(template.New("name").Parse(configDef))
	newBuffer := bytes.NewBufferString("")
	err := templ.Execute(newBuffer, data)
	if err != nil {
		panic(err)
	}
	return newBuffer.String()
}

type Servers []v1alpha1.AppServers

func getPathList(servers Servers) []string {
	var paths []string
	for _, server := range servers {
		paths = append(paths, server.Path)
	}
	return paths
}

// returns a array of replica numbers from 0 to replicas-1
// used for looping over replicas in haproxy config
func generateReplicaArray(replicas int) []int {
	Replicas := []int{}
	for i := 0; i < replicas; i++ {
		Replicas = append(Replicas, i)
	}
	return Replicas
}
