package k8sutil

import (
	"bytes"
	"fmt"
	"strings"
	"text/template"

	marklogicv1 "github.com/marklogic/marklogic-operator-kubernetes/api/v1"
)

type HAProxyTemplate struct {
	FrontendName     string
	BackendName      string
	TcpName          string
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
	IsPathBased      bool
}

type HAProxyConfig struct {
	IsPathBased       bool
	FrontEndConfigMap map[string]FrontEndConfig
	BackendConfigMap  map[string][]BackendConfig
	TCPConfigMap      map[string][]TCPConfig
}

type FrontEndConfig struct {
	FrontendName string
	IsPathBased  bool
	Port         int
	TargetPort   int
	Path         string
	BackendName  string
}

type BackendConfig struct {
	BackendName string
	IsPathBased bool
	GroupName   string
	Port        int
	TargetPort  int
	Path        string
	Replicas    int
}

type TCPConfig struct {
	TcpName    string
	Port       int
	TargetPort int
	PortName   string
	PodName    string
	Replicas   int
	GroupName  string
}

func generateHAProxyConfig(cr *marklogicv1.MarklogicCluster) *HAProxyConfig {
	config := &HAProxyConfig{}
	frontendMap := make(map[string]FrontEndConfig)
	backendMap := make(map[string][]BackendConfig)
	tcpMap := make(map[string][]TCPConfig)
	defaultAppServer := cr.Spec.HAProxy.AppServers
	groups := cr.Spec.MarkLogicGroups
	config.IsPathBased = *cr.Spec.HAProxy.PathBasedRouting
	for _, group := range groups {
		if group.HAProxy != nil && !group.HAProxy.Enabled {
			continue
		} else {
			// setting is path-based
			if !config.IsPathBased && *group.HAProxy.PathBasedRouting == true {
				config.IsPathBased = true
			}
		}
		// process tcp ports
		if cr.Spec.HAProxy.TcpPorts != nil && group.HAProxy.TcpPorts != nil && group.HAProxy.TcpPorts.Enabled {
			tcpPorts := cr.Spec.HAProxy.TcpPorts.Ports
			if group.HAProxy != nil && group.HAProxy.TcpPorts != nil {
				tcpPorts = group.HAProxy.TcpPorts.Ports
			}
			if len(tcpPorts) == 0 {
				tcpPorts = []marklogicv1.TcpPort{}
			}
			for _, tcpPort := range tcpPorts {
				targetPort := int(tcpPort.TargetPort)
				if tcpPort.TargetPort == 0 {
					targetPort = int(tcpPort.Port)
				}
				var key string
				if int(tcpPort.Port) == targetPort {
					key = fmt.Sprintf("%d", tcpPort.Port)
				} else {
					key = fmt.Sprintf("%d-%d", tcpPort.Port, targetPort)
				}
				if _, exists := tcpMap[key]; exists {
					tcpMap[key] = append(tcpMap[key], TCPConfig{
						TcpName:    key,
						Port:       int(tcpPort.Port),
						TargetPort: targetPort,
						PortName:   tcpPort.Name,
						PodName:    group.Name,
						Replicas:   int(*group.Replicas),
						GroupName:  group.Name,
					})
				} else {
					tcpMap[key] = []TCPConfig{{
						TcpName:    key,
						Port:       int(tcpPort.Port),
						TargetPort: targetPort,
						PortName:   tcpPort.Name,
						PodName:    group.Name,
						Replicas:   int(*group.Replicas),
						GroupName:  group.Name,
					}}
				}
			}
		}

		// process http ports with appServers
		appServers := group.HAProxy.AppServers
		groupPathBased := *cr.Spec.HAProxy.PathBasedRouting
		if group.HAProxy.PathBasedRouting != nil {
			groupPathBased = *group.HAProxy.PathBasedRouting
		}
		if len(appServers) == 0 {
			appServers = defaultAppServer
		}
		for _, appServer := range appServers {
			targetPort := int(appServer.TargetPort)
			if appServer.TargetPort == 0 {
				targetPort = int(appServer.Port)
			}
			var key string
			if !groupPathBased {
				if int(appServer.Port) == targetPort {
					key = fmt.Sprintf("%d", appServer.Port)
				} else {
					key = fmt.Sprintf("%d-%d", appServer.Port, targetPort)
				}
			} else {
				pathWithoutSlashes := strings.ReplaceAll(appServer.Path, "/", "")
				key = fmt.Sprintf("%d-%s-path", appServer.TargetPort, pathWithoutSlashes)
			}

			backendName := "marklogic-" + key + "-backend"
			// only add frontend when pathBasedRouting is set to false for the group
			if !groupPathBased {
				frontendName := "marklogic-" + key + "-frontend"
				if _, exists := frontendMap[key]; !exists {
					frontend := FrontEndConfig{
						FrontendName: frontendName,
						IsPathBased:  groupPathBased,
						Port:         int(appServer.Port),
						TargetPort:   targetPort,
						BackendName:  backendName,
					}
					frontendMap[key] = frontend
				}
			}
			backend := BackendConfig{
				BackendName: backendName,
				GroupName:   group.Name,
				Port:        int(appServer.Port),
				TargetPort:  targetPort,
				Path:        appServer.Path,
				Replicas:    int(*group.Replicas),
				IsPathBased: groupPathBased,
			}
			backendMap[key] = append(backendMap[key], backend)
		}
	}
	config.FrontEndConfigMap = frontendMap
	config.BackendConfigMap = backendMap
	config.TCPConfigMap = tcpMap
	return config
}

// generates frontend config for HAProxy depending on pathBasedRouting flag
// if pathBasedRouting is disabled, it will generate a frontend for each appServer
// otherwise, it will generate a single frontend with path based routing
func generateFrontendConfig(cr *marklogicv1.MarklogicCluster, config *HAProxyConfig) string {
	frontEndConfigs := config.FrontEndConfigMap
	var frontEndDef string
	var data *HAProxyTemplate
	var result string
	if config.IsPathBased {
		// front end configuration for path based routing
		frontEndDef = `
frontend marklogic-pathbased-frontend
  mode http
  option httplog
  bind :{{ .PortNumber}} {{ .SslCert }}
  http-request set-header Host marklogic:{{ .PortNumber}}
  http-request set-header REFERER http://marklogic:{{ .PortNumber}}`
		data = &HAProxyTemplate{
			PortNumber: int(cr.Spec.HAProxy.FrontendPort),
			SslCert:    getSSLConfig(cr.Spec.HAProxy.Tls),
		}
		result = parseTemplateToString(frontEndDef, data)
		for _, backends := range config.BackendConfigMap {
			for _, babackend := range backends {
				if !babackend.IsPathBased {
					continue
				}
				data = &HAProxyTemplate{
					PortNumber:       int(babackend.Port),
					TargetPortNumber: int(babackend.TargetPort),
					Path:             babackend.Path,
					IsPathBased:      babackend.IsPathBased,
					BackendName:      babackend.BackendName,
				}
				result += getFrontendForPathbased(data)

			}
		}
	}
	// front end configuration for non-path based routing
	frontEndDef = `
frontend {{ .FrontendName }}
  mode http
  bind :{{ .PortNumber }} {{ .SslCert }}
  log-format "%ci:%cp [%tr] %ft %b/%s %TR/%Tw/%Tc/%Tr/%Ta %ST %B %CC %CS %tsc %ac/%fc/%bc/%sc/%rc %sq/%bq %hr %hs %{+Q}r"
  default_backend {{ .BackendName }}`
	for _, frontend := range frontEndConfigs {
		data = &HAProxyTemplate{
			FrontendName:     frontend.FrontendName,
			BackendName:      frontend.BackendName,
			PortNumber:       int(frontend.Port),
			TargetPortNumber: int(frontend.TargetPort),
			SslCert:          getSSLConfig(cr.Spec.HAProxy.Tls),
		}
		result += parseTemplateToString(frontEndDef, data) + "\n"
	}
	return result
}

// generates backend config for HAProxy depending on pathBasedRouting flag and appServers
func generateBackendConfig(cr *marklogicv1.MarklogicCluster, config *HAProxyConfig) string {
	backendConfigs := config.BackendConfigMap
	var result string

	backendTemplate := `
backend {{ .BackendName }}
  mode http
  balance leastconn
  option forwardfor
  cookie haproxy insert indirect httponly nocache maxidle 30m maxlife 4h
  stick-table type string len 32 size 10k expire 4h
  stick store-response res.cook(HostId)
  stick store-response res.cook(SessionId)
  stick match req.cook(HostId)
  stick match req.cook(SessionId)
  default-server check`
	for _, backends := range backendConfigs {
		data := &HAProxyTemplate{
			BackendName: backends[0].BackendName,
			PortNumber:  backends[0].Port,
			Path:        backends[0].Path,
		}
		result += parseTemplateToString(backendTemplate, data)
		for _, backend := range backends {
			name := backend.GroupName
			groupReplicas := backend.Replicas
			if backend.IsPathBased {
				backendTemplate += `
  http-request replace-path {{.Path}}(/)?(.*) /\2`
			}
			for i := 0; i < groupReplicas; i++ {
				data := &HAProxyTemplate{
					PortNumber:       backend.TargetPort,
					PodName:          name,
					Path:             backend.Path,
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

func getBackendServerConfigs(data *HAProxyTemplate) string {
	backend := `
  server {{.PodName}}-{{.PortNumber}}-{{.Index}} {{.PodName}}-{{.Index}}.{{.ServiceName}}.{{.NSName}}.svc.{{.ClusterName}}:{{.PortNumber}} resolvers dns init-addr none cookie {{.PodName}}-{{.PortNumber}}-{{.Index}}`
	if data.sslEnabledServer {
		backend += " ssl verify none"
	}

	return parseTemplateToString(backend, data)
}

func getFrontendForPathbased(data *HAProxyTemplate) string {
	frontend := `
  use_backend {{.BackendName}} if { path {{.Path}} } || { path_beg {{.Path}}/ }`
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

func getBackendForTCP(data *HAProxyTemplate) string {
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
func generateTcpConfig(cr *marklogicv1.MarklogicCluster, config *HAProxyConfig) string {
	result := ""
	tcpConfigs := config.TCPConfigMap
	if len(tcpConfigs) == 0 {
		return result
	}
	for _, tcpConfigSlice := range tcpConfigs {
		t := `
listen marklogic-TCP-{{.TcpName }}
  bind :{{ .PortNumber }} {{ .SslCert }}
  mode tcp
  balance leastconn`
		data := &HAProxyTemplate{
			PortNumber: int(tcpConfigSlice[0].Port),
			TcpName:    tcpConfigSlice[0].TcpName,
			SslCert:    getSSLConfig(cr.Spec.HAProxy.Tls),
		}
		result += parseTemplateToString(t, data)
		name := tcpConfigSlice[0].GroupName
		groupReplicas := int(tcpConfigSlice[0].Replicas)
		for _, tcpConfig := range tcpConfigSlice {
			for i := 0; i < groupReplicas; i++ {
				data := &HAProxyTemplate{
					PortNumber:  int(tcpConfig.TargetPort),
					PodName:     tcpConfig.PodName,
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
