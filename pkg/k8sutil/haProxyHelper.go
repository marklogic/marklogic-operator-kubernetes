package k8sutil

import (
	"bytes"
	"text/template"

	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
)

type HAProxyTemplateData struct {
	TargetPortNumber int
	PortNumber       int
	PortName         string
	Path             string
	PodName          string
	Index            int
	ServiceName      string
	NSName           string
	ClusterName      string
	SslCert          string
	sslEnabledServer bool
}

// generates frontend config for HAProxy depending on pathBasedRouting flag
// if pathBasedRouting is disabled, it will generate a frontend for each appServer
// otherwise, it will generate a single frontend with path based routing
func generateFrontendConfig(cr *marklogicv1.MarklogicCluster) string {

	var frontEndDef string
	var data *HAProxyTemplateData
	var result string
	pathBasedRouting := cr.Spec.HAProxy.PathBasedRouting
	appServers := cr.Spec.HAProxy.AppServers
	if *pathBasedRouting {
		frontEndDef = `
frontend marklogic-pathbased-frontend
  mode http
  option httplog
  bind :{{ .PortNumber}} {{ .SslCert }}
  http-request set-header Host marklogic:{{ .PortNumber}}
  http-request set-header REFERER http://marklogic:{{ .PortNumber}}`
		data = &HAProxyTemplateData{
			PortNumber: int(cr.Spec.HAProxy.FrontendPort),
			SslCert:    getSSLConfig(cr.Spec.HAProxy.Tls),
		}
		result = parseTemplateToString(frontEndDef, data)
		for _, appServer := range appServers {
			data = &HAProxyTemplateData{
				PortNumber:       int(appServer.Port),
				TargetPortNumber: int(appServer.TargetPort),
				Path:             appServer.Path,
			}
			result += getFrontendForPathbased(data)
		}
	} else {
		frontEndDef = `
frontend marklogic-{{ .PortNumber}}
  mode http
  bind :{{ .PortNumber }} {{ .SslCert }}
  log-format "%ci:%cp [%tr] %ft %b/%s %TR/%Tw/%Tc/%Tr/%Ta %ST %B %CC %CS %tsc %ac/%fc/%bc/%sc/%rc %sq/%bq %hr %hs %{+Q}r"
  default_backend marklogic-{{ .PortNumber}}-backend`

		for _, appServer := range appServers {
			data = &HAProxyTemplateData{
				PortNumber: int(appServer.Port),
				SslCert:    getSSLConfig(cr.Spec.HAProxy.Tls),
			}
			result += parseTemplateToString(frontEndDef, data) + "\n"
		}
	}
	return result
}

// generates backend config for HAProxy depending on pathBasedRouting flag and appServers
func generateBackendConfig(cr *marklogicv1.MarklogicCluster) string {

	pathBasedRouting := cr.Spec.HAProxy.PathBasedRouting
	var result string

	backendTemplate := `
backend marklogic-{{ .PortNumber}}-backend
  mode http
  balance leastconn
  option forwardfor
  cookie haproxy insert indirect nocache maxidle 30m maxlife 4h
  stick-table type string len 32 size 10k expire 4h
  stick store-response res.cook(HostId)
  stick store-response res.cook(SessionId)
  stick match req.cook(HostId)
  stick match req.cook(SessionId)
  default-server check`

	if *pathBasedRouting {
		backendTemplate += `
  http-request replace-path {{.Path}}(/)?(.*) /\2`
	}
	groups := cr.Spec.MarkLogicGroups

	appServers := cr.Spec.HAProxy.AppServers

	for _, appServer := range appServers {
		data := &HAProxyTemplateData{
			PortNumber: int(appServer.Port),
			Path:       appServer.Path,
		}
		result += parseTemplateToString(backendTemplate, data)
		for _, group := range groups {
			name := group.Name
			groupReplicas := int(*group.Replicas)
			if group.HAProxy != nil && !group.HAProxy.Enabled {
				continue
			}
			for i := 0; i < groupReplicas; i++ {
				data := &HAProxyTemplateData{
					PortNumber:       int(appServer.Port),
					PodName:          name,
					Path:             appServer.Path,
					Index:            i,
					ServiceName:      name,
					NSName:           cr.ObjectMeta.Namespace,
					ClusterName:      cr.Spec.ClusterDomain,
					sslEnabledServer: cr.Spec.Tls != nil && cr.Spec.Tls.EnableOnDefaultAppServers,
				}
				result += getBackendServerConfigs(data)
			}
		}
		result += "\n"
	}

	return result
}

func getBackendServerConfigs(data *HAProxyTemplateData) string {
	backend := `
  server {{.PodName}}-{{.PortNumber}}-{{.Index}} {{.PodName}}-{{.Index}}.{{.ServiceName}}.{{.NSName}}.svc.{{.ClusterName}}:{{.PortNumber}} resolvers dns init-addr none cookie {{.PodName}}-{{.PortNumber}}-{{.Index}}`
	if data.sslEnabledServer {
		backend += " ssl verify none"
	}

	return parseTemplateToString(backend, data)
}

func getFrontendForPathbased(data *HAProxyTemplateData) string {
	frontend := `
  use_backend marklogic-{{.PortNumber}}-backend if { path {{.Path}} } || { path_beg {{.Path}}/ }`
	if data.PortNumber == 8000 || data.TargetPortNumber == 8000 {
		frontend += `
  http-request set-header X-ML-QC-Path {{.Path}}`
	} else if data.PortNumber == 8001 || data.TargetPortNumber == 8001 {
		frontend += `
  http-request set-header X-ML-ADM-Path {{.Path}}`
	} else if data.PortNumber == 8002 || data.TargetPortNumber == 8002 {
		frontend += `
  http-request set-header X-ML-MNG-Path {{.Path}}`
	}
	return parseTemplateToString(frontend, data)
}

func getBackendForTCP(data *HAProxyTemplateData) string {
	backend := `
server ml-{{.PodName}}-{{.PortNumber}}-{{.Index}} {{.PodName}}-{{.Index}}.{{.ServiceName}}.{{.NSName}}.svc.{{.ClusterName}}:{{.PortNumber}} check resolvers dns init-addr none`
	return parseTemplateToString(backend, data)
}

// generates the stats config for HAProxy
func generateStatsConfig(cr *marklogicv1.MarklogicCluster) string {
	statsDef := `
frontend stats
  mode http
  bind *:{{ .StatsPort }} {{ .SslCert }}
  stats enable
  http-request use-service prometheus-exporter if { path /metrics }
  stats uri /
  stats refresh 10s
  stats admin if LOCALHOST
`
	data := map[string]interface{}{
		"StatsPort": cr.Spec.HAProxy.Stats.Port,
		"SslCert":   getSSLConfig(cr.Spec.HAProxy.Tls),
	}
	if cr.Spec.HAProxy.Stats.Auth.Enabled {
		statsDef += `  stats auth {{ .StatsUsername }}:{{ .StatsPassword }}
`
		data["StatsUsername"] = cr.Spec.HAProxy.Stats.Auth.Username
		data["StatsPassword"] = cr.Spec.HAProxy.Stats.Auth.Password
	}

	return parseTemplateToString(statsDef, data)
}

// generates the tcp config for HAProxy
func generateTcpConfig(cr *marklogicv1.MarklogicCluster) string {
	result := ""

	for _, tcpPort := range cr.Spec.HAProxy.TcpPorts.Ports {
		t := `
listen marklogic-TCP-{{.PortNumber}}
  bind :{{ .PortNumber }} {{ .SslCert }}
  mode tcp
  balance leastconn`
		data := &HAProxyTemplateData{
			PortNumber: int(tcpPort.Port),
			SslCert:    getSSLConfig(cr.Spec.HAProxy.Tls),
		}
		result += parseTemplateToString(t, data)
		for _, group := range cr.Spec.MarkLogicGroups {
			name := group.Name
			groupReplicas := int(*group.Replicas)
			if group.HAProxy != nil && !group.HAProxy.Enabled {
				continue
			}
			for i := 0; i < groupReplicas; i++ {
				data := &HAProxyTemplateData{
					PortNumber:  int(tcpPort.Port),
					PodName:     name,
					Index:       i,
					ServiceName: name,
					NSName:      cr.ObjectMeta.Namespace,
					ClusterName: cr.Spec.ClusterDomain,
				}
				result += getBackendForTCP(data)
			}
		}
	}

	return result
}

func getSSLConfig(tls *marklogicv1.TlsForHAProxy) string {
	if tls == nil || !tls.Enabled {
		return ""
	} else {
		return "ssl crt /usr/local/etc/ssl/" + tls.CertFileName
	}
}

// parses the given template with the given data
func parseTemplateToString(templateStr string, data interface{}) string {
	t := template.Must(template.New("haproxyConfig").Parse(templateStr))
	buf := bytes.NewBufferString("")
	err := t.Execute(buf, data)
	if err != nil {
		panic(err)
	}
	return buf.String()
}

type Servers []marklogicv1.AppServers

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
