package k8sutil

import (
	"bytes"
	"text/template"

	"github.com/marklogic/marklogic-kubernetes-operator/api/v1alpha1"
	databasev1alpha1 "github.com/marklogic/marklogic-kubernetes-operator/api/v1alpha1"
)

type HAProxyTemplateData struct {
	PortNumber  int
	PortName    string
	Path        string
	PodName     string
	Index       int
	ServiceName string
	NSName      string
	ClusterName string
}

// generates frontend config for HAProxy depending on pathBasedRouting flag
// if pathBasedRouting is disabled, it will generate a frontend for each appServer
// otherwise, it will generate a single frontend with path based routing
func generateFrontendConfig(cr *databasev1alpha1.MarklogicCluster) string {

	var frontEndDef string
	var data *HAProxyTemplateData
	var result string
	pathBasedRouting := cr.Spec.HAProxy.PathBasedRouting
	appServers := cr.Spec.HAProxy.AppServers
	if pathBasedRouting {
		frontEndDef = `
frontend marklogic-pathbased-frontend
	mode http
	option httplog
	bind :{{ $.PortNumber}}
	http-request set-header Host marklogic:{{ $.PortNumber}}
	http-request set-header REFERER http://marklogic:{{ $.PortNumber}}
	http-request set-header X-ML-QC-Path /console
	http-request set-header X-ML-ADM-Path /admin
	http-request set-header X-ML-MNG-Path /manage
`
		data = &HAProxyTemplateData{
			PortNumber: int(cr.Spec.HAProxy.FrontendPort),
		}
		result = parseTemplateToString(frontEndDef, data) + "\n"
		for _, appServer := range appServers {
			data = &HAProxyTemplateData{
				PortNumber: int(appServer.Port),
				Path:       appServer.Path,
			}
			result += getFrontendForPathbased(data)
		}
	} else {
		frontEndDef = `
frontend marklogic-{{ $.PortNumber}}
  mode http
  bind :{{ $.PortNumber }}
  log-format "%ci:%cp [%tr] %ft %b/%s %TR/%Tw/%Tc/%Tr/%Ta %ST %B %CC %CS %tsc %ac/%fc/%bc/%sc/%rc %sq/%bq %hr %hs %{+Q}r"
  default_backend marklogic-{{ $.PortNumber}}-backend`

		for _, appServer := range appServers {
			data = &HAProxyTemplateData{
				PortNumber: int(appServer.Port),
			}
			result += parseTemplateToString(frontEndDef, data) + "\n"
		}
	}

	return result
}

// generates backend config for HAProxy depending on pathBasedRouting flag and appServers
func generateBackendConfig(cr *databasev1alpha1.MarklogicCluster) string {

	pathBasedRouting := cr.Spec.HAProxy.PathBasedRouting
	var result string

	// http-request replace-path {{.Path}}(/)?(.*) /\2

	backendTemplate := `
backend marklogic-{{ $.PortNumber}}-backend
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

	if pathBasedRouting {
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
					PortNumber:  int(appServer.Port),
					PodName:     name,
					Path:        appServer.Path,
					Index:       i,
					ServiceName: name,
					NSName:      cr.ObjectMeta.Namespace,
					ClusterName: cr.Spec.ClusterDomain,
				}
				result += getBackendServerConfigs(data)
			}
		}
	}

	return result
}

func getBackendServerConfigs(data *HAProxyTemplateData) string {
	backend := `
    server {{.PodName}}-{{.PortNumber}}-{{.Index}} {{.PodName}}-{{.Index}}.{{.ServiceName}}.{{.NSName}}.svc.{{.ClusterName}}:{{.PortNumber}} resolvers dns init-addr none cookie {{.PodName}}-{{.PortNumber}}-{{.Index}}`
	return parseTemplateToString(backend, data)
}

func getFrontendForPathbased(data *HAProxyTemplateData) string {
	backend := `
	use_backend marklogic-{{.PortNumber}}-backend if { path {{.Path}} } || { path_beg {{.Path}}/ }`
	return parseTemplateToString(backend, data)
}

func getBackendForTCP(data *HAProxyTemplateData) string {
	backend := `
	  server ml-{{.PodName}}-{{.PortNumber}}-{{.Index}} {{.PodName}}-{{.Index}}.{{.ServiceName}}.{{.NSName}}.svc.{{.ClusterName}}:{{.PortNumber}} check resolvers dns init-addr none`
	return parseTemplateToString(backend, data)
}

// {{- if $.StatsAuth }}
// stats auth {{ $.StatsUsername }}:{{ $.StatsPassword }}
// {{- end }}
// "StatsAuth":     cr.Spec.HAProxy.Stats.Auth.Enabled,
// "StatsUsername": cr.Spec.HAProxy.Stats.Auth.Username,
// "StatsPassword": cr.Spec.HAProxy.Stats.Auth.Password,
// generates the stats config for HAProxy
func generateStatsConfig(cr *databasev1alpha1.MarklogicCluster) string {
	statsDef := `
frontend stats
  mode http
  bind *:{{ $.StatsPort }}
  stats enable
  http-request use-service prometheus-exporter if { path /metrics }
  stats uri /
  stats refresh 10s
  stats admin if LOCALHOST
`

	data := map[string]interface{}{
		"StatsPort": cr.Spec.HAProxy.Stats.Port,
	}
	return parseTemplateToString(statsDef, data)
}

// generates the tcp config for HAProxy
func generateTcpConfig(cr *databasev1alpha1.MarklogicCluster) string {
	result := ""

	for _, tcpPort := range cr.Spec.HAProxy.TcpPorts.Ports {
		t := `
		listen marklogic-TCP-{{.PortNumber}}
		bind :{{ .PortNumber }}
		mode tcp
		balance leastconn
	  `
		data := &HAProxyTemplateData{
			PortNumber: int(tcpPort.Port),
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
