package k8sutil

import (
	"github.com/marklogic/marklogic-kubernetes-operator/pkg/result"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func (oc *OperatorContext) ReconcileConfigMap() result.ReconcileResult {
	logger := oc.ReqLogger
	client := oc.Client
	cr := oc.MarklogicGroup

	logger.Info("Reconciling MarkLogic ConfigMap")
	labels := getMarkLogicLabels(cr.Spec.Name)
	annotations := map[string]string{}
	configMapName := cr.Spec.Name + "-scripts"
	objectMeta := generateObjectMeta(configMapName, cr.Namespace, labels, annotations)
	nsName := types.NamespacedName{Name: objectMeta.Name, Namespace: objectMeta.Namespace}
	configmap := &corev1.ConfigMap{}
	err := client.Get(oc.Ctx, nsName, configmap)
	if err != nil {
		if errors.IsNotFound(err) {
			logger.Info("MarkLogic sripts ConfigMap is not found, creating a new one")
			configmapDef := generateConfigMapDef(objectMeta, marklogicServerAsOwner(cr))
			err = oc.createConfigMap(configmapDef)
			if err != nil {
				logger.Info("MarkLogic scripts configmap creation is failed")
				return result.Error(err)
			}
			logger.Info("MarkLogic scripts configmap creation is successful")
			// result.Continue()
		} else {
			logger.Error(err, "MarkLogic scripts configmap creation is failed")
			return result.Error(err)
		}
	}

	return result.Continue()
}

func generateConfigMapDef(configMapMeta metav1.ObjectMeta, ownerRef metav1.OwnerReference) *corev1.ConfigMap {
	configmap := &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{
			Kind:       "ConfigMap",
			APIVersion: "v1",
		},
		ObjectMeta: configMapMeta,
		Data: map[string]string{
			"liveness-probe.sh": getLivenessProbeScript(),
			"copy-certs.sh":     getCopyCertScript(),
			"prestop-hook.sh":   getPrestopHookScript(),
			"poststart-hook.sh": getPoststartHookScript(),
		},
	}
	configmap.SetOwnerReferences(append(configmap.GetOwnerReferences(), ownerRef))
	return configmap
}

func (oc *OperatorContext) createConfigMap(configMap *corev1.ConfigMap) error {
	logger := oc.ReqLogger
	client := oc.Client
	err := client.Create(oc.Ctx, configMap)
	if err != nil {
		logger.Error(err, "MarkLogic script configmap creation is failed")
		return err
	}
	logger.Info("MarkLogic script configmap creation is successful")
	return nil
}

func getLivenessProbeScript() string {
	return `#!/bin/bash
log () {
    local TIMESTAMP=$(date +"%Y-%m-%d %T.%3N")
    # Check to make sure pod doesn't terminate if PID value is empty for any reason
    if [ -n "$pid" ]; then
        echo "${TIMESTAMP} $@" > /proc/$pid/fd/1
    fi
}

pid=$(pgrep -fn start.marklogic)

# Check if ML service is running. Exit with 1 if it is other than running
ml_status=$(/etc/init.d/MarkLogic status)

if [[ "$ml_status" =~ "running" ]]; then
    http_code=$(curl -o /tmp/probe_response.txt -s -w "%{http_code}" "http://${HOSTNAME}:8001/admin/v1/timestamp")
    curl_code=$?
    http_resp=$(cat /tmp/probe_response.txt)

    if [[ $curl_code -ne 0 && $http_code -ne 401 ]]; then
        log "Info: [Liveness Probe] Error with MarkLogic"
        log "Info: [Liveness Probe] Curl response code: "$curl_code
        log "Info: [Liveness Probe] Http response code: "$http_code
        log "Info: [Liveness Probe] Http response message: "$http_resp 
    fi
    rm -f /tmp/probe_response.txt
    exit 0
else
    exit 1
fi
`
}

func getCopyCertScript() string {
	return `#!/bin/bash
MARKLOGIC_ADMIN_USERNAME="$(< /run/secrets/ml-secrets/username)"            
MARKLOGIC_ADMIN_PASSWORD="$(< /run/secrets/ml-secrets/username)"
log () {
    local TIMESTAMP=$(date +"%Y-%m-%d %T.%3N")
    echo "${TIMESTAMP}  $@"
}
if [[ -d "/tmp/server-cert-secrets" ]]; then
    certType="named"
else
    certType="self-signed"
fi
log "Info: [copy-certs] Proceeding with $certType certificate flow."
host_FQDN="$POD_NAME.$MARKLOGIC_FQDN_SUFFIX"
log "Info: [copy-certs] FQDN for this server: $host_FQDN"
foundMatchingCert="false"
if [[ "$certType" == "named" ]]; then
    cp -f /tmp/ca-cert-secret/* /run/secrets/marklogic-certs/;
    cert_paths=$(find /tmp/server-cert-secrets/tls_*.crt)
    for cert_path in $cert_paths; do
    cert_cn=$(openssl x509 -noout -subject -in $cert_path | sed -n 's/.*CN = \([^,]*\).*/\1/p')
    log "Info: [copy-certs] FQDN for the certificate: $cert_cn"
    if [[ "$host_FQDN" == "$cert_cn" ]]; then
        log "Info: [copy-certs] found certificate for the server"
        foundMatchingCert="true"
        cp $cert_path /run/secrets/marklogic-certs/tls.crt
        pkey_path=$(echo "$cert_path" | sed "s:.crt:.key:")
        cp $pkey_path /run/secrets/marklogic-certs/tls.key
        if [[ ! -e "$pkey_path" ]]; then
        log "Error: [copy-certs] private key tls.key for certificate $cert_cn is not found. Exiting."
        exit 1
        fi

        # verify the tls.crt and cacert.pem is valid, otherwise exit
        openssl verify -CAfile /run/secrets/marklogic-certs/cacert.pem /run/secrets/marklogic-certs/tls.crt
        if [[ $? -ne 0 ]]; then
        log "Error: [copy-certs] Server certificate tls.crt verification with cacert.pem failed. Exiting."
        exit 1
        fi
        # verify the tls.crt and tls.key is matching, otherwise exit
        privateKeyMD5=$(openssl rsa -modulus -noout -in /run/secrets/marklogic-certs/tls.key | openssl md5)
        publicKeyMD5=$(openssl x509 -modulus -noout -in /run/secrets/marklogic-certs/tls.crt | openssl md5)
        if [[ -z "privateKeyMD5" ]] || [[ "$privateKeyMD5" != "$publicKeyMD5" ]]; then
        log "Error: [copy-certs] private key tls.key and server certificate tls.crt are not matching. Exiting."
        exit 1
        fi
        log "Info: [copy-certs] certificate and private key are valid."
        break
    fi
    done
    if [[ $foundMatchingCert == "false" ]]; then
    if [[ $POD_NAME = *"-0" ]]; then
        log "Error: [copy-certs] Failed to find matching certificate for the bootstrap server. Exiting."
        exit 1
    else 
        log "Error: [copy-certs] Failed to find matching certificate for the non-bootstrap server. Continuing with temporary certificate for this host. Please update the certificate for this host later."
    fi
    fi
elif [[ "$certType" == "self-signed" ]]; then
    if [[ $POD_NAME != *"-0" ]] || [[ $MARKLOGIC_CLUSTER_TYPE == "non-bootstrap" ]]; then
    log "Info: [copy-certs] Getting CA for bootstrap host"
    cd /run/secrets/marklogic-certs/
    echo quit | openssl s_client -showcerts -servername "${MARKLOGIC_BOOTSTRAP_HOST}" -showcerts -connect "${MARKLOGIC_BOOTSTRAP_HOST}":8000 2>&1 < /dev/null | sed -n '/-----BEGIN/,/-----END/p' > cacert.pem
    fi
else 
    log "Error: [copy-certs] unknown certType: $certType"
    exit 1
fi
`
}

func getPrestopHookScript() string {
	return `#!/bin/bash
MARKLOGIC_ADMIN_USERNAME="$(< /run/secrets/ml-secrets/username)"
MARKLOGIC_ADMIN_PASSWORD="$(< /run/secrets/ml-secrets/password)"

log () {
    local TIMESTAMP=$(date +"%Y-%m-%d %T.%3N")
    # Check to make sure pod doesn't terminate if PID value is empty for any reason
    # If PID value is empty preStart hook logs are not recorded
    if [ -n "$pid" ]; then
        echo "${TIMESTAMP} $@" > /proc/$pid/fd/1
    fi
}

pid=$(pgrep -fn start.marklogic)
log "Info: [prestop] Prestop Hook Execution"

my_host=$(hostname -f)

HTTP_PROTOCOL="http"
HTTPS_OPTION=""
if [[ "$MARKLOGIC_JOIN_TLS_ENABLED" == "true" ]]; then
    HTTP_PROTOCOL="https"
    HTTPS_OPTION="-k"
fi
log "Info: [prestop] MarkLogic Pod Hostname: "$my_host
for ((i = 0; i < 5; i = i + 1)); do
    res_code=$(curl --anyauth --user $MARKLOGIC_ADMIN_USERNAME:$MARKLOGIC_ADMIN_PASSWORD \
        -o /dev/null -m 10 -s -w %{http_code} \
        -i -X POST ${HTTPS_OPTION} --data "state=shutdown&failover=true" \
        -H "Content-type: application/x-www-form-urlencoded" \
        ${HTTP_PROTOCOL}://localhost:8002/manage/v2/hosts/$my_host?format=json)

    if [[ ${res_code} -eq 202 ]]; then
        log "Info: [prestop] Host shut down response code: "$res_code

        while (true)
        do
            ml_status=$(service MarkLogic status)
            log "Info: [prestop] MarkLogic Status: "$ml_status
            if [[ "$ml_status" =~ "running" ]]; then
                sleep 5s
                continue
            else
                break
            fi
        done
        break
    else
        log "ERROR: [prestop] Retry Attempt: "$i
        log "ERROR: [prestop] Host shut down expected response code 202, got "$res_code
        sleep 10s
    fi
done
`
}

func getPoststartHookScript() string {
	return `#!/bin/bash
# Refer to https://docs.marklogic.com/guide/admin-api/cluster#id_10889 for cluster joining process

N_RETRY=60
RETRY_INTERVAL=1
HOST_FQDN="$(hostname).${MARKLOGIC_FQDN_SUFFIX}"

# HTTP_PROTOCOL could be http or https 
HTTP_PROTOCOL="http"
HTTPS_OPTION=""
if [[ "$MARKLOGIC_JOIN_TLS_ENABLED" == "true" ]]; then
    HTTP_PROTOCOL="https"
    HTTPS_OPTION="-k"
fi

IS_BOOTSTRAP_HOST=false
if [[ "$(hostname)" == *-0 ]]; then
    echo "IS_BOOTSTRAP_HOST true"
    IS_BOOTSTRAP_HOST=true
else 
    echo "IS_BOOTSTRAP_HOST false"
fi

# MARKLOGIC_ADMIN_USERNAME="$(< /run/secrets/ml-secrets/username)"
# MARKLOGIC_ADMIN_PASSWORD="$(< /run/secrets/ml-secrets/password)"
MARKLOGIC_ADMIN_USERNAME="admin"
MARKLOGIC_ADMIN_PASSWORD="admin"

pid=$(pgrep -fn start.marklogic)

###############################################################
# Logging utility
###############################################################
info() {
    log "Info" "$@"
}

error() {
    log "Error" "$1"
    local EXIT_STATUS="$2"
    if [[ ${EXIT_STATUS} == "exit" ]]
    then
        exit 1
    fi
}

log () {
    local TIMESTAMP=$(date +"%Y-%m-%d %T.%3N")
    # Check to make sure pod doesn't terminate if PID value is empty for any reason
    # If PID value is empty postStart hook logs are not recorded
    message="${TIMESTAMP} [postStart] $@"
    if [ -n "$pid" ]; then
        echo $message  > /proc/$pid/fd/1
    fi
    
    echo $message >> /tmp/script.log
}

################################################################
# restart_check(hostname, baseline_timestamp)
#
# Use the timestamp service to detect a server restart, given a
# a baseline timestamp. Use N_RETRY and RETRY_INTERVAL to tune
# the test length. Include authentication in the curl command
# so the function works whether or not security is initialized.
#   $1 :  The hostname to test against
#   $2 :  The baseline timestamp
# Returns 0 if restart is detected, exits with an error if not.
################################################################
function restart_check {
    local hostname=$1
    local old_timestamp=$2
    local retry_count
    local last_start

    info "${hostname} - waiting for MarkLogic to restart"
    
    last_start=$( \
        curl -s --anyauth \
        --user "${MARKLOGIC_ADMIN_USERNAME}":"${MARKLOGIC_ADMIN_PASSWORD}" \
        "http://${hostname}:8001/admin/v1/timestamp" \
    )
    for ((retry_count = 0; retry_count < N_RETRY; retry_count = retry_count + 1)); do
        if [ "${old_timestamp}" == "${last_start}" ] || [ -z "${last_start}" ]; then
            info "${hostname} - waiting for MarkLogic to restart: ${old_timestamp} ${last_start}"
            sleep ${RETRY_INTERVAL}
            last_start=$( \
                curl -s --anyauth \
                --user "${MARKLOGIC_ADMIN_USERNAME}":"${MARKLOGIC_ADMIN_PASSWORD}" \
                "http://${hostname}:8001/admin/v1/timestamp" \
            )
        else
            info "${hostname} - MarkLogic has restarted"
            return 0
        fi
    done
    error "${hostname} - failed to restart" exit
}
################################################################
# retry_and_timeout(target_url, expected_response_code, additional_options, return_error)
# The third argument is optional and can be used to pass additional options to curl.
# Fourth argurment is optional, default is set to true, can be used when custom error handling is required,
# if set to true means function will return error and exit if curl fails N_RETRY times
# setting to false means function will return response code instead of failing and exiting.
# Retry a curl command until it returns the expected response
# code or fails N_RETRY times.
# Use RETRY_INTERVAL to tune the test length.
# Validate that response code is the same as expected response
# code or exit with an error.
#
#   $1 :  The target url to test against
#   $2 :  The expected response code
#   $3 :  Additional options to pass to curl
#   $4 :  Option to return error or response code in case of error   
################################################################
function curl_retry_validate {
    local retry_count
    local return_error="${4:-true}"
    for ((retry_count = 0; retry_count < N_RETRY; retry_count = retry_count + 1)); do
        request="curl -m 30 -s -w '%{http_code}' $3 $1"
        response_code=$(eval "${request}")
        if [[ ${response_code} -eq $2 ]]; then
            return "${response_code}"
        fi
        sleep ${RETRY_INTERVAL}
    done
    if [[ "${return_error}" = "false" ]] ; then
        return "${response_code}"  
    fi
    error "Expected response code ${2}, got ${response_code} from ${1}." exit
}

################################################################
# Function to initialize a host
# $1: The host name
# return values: 0 - successfully initialized
#                1 - host not reachable
################################################################
function wait_until_marklogic_ready {
    local host=$1
    info "wait until $host is ready"
    timestamp=$( curl -s --anyauth \
                --user "${MARKLOGIC_ADMIN_USERNAME}":"${MARKLOGIC_ADMIN_PASSWORD}" \
                http://${host}:8001/admin/v1/timestamp )
    if [ -z "${timestamp}" ]; then
        info "${host} - not responding yet"
        sleep 5s
        wait_until_marklogic_ready $host 
    else 
        info "${host} - responding, calling init"
        out="/tmp/${host}.out"

        response_code=$( \
            curl --anyauth -m 30 -s --retry 5 \
            -w '%{http_code}' -o "${out}" \
            -i -X POST -H "Content-type:application/json" \
            -d "${LICENSE_PAYLOAD}" \
            --user "${MARKLOGIC_ADMIN_USERNAME}":"${MARKLOGIC_ADMIN_PASSWORD}" \
            http://${host}:8001/admin/v1/init \
        )
        if [ "${response_code}" = "202" ]; then
            info "${host} - init called, restart triggered"
            last_startup=$( \
                cat "${out}" | 
                grep "last-startup" |
                sed 's%^.*<last-startup.*>\(.*\)</last-startup>.*$%\1%' \
            )

            restart_check "${host}" "${last_startup}"
            info "${host} - restarted"
            info "${host} - init complete"
        elif [ "${response_code}" -eq "204" ]; then
            info "${host} - init called, no restart triggered"
            info "${host} - init complete"
        else
            info "${host} - error calling init: ${response_code}"
        fi
    fi
}

################################################################
# Function to initialize a host
# $1: The host name
# return values: 0 - successfully initialized
#                1 - host not reachable
################################################################
function init_marklogic_host {
    local hostname=$1
    info "initializing host: $hostname"
    timestamp=$( \
        curl -s --anyauth \
        --user "${MARKLOGIC_ADMIN_USERNAME}":"${MARKLOGIC_ADMIN_PASSWORD}" \
        http://${hostname}:8001/admin/v1/timestamp \
    )
    if [ -z "${timestamp}" ]; then
        info "${hostname} - not responding yet"
        return 1
    fi
    info "${hostname} - responding, calling init"
    output_path="/tmp/${hostname}.out"
    response_code=$( \
        curl --anyauth -m 30 -s --retry 5 \
        -w '%{http_code}' -o "${output_path}" \
        -i -X POST -H "Content-type:application/json" \
        -d "${LICENSE_PAYLOAD}" \
        --user "${MARKLOGIC_ADMIN_USERNAME}":"${MARKLOGIC_ADMIN_PASSWORD}" \
        http://${hostname}:8001/admin/v1/init \
    )

    if [ "${response_code}" = "202" ]; then
        info "${hostname} - init called, restart triggered"
        last_startup=$( \
            cat "${output_path}" | 
            grep "last-startup" |
            sed 's%^.*<last-startup.*>\(.*\)</last-startup>.*$%\1%' \
        )

        restart_check "${hostname}" "${last_startup}"
        return 0
    elif [ "${response_code}" -eq "204" ]; then
        info "${hostname} - init called, no restart triggered"
        info "${hostname} - init complete"
        return 0
    else
        info "${hostname} - error calling init: ${response_code}"
        [ -f "${out}" ] && cat "${out}"
    fi
}

################################################################
# Function to bootstrap host is ready:
#   1. If TLS is not enabled, wait until Security DB is installed.
#   2. If TLS is enabled, wait until TLS is turned on in App Server
# return values: 0 - admin user successfully initialized
################################################################
function wait_bootstrap_ready {
    resp=$(curl -w '%{http_code}' -o /dev/null http://$MARKLOGIC_BOOTSTRAP_HOST:8001/admin/v1/timestamp )
    if [[ "$MARKLOGIC_JOIN_TLS_ENABLED" == "true" ]]; then
        # return 403 if tls is enabled
        if [[ $resp -eq 403 ]]; then
            info "Bootstrap host is ready with TLS enabled"
        else
            info "Timestamp response code:$resp. Bootstrap host is not ready with TLS enabled, try again in 10s"
            sleep 10s
            wait_bootstrap_ready
        fi
    else
        if [[ $resp -eq 401 ]]; then
            info "Bootstrap host is ready with no TLS"
        else
            info "Timestamp response code:$resp. Bootstrap host is not ready, try again in 10s"
            sleep 10s
            wait_bootstrap_ready
        fi
    fi
}

################################################################
# Function to initialize admin user and security DB
# 
# return values: 0 - admin user successfully initialized
################################################################
function init_security_db {
    info "initializing as bootstrap cluster"

    # check to see if the bootstrap host is already configured
    response_code=$( \
        curl -s --anyauth \
        -w '%{http_code}' -o "/tmp/${MARKLOGIC_BOOTSTRAP_HOST}.out" \
        --user "${MARKLOGIC_ADMIN_USERNAME}":"${MARKLOGIC_ADMIN_PASSWORD}" $HTTPS_OPTION \
        $HTTP_PROTOCOL://$MARKLOGIC_BOOTSTRAP_HOST:8002/manage/v2/hosts/$MARKLOGIC_BOOTSTRAP_HOST/properties
    )

    if [ "${response_code}" = "200" ]; then
        info "${MARKLOGIC_BOOTSTRAP_HOST} - bootstrap security already initialized"
        return 0
    else
        info "${MARKLOGIC_BOOTSTRAP_HOST} - initializing bootstrap security"

        # Get last restart timestamp directly before instance-admin call to verify restart after
        timestamp=$( \
            curl -s --anyauth \
            --user "${MARKLOGIC_ADMIN_USERNAME}":"${MARKLOGIC_ADMIN_PASSWORD}" \
            "http://${MARKLOGIC_BOOTSTRAP_HOST}:8001/admin/v1/timestamp" \
        )

        curl_retry_validate "http://${MARKLOGIC_BOOTSTRAP_HOST}:8001/admin/v1/instance-admin" 202 \
            "-o /dev/null \
            -X POST -H \"Content-type:application/x-www-form-urlencoded; charset=utf-8\" \
            -d \"admin-username=${MARKLOGIC_ADMIN_USERNAME}\" --data-urlencode \"admin-password=${MARKLOGIC_ADMIN_PASSWORD}\" \
            -d \"realm=${ML_REALM}\" -d \"${MARKLOGIC_WALLET_PASSWORD_PAYLOAD}\""

        restart_check "${MARKLOGIC_BOOTSTRAP_HOST}" "${timestamp}"

        info "${MARKLOGIC_BOOTSTRAP_HOST} - bootstrap security initialized"
        return 0
    fi
}

################################################################
# Function to join marklogic host to cluster
# 
# return values: 0 - admin user successfully initialized
################################################################
function join_cluster {
    hostname=$1

    # check if Bootstrap Host is ready
    # if server could not be reached, response_code == 000
    # if host has not join cluster, return 404
    # if bootstrap host not init, return 403
    # if Security DB not set or credential not correct return 401
    # if host is already in cluster, return 200
    response_code=$( curl -s --anyauth -o /dev/null -w '%{http_code}' \
        --user "${MARKLOGIC_ADMIN_USERNAME}":"${MARKLOGIC_ADMIN_PASSWORD}" $HTTPS_OPTION \
        $HTTP_PROTOCOL://${MARKLOGIC_BOOTSTRAP_HOST}:8002/manage/v2/hosts/${hostname}/properties?format=xml \
    )

    info "response_code: $response_code"

    if [ "${response_code}" = "200" ]; then
        info "host has already joined the cluster"
        return 0
    elif [ "${response_code}" != "404" ]; then
        sleep 10s
        join_cluster $hostname
    else
        info "Proceed to joining bootstrap host"
    fi
    
    # process to join the host

    # Wait until the group is ready
    retry_count=10
    while [ $retry_count -gt 0 ]; do
        GROUP_RESP_CODE=$( curl --anyauth -m 20 -s -o /dev/null -w "%{http_code}" $HTTPS_OPTION -X GET $HTTP_PROTOCOL://${MARKLOGIC_BOOTSTRAP_HOST}:8002/manage/v2/groups/${MARKLOGIC_GROUP} --anyauth --user ${MARKLOGIC_ADMIN_USERNAME}:${MARKLOGIC_ADMIN_PASSWORD} )
        if [[ ${GROUP_RESP_CODE} -eq 200 ]]; then
            info "Found the group, process to join the group"
            break
        else 
            ((retry_count--))
            info "GROUP_RESP_CODE: $GROUP_RESP_CODE , retry $retry_count times to joining ${MARKLOGIC_GROUP} group in marklogic cluster"
            sleep 10s
        fi
    done

    if [[ $retry_count -le 0 ]]; then
        info "retry_count: $retry_count"
        error "pass timeout to wait for the group ready"
        exit 1
    fi
    
    info "${hostname} - joining group ${MARKLOGIC_GROUP}"
    payload=\"group=${MARKLOGIC_GROUP}\"
    curl_retry_validate "http://${hostname}:8001/admin/v1/server-config" 200 \
        "--anyauth --user \"${MARKLOGIC_ADMIN_USERNAME}\":\"${MARKLOGIC_ADMIN_PASSWORD}\" \
        -o /tmp/${hostname}.xml -X GET -H \"Accept: application/xml\""

    curl_retry_validate "$HTTP_PROTOCOL://${MARKLOGIC_BOOTSTRAP_HOST}:8001/admin/v1/cluster-config" 200 \
        "--anyauth $HTTPS_OPTION --user \"${MARKLOGIC_ADMIN_USERNAME}\":\"${MARKLOGIC_ADMIN_PASSWORD}\" \
        -X POST -d \"${payload}\" \
        --data-urlencode \"server-config@/tmp/${hostname}.xml\" \
        -H \"Content-type: application/x-www-form-urlencoded\" \
        -o /tmp/${hostname}_cluster.zip"

    timestamp=$( \
            curl -s --anyauth $HTTPS_OPTION \
            --user "${MARKLOGIC_ADMIN_USERNAME}":"${MARKLOGIC_ADMIN_PASSWORD}" \
            "http://${hostname}:8001/admin/v1/timestamp" \
        )

    curl_retry_validate "http://${hostname}:8001/admin/v1/cluster-config" 202 \
            "-o /dev/null --anyauth --user \"${MARKLOGIC_ADMIN_USERNAME}\":\"${MARKLOGIC_ADMIN_PASSWORD}\" \
            -X POST -H \"Content-type: application/zip\" \
            --data-binary @/tmp/${hostname}_cluster.zip"
    
    # 202 causes restart
    info "${hostname} - restart triggered"
    # restart_check "${hostname}" "${timestamp}"

    info "${hostname} - joined group ${MARKLOGIC_GROUP}"
}

################################################################
# Function to configure MarkLogic Group
# 
# return 
################################################################
function configure_group {

    if [[ "$IS_BOOTSTRAP_HOST" == "true" ]]; then
        group_cfg_template='{"group-name":"%s", "xdqp-ssl-enabled":"%s"}'
        group_cfg=$(printf "$group_cfg_template" "$MARKLOGIC_GROUP" "$XDQP_SSL_ENABLED") 

        # check if host is already in and get the current cluster
        response_code=$( \
            curl -s --anyauth \
            -w '%{http_code}' -o "/tmp/groups.out" \
            --user "${MARKLOGIC_ADMIN_USERNAME}":"${MARKLOGIC_ADMIN_PASSWORD}" \
            http://${MARKLOGIC_BOOTSTRAP_HOST}:8002/manage/v2/hosts/${HOST_FQDN}/properties?format=xml
        )

        if [ "${response_code}" = "200" ]; then
            current_group=$( \
                cat "/tmp/groups.out" | 
                grep "group" |
                sed 's%^.*<group.*>\(.*\)</group>.*$%\1%' \
            )

            info "current_group: $current_group"
            info "group_cfg: $group_cfg"

            # curl retry doesn't work in the lower version
            response_code=$( \
                curl -s --anyauth \
                --user ${MARKLOGIC_ADMIN_USERNAME}:${MARKLOGIC_ADMIN_PASSWORD} \
                -w '%{http_code}' \
                -X PUT \
                -H "Content-type: application/json" \
                -d "${group_cfg}" \
                http://${MARKLOGIC_BOOTSTRAP_HOST}:8002/manage/v2/groups/${current_group}/properties \
            )

            info "response_code: $response_code"

            if [[ "${response_code}" = "204" ]]; then
                info "group \"${current_group}\" updated"
            elif [[ "${response_code}" = "202" ]]; then
                # Note: THIS SHOULD NOT HAPPEN WITH THE CURRENT GROUP CONFIG
                info "group \"${current_group}\" updated and a restart of all hosts in the group was triggered"
            else
                info "unexpected response when updating group \"${current_group}\": ${response_code}"
            fi
        
        fi

        if [[ "$MARKLOGIC_CLUSTER_TYPE" == "non-bootstrap" ]]; then
            info "creating group for other Helm Chart"

            # Create a group if group is not already exits
            GROUP_RESP_CODE=$( curl --anyauth -m 20 -s -o /dev/null -w "%{http_code}" $HTTPS_OPTION -X GET $HTTP_PROTOCOL://${MARKLOGIC_BOOTSTRAP_HOST}:8002/manage/v2/groups/${MARKLOGIC_GROUP} --anyauth --user ${MARKLOGIC_ADMIN_USERNAME}:${MARKLOGIC_ADMIN_PASSWORD} )
            if [[ ${GROUP_RESP_CODE} -eq 200 ]]; then
                info "Skipping creation of group $MARKLOGIC_GROUP as it already exists on the MarkLogic cluster." 
            else 
                res_code=$(curl --anyauth --user ${MARKLOGIC_ADMIN_USERNAME}:${MARKLOGIC_ADMIN_PASSWORD} $HTTPS_OPTION -m 20 -s -w '%{http_code}' -X POST -d "${group_cfg}" -H "Content-type: application/json" $HTTP_PROTOCOL://${MARKLOGIC_BOOTSTRAP_HOST}:8002/manage/v2/groups)
                if [[ ${res_code} -eq 201 ]]; then
                    log "Info: [initContainer] Successfully configured group $MARKLOGIC_GROUP on the MarkLogic cluster."
                else
                    log "Info: [initContainer] Expected response code 201, got $res_code"
                fi
            fi
            
        fi
    else
        info "not bootstrap host. Skip group configuration"
    fi

}

function configure_tls {
    info "Configuring TLS for App Servers"

    AUTH_CURL="curl --anyauth --user $MARKLOGIC_ADMIN_USERNAME:$MARKLOGIC_ADMIN_PASSWORD -m 20 -s "

    cd /tmp/
    if [[ -e "/run/secrets/marklogic-certs/tls.crt" ]]; then
        info "Configuring named certificates on host"
        certType="named"
    else 
        info "Configuring self-signed certificates on host"
        certType="self-signed"
    fi
    info "certType in postStart: $certType"

    cat <<'EOF' > defaultCertificateTemplate.json
{
    "template-name": "defaultTemplate",
    "template-description": "defaultTemplate",
    "key-type": "rsa",
    "key-options": {
        "key-length": "2048"
    },  
    "req": {
        "version": "0",
        "subject": {
            "organizationName": "MarkLogic"
        }
    }
}
EOF

if [[ $POD_NAME == *-0 ]] && [[ $MARKLOGIC_CLUSTER_TYPE == "bootstrap" ]]; then
        log "Info:  creating default certificate Template"
        response=$($AUTH_CURL -X POST --header "Content-Type:application/json" -d @defaultCertificateTemplate.json http://localhost:8002/manage/v2/certificate-templates)
        sleep 5s
        log "Info:  done creating default certificate Template"
    fi
    
    log "Info:  creating insert-host-certificates.json"
    cat <<'EOF' > insert-host-certificates.json
    {
        "operation": "insert-host-certificates",
        "certificates": [
            {
                "certificate": {
                "cert": "CERT",
                "pkey": "PKEY"
                }
            }
        ]
    }
EOF

    log "Info:  creating generateCA.xqy"
    cat <<'EOF' > generateCA.xqy
xquery=
    xquery version "1.0-ml"; 
    import module namespace pki = "http://marklogic.com/xdmp/pki" 
        at "/MarkLogic/pki.xqy";
    let $tid := pki:template-get-id(pki:get-template-by-name("defaultTemplate"))
    return
        pki:generate-template-certificate-authority($tid, 365)
EOF

    log "Info:  creating createTempCert.xqy"
    cat <<'EOF' > createTempCert.xqy
xquery= 
    xquery version "1.0-ml"; 
    import module namespace pki = "http://marklogic.com/xdmp/pki" 
        at "/MarkLogic/pki.xqy";
    import module namespace admin = "http://marklogic.com/xdmp/admin"
        at "/MarkLogic/admin.xqy";
    let $tid := pki:template-get-id(pki:get-template-by-name("defaultTemplate"))
    let $config := admin:get-configuration()
    let $hostname := admin:host-get-name($config, admin:host-get-id($config, xdmp:host-name()))
    return
        pki:generate-temporary-certificate-if-necessary($tid, 365, $hostname, (), ())
EOF
    
    log "Info:  inserting certificates $certType"
    if [[ "$certType" == "named" ]]; then
        log "Info:  creating named certificate"
        cert_path="/run/secrets/marklogic-certs/tls.crt"
        pkey_path="/run/secrets/marklogic-certs/tls.key"
        cp insert-host-certificates.json insert_cert_payload.json
        cert="$(<$cert_path)"
        cert="${cert//$'\n'/}"
        pkey="$(<$pkey_path)"
        pkey="${pkey//$'\n'/}"

        sed -i "s|CERT|$cert|" insert_cert_payload.json
        sed -i "s|CERTIFICATE-----|CERTIFICATE-----\\\\n|" insert_cert_payload.json
        sed -i "s|-----END CERTIFICATE|\\\\n-----END CERTIFICATE|" insert_cert_payload.json
        sed -i "s|PKEY|$pkey|" insert_cert_payload.json
        sed -i "s|PRIVATE KEY-----|PRIVATE KEY-----\\\\n|" insert_cert_payload.json
        sed -i "s|-----END RSA|\\\\n-----END RSA|" insert_cert_payload.json
        sed -i "s|-----END PRIVATE|\\\\n-----END PRIVATE|" insert_cert_payload.json
        
        log "Info:  inserting following certificates for $cert_path for $MARKLOGIC_CLUSTER_TYPE"

        if [[ $POD_NAME == *-0 ]]; then
        res=$($AUTH_CURL -X POST --header "Content-Type:application/json" -d @insert_cert_payload.json http://localhost:8002/manage/v2/certificate-templates/defaultTemplate 2>&1)
        else 
        res=$($AUTH_CURL -k  -X POST --header "Content-Type:application/json" -d @insert_cert_payload.json https://localhost:8002/manage/v2/certificate-templates/defaultTemplate 2>&1)
        fi
        log "Info:  $res"
        sleep 5s
    fi

    if [[ $POD_NAME == *-0 ]]; then
        if [[ $MARKLOGIC_CLUSTER_TYPE == "bootstrap" ]]; then
            log "Info:  Generating Temporary CA Certificate"
            $AUTH_CURL -X POST -i -d @generateCA.xqy \
            -H "Content-type: application/x-www-form-urlencoded" \
            -H "Accept: multipart/mixed; boundary=BOUNDARY" \
            http://localhost:8000/v1/eval
            resp_code=$?
            info "response code for Generating Temporary CA Certificate is $resp_code"
            sleep 5s
            fi
        
            log "Info:  enabling app-servers for HTTPS"
            # Manage need be put in the last in the array to make sure http works for all the requests
            appServers=("App-Services" "Admin" "Manage")
            for appServer in ${appServers[@]}; do
            log "configuring SSL for App Server $appServer"
            curl --anyauth --user $MARKLOGIC_ADMIN_USERNAME:$MARKLOGIC_ADMIN_PASSWORD \
                -X PUT -H "Content-type: application/json" -d '{"ssl-certificate-template":"defaultTemplate"}' \
            http://localhost:8002/manage/v2/servers/${appServer}/properties?group-id=${MARKLOGIC_GROUP}
            sleep 5s
            done
            log "Info:  Configure HTTPS in App Server finished"

            if [[ "$certType" == "self-signed" ]]; then
            log "Info:  Generate temporary certificate if necessary"
            $AUTH_CURL -k -X POST -i -d @createTempCert.xqy -H "Content-type: application/x-www-form-urlencoded" \
            -H "Accept: multipart/mixed; boundary=BOUNDARY" https://localhost:8000/v1/eval
            resp_code=$?
            info "response code for Generate temporary certificate is $resp_code"
        fi
    fi
    
    log "Info:  removing cert keys"
    rm -f /run/secrets/marklogic-certs/*.key
}   

###############################################################
# Env Setup of MarkLogic
###############################################################
# Make sure username and password variables are not empty
if [[ -z "${MARKLOGIC_ADMIN_USERNAME}" ]] || [[ -z "${MARKLOGIC_ADMIN_PASSWORD}" ]]; then
    error "MARKLOGIC_ADMIN_USERNAME and MARKLOGIC_ADMIN_PASSWORD must be set." exit
fi

# generate JSON payload conditionally with license details.
if [[ -z "${LICENSE_KEY}" ]] || [[ -z "${LICENSEE}" ]]; then
    LICENSE_PAYLOAD="{}"
else
    info "LICENSE_KEY and LICENSEE are defined, installing MarkLogic license."
    LICENSE_PAYLOAD="{\"license-key\" : \"${LICENSE_KEY}\",\"licensee\" : \"${LICENSEE}\"}"
fi

# sets realm conditionally based on user input
if [[ -z "${REALM}" ]]; then
    ML_REALM="public"
else
    info "REALM is defined, setting realm."
    ML_REALM="${REALM}"
fi

if [[ -z "${MARKLOGIC_WALLET_PASSWORD}" ]]; then
    MARKLOGIC_WALLET_PASSWORD_PAYLOAD=""
else
    MARKLOGIC_WALLET_PASSWORD_PAYLOAD="wallet-password=${MARKLOGIC_WALLET_PASSWORD}"
fi

###############################################################
info "Start configuring MarkLogic for $HOST_FQDN"
info "Bootstrap host: $MARKLOGIC_BOOTSTRAP_HOST"

# Wait for current pod ready
wait_until_marklogic_ready $HOST_FQDN

# Only do this if the bootstrap host is in the statefulset we are configuring
if [[ "${MARKLOGIC_CLUSTER_TYPE}" = "bootstrap" && "${HOST_FQDN}" = "${MARKLOGIC_BOOTSTRAP_HOST}" ]]; then
    sleep 2s
    init_security_db
    configure_group
else
    wait_bootstrap_ready
    configure_group
    join_cluster $HOST_FQDN
fi

sleep 5s 

# Authentication configuration when path based is used
if [[ $POD_NAME == *-0 ]] && [[ $PATH_BASED_ROUTING == "true" ]]; then                    
    log "Info:  path based routing is set. Adapting authentication method"
    resp=$(curl --anyauth -w "%{http_code}" --user $MARKLOGIC_ADMIN_USERNAME:$MARKLOGIC_ADMIN_PASSWORD -m 20 -s -X PUT -H "Content-type: application/json" -d '{"authentication":"basic"}' http://localhost:8002/manage/v2/servers/Admin/properties?group-id=${MARKLOGIC_GROUP})
    log "Info:  Admin-Servers response code: $resp"
    resp=$(curl --anyauth -w "%{http_code}" --user $MARKLOGIC_ADMIN_USERNAME:$MARKLOGIC_ADMIN_PASSWORD -m 20 -s -X PUT -H "Content-type: application/json" -d '{"authentication":"basic"}' http://localhost:8002/manage/v2/servers/App-Services/properties?group-id=${MARKLOGIC_GROUP})
    log "Info:  App Service response code: $resp"
    resp=$(curl --anyauth -w "%{http_code}" --user $MARKLOGIC_ADMIN_USERNAME:$MARKLOGIC_ADMIN_PASSWORD -m 20 -s -X PUT -H "Content-type: application/json" -d '{"authentication":"basic"}' http://localhost:8002/manage/v2/servers/Manage/properties?group-id=${MARKLOGIC_GROUP})
    log "Info:  Manage response code: $resp"
    log "Info:  Default App-Servers authentication set to basic auth"
else
    log "Info:  This is not the boostrap host or path based routing is not set. Skipping authentication configuration"
fi
#End of authentication configuration

if [[ $MARKLOGIC_JOIN_TLS_ENABLED == "true" ]]; then
    configure_tls
fi

info "helm script completed"
`
}
