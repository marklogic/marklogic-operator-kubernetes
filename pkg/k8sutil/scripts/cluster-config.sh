#! /bin/bash   
# Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved. 

# MarkLogic Cluster Configuration Script
# Handles cluster initialization, security setup, host joining, TLS configuration, and group management
# Refer to https://docs.marklogic.com/guide/admin-api/cluster#id_10889 for cluster joining process

N_RETRY=10
RETRY_INTERVAL=5
HOSTNAME=$(cat /etc/hostname)
HOST_FQDN="${HOSTNAME}.${MARKLOGIC_FQDN_SUFFIX}"
ML_KUBERNETES_FILE_PATH="/var/opt/MarkLogic/Kubernetes"
TLS_CONFIGURATION_CURRENT=false

if [[ "${MARKLOGIC_DYNAMIC_HOST}" == "true" ]]; then
    echo "$(date +"%Y-%m-%d %T.%3N") [cluster-config] Info: Dynamic host mode enabled. Skipping static bootstrap/join path."
    exit 0
fi

# HTTP_PROTOCOL could be http or https 
HTTP_PROTOCOL="http"
HTTPS_OPTION=""
if [[ "$MARKLOGIC_JOIN_TLS_ENABLED" == "true" ]]; then
    HTTP_PROTOCOL="https"
    if [[ -s "/run/secrets/marklogic-certs/cacert.pem" ]]; then
        HTTPS_OPTION="--cacert /run/secrets/marklogic-certs/cacert.pem"
    else
        HTTPS_OPTION="-k"
    fi
fi

IS_BOOTSTRAP_HOST=false
if [[ "${HOSTNAME}" == *-0 ]]; then
    echo "IS_BOOTSTRAP_HOST true"
    IS_BOOTSTRAP_HOST=true
else 
    echo "IS_BOOTSTRAP_HOST false"
fi

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
    message="${TIMESTAMP} [cluster-config] $@"
    echo $message  > /proc/1/fd/1
}

# Function to retry a command based on the return code
# $1: The number of retries
# $2: The command to run
retry() {
    local retries=$1
    shift
    local count=0
    until "$@"; do
        exit_code=$?
        count=$((count + 1))
        if [ $count -ge $retries ]; then
        echo "Command failed after $retries attempts."
        return $exit_code
        fi
        echo "Attempt $count failed. Retrying..."
        sleep 5
    done
}

###############################################################
# Function to get the current host protocol
# $1: The host name
# $2: The port number (default 8001)
###############################################################
get_current_host_protocol() {
    local hostname port protocol resp_code
    hostname="${1:-localhost}"
    port="${2:-8001}"
    protocol="http"
    resp_code=$(curl -s --retry 5 -m 5 -o /dev/null -w '%{http_code}' http://$hostname:$port)
    if [[ $resp_code -eq 403 ]]; then
        protocol="https"
    fi
    echo $protocol
}

###############################################################
# Env Setup of MarkLogic
###############################################################
MARKLOGIC_ADMIN_USERNAME="$(< /run/secrets/ml-secrets/username)"
MARKLOGIC_ADMIN_PASSWORD="$(< /run/secrets/ml-secrets/password)"
MARKLOGIC_MANAGEMENT_USERNAME="marklogic-kubernetes-operator"
MARKLOGIC_MANAGEMENT_PASSWORD=""

# Make sure username and password variables are not empty
if [[ -z "${MARKLOGIC_ADMIN_USERNAME}" ]] || [[ -z "${MARKLOGIC_ADMIN_PASSWORD}" ]]; then
	 error "MarkLogic admin credentials must be set." exit
fi

################################################################
# Wait for the controller to create and verify the dedicated operator user,
# then use it for every operation after host and Security initialization.
################################################################
function activate_operator_credentials {
    local manage_protocol manage_host response_code max_wait_seconds=300
    local deadline=$((SECONDS + max_wait_seconds))
    local -a manage_tls_options=()

    info "Waiting for dedicated operator credentials (up to ${max_wait_seconds} seconds)"
    while ((SECONDS < deadline)); do
        if [[ ! -s /run/secrets/ml-operator-secrets/password ]]; then
            if ((SECONDS < deadline)); then
                sleep "${RETRY_INTERVAL}"
            fi
            continue
        fi

        MARKLOGIC_MANAGEMENT_PASSWORD="$(< /run/secrets/ml-operator-secrets/password)"
        manage_protocol=$(get_current_host_protocol "${MARKLOGIC_BOOTSTRAP_HOST}" 8002)
        manage_host="${MARKLOGIC_BOOTSTRAP_HOST}"
        manage_tls_options=()
        if [[ "${manage_protocol}" == "https" ]]; then
            if [[ -s /run/secrets/marklogic-certs/cacert.pem ]]; then
                manage_tls_options=(--cacert /run/secrets/marklogic-certs/cacert.pem)
            else
                manage_tls_options=(-k)
            fi
        fi

        response_code=$(curl -s -m 20 --anyauth -o /dev/null -w '%{http_code}' \
            --user "${MARKLOGIC_MANAGEMENT_USERNAME}:${MARKLOGIC_MANAGEMENT_PASSWORD}" \
            "${manage_tls_options[@]}" \
            "${manage_protocol}://${manage_host}:8002/manage/v2/hosts?view=status&format=json")
        if [[ "${response_code}" == "200" ]]; then
            info "Dedicated operator credentials are active"
            return 0
        fi

        info "Dedicated operator credentials are not active yet (HTTP ${response_code}). Retry in ${RETRY_INTERVAL}s."
        if ((SECONDS < deadline)); then
            sleep "${RETRY_INTERVAL}"
        fi
    done

    error "Dedicated operator credentials did not become active after ${max_wait_seconds} seconds." exit
}

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
    info "Waiting for MarkLogic to restart."
    local retry_count LAST_START username password
    username=$3
    password=$4
    LAST_START=$(curl -s -m 5 --anyauth --user "${username}:${password}" "http://$1:8001/admin/v1/timestamp")
    for ((retry_count = 0; retry_count < N_RETRY; retry_count = retry_count + 1)); do
        if [ "$2" == "${LAST_START}" ] || [ -z "${LAST_START}" ]; then
            sleep ${RETRY_INTERVAL}
            LAST_START=$(curl -s -m 5 --anyauth --user "${username}:${password}" "http://$1:8001/admin/v1/timestamp")
        else
            info "MarkLogic has restarted."
            return 0
        fi
    done
    error "Failed to restart $1" exit
}

################################################################
# Wait until the bootstrap Management API reports the joining host online.
################################################################
function join_check {
    local hostname=$1 retry_count response_code
    info "Waiting for joining host ${hostname} to be online."
    for ((retry_count = 0; retry_count < N_RETRY; retry_count = retry_count + 1)); do
        response_code=$(curl -s -m 20 --anyauth \
            --user "${MARKLOGIC_MANAGEMENT_USERNAME}:${MARKLOGIC_MANAGEMENT_PASSWORD}" \
            -o /tmp/marklogic-joining-host-status.json -w '%{http_code}' $HTTPS_OPTION \
            "${HTTP_PROTOCOL}://${MARKLOGIC_BOOTSTRAP_HOST}:8002/manage/v2/hosts/${hostname}?view=status&format=json")
        if [[ "${response_code}" == "200" ]] && \
            grep -Eq '"online"[[:space:]]*:[[:space:]]*(true|\{[^}]*"value"[[:space:]]*:[[:space:]]*true)' /tmp/marklogic-joining-host-status.json; then
            info "Joining host ${hostname} is online."
            return 0
        fi
        info "Joining host ${hostname} is not online yet (HTTP ${response_code}). Retry in ${RETRY_INTERVAL}s."
        sleep "${RETRY_INTERVAL}"
    done
    error "Joining host ${hostname} did not reach the online state after joining." exit
}

################################################################
# curl_retry_validate(return_error, endpoint, expected_response_code, curl_options...)
# Retry a curl command until it returns the expected response
# code or fails N_RETRY times.
# Use RETRY_INTERVAL to tune the test length.
# Validate that response code is the same as expected response
# code or exit with an error.
#
#   $1 :  Flag indicating if the script should exit if the given response code is not received ("true" to exit, "false" to return the response code")
#   $2 :  The target url to test against
#   $3 :  The expected response code
#   $4+:  Additional options to pass to curl
################################################################
function curl_retry_validate {
    local retry_count response response_code response_content
    local return_error=$1; shift
    local endpoint=$1; shift
    local expected_response_code=$1; shift
    local curl_options=("$@")

    for ((retry_count = 0; retry_count < N_RETRY; retry_count = retry_count + 1)); do
        response=$(curl -s -m 30 -w '%{http_code}' "${curl_options[@]}" "$endpoint")
        response_code=$(tail -n1 <<< "$response")
        CURL_RESPONSE_CODE=${response_code}
        response_content=$(sed '$ d' <<< "$response")
        if [[ ${response_code} -eq ${expected_response_code} ]]; then
            return ${response_code}
        else
            echo "${response_content}" > /tmp/start-marklogic_curl_retry_validate.log
        fi
        
        sleep ${RETRY_INTERVAL}
    done

    if [[ "${return_error}" = "false" ]] ; then
        return ${response_code}
    fi
    [ -f "/tmp/start-marklogic_curl_retry_validate.log" ] && cat start-marklogic_curl_retry_validate.log
    error "Expected response code ${expected_response_code}, got ${response_code} from ${endpoint}." exit
}

################################################################
# Function to initialize a host
# $1: The host name
# return values: 0 - successfully initialized
#                1 - host not reachable
################################################################
function init_marklogic {
    local host=$1
    info "wait until $host is ready"
    timestamp=$( curl -s --anyauth -m 4 \
                --user "${MARKLOGIC_ADMIN_USERNAME}":"${MARKLOGIC_ADMIN_PASSWORD}" \
                http://localhost:8001/admin/v1/timestamp )
    if [ -z "${timestamp}" ]; then
        info "${host} - not responding yet"
        sleep 10s
        init_marklogic $host
        return 0
    else 
        info "${host} - responding with $timestamp"
        out="/tmp/${host}.out"

        response_code=$( \
            curl --anyauth -m 30 -s --retry 5 \
            -w '%{http_code}' -o "${out}" \
            -i -X POST -H "Content-type:application/json" \
            -d "${LICENSE_PAYLOAD}" \
            --user "${MARKLOGIC_ADMIN_USERNAME}":"${MARKLOGIC_ADMIN_PASSWORD}" \
            http://localhost:8001/admin/v1/init \
        )
        if [ "${response_code}" = "202" ]; then
            info "${host} - init called, restart triggered"
            last_startup=$( \
                cat "${out}" | 
                grep "last-startup" |
                sed 's%^.*<last-startup.*>\(.*\)</last-startup>.*$%\1%' \
            )

            restart_check "${host}" "${last_startup}" "${MARKLOGIC_ADMIN_USERNAME}" "${MARKLOGIC_ADMIN_PASSWORD}"
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
# Function to bootstrap host is ready:
#   1. If TLS is not enabled, wait until Security DB is installed.
#   2. If TLS is enabled, wait until TLS is turned on in App Server
# return values: 0 - admin user successfully initialized
################################################################
function wait_bootstrap_ready {
    local body
    body=$(mktemp)
    resp=$(curl -s -w '%{http_code}' -o "$body" http://$MARKLOGIC_BOOTSTRAP_HOST:8001/admin/v1/timestamp )
    if [[ "$MARKLOGIC_JOIN_TLS_ENABLED" == "true" ]]; then
        # The bootstrap host is ready once its Admin endpoint stops serving the
        # timestamp openly. MarkLogic 12.0.x rejects the plaintext request with
        # 403 (HTTPS required), while 12.1+ responds 500 with XDMP-NOUSER
        # (authentication required). Accept either as the TLS-enabled ready signal.
        if [[ $resp -eq 403 ]] || { [[ $resp -eq 500 ]] && grep -q "XDMP-NOUSER" "$body"; }; then
            info "Bootstrap host is ready with TLS enabled"
            rm -f "$body"
        else
            info "Calling Bootstrap host with response code:$resp. Bootstrap host is not ready with TLS enabled, try again in 10s"
            rm -f "$body"
            sleep 10s
            wait_bootstrap_ready
            return 0
        fi
    else
        if [[ $resp -eq 401 ]]; then
            info "Bootstrap host is ready with no TLS"
            rm -f "$body"
        else
            info "Calling Bootstrap host with response code:$resp. Bootstrap host is not ready, try again in 10s"
            rm -f "$body"
            sleep 10s
            wait_bootstrap_ready
            return 0
        fi
    fi
}

################################################################
# Return success only when this local host authenticates against
# the shared cluster Security database with active management credentials.
################################################################
function local_host_uses_shared_security {
    local manage_protocol manage_host response_code
    local -a manage_tls_options=()

    manage_protocol=$(get_current_host_protocol "$HOST_FQDN" 8002)
    manage_host="localhost"
    if [[ "$manage_protocol" == "https" ]]; then
        manage_host="$HOST_FQDN"
        if [[ -s "/run/secrets/marklogic-certs/cacert.pem" ]]; then
            manage_tls_options=(--cacert /run/secrets/marklogic-certs/cacert.pem)
        else
            manage_tls_options=(-k)
        fi
    fi

    response_code=$(curl -s -m 20 --anyauth -o /dev/null -w '%{http_code}' \
        --user "${MARKLOGIC_MANAGEMENT_USERNAME}:${MARKLOGIC_MANAGEMENT_PASSWORD}" \
        "${manage_tls_options[@]}" \
        "${manage_protocol}://${manage_host}:8002/manage/v2/hosts/${HOST_FQDN}/properties?format=xml")

    [[ "$response_code" == "200" ]]
}

function bootstrap_host_registration_status {
    local hostname=$1

    curl -s --anyauth -o /dev/null -w '%{http_code}' \
        --user "${MARKLOGIC_MANAGEMENT_USERNAME}:${MARKLOGIC_MANAGEMENT_PASSWORD}" $HTTPS_OPTION \
        "$HTTP_PROTOCOL://${MARKLOGIC_BOOTSTRAP_HOST}:8002/manage/v2/hosts/${hostname}/properties?format=xml"
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
        --user "${MARKLOGIC_ADMIN_USERNAME}:${MARKLOGIC_ADMIN_PASSWORD}" $HTTPS_OPTION \
        $HTTP_PROTOCOL://$MARKLOGIC_BOOTSTRAP_HOST:8002/manage/v2/hosts/$MARKLOGIC_BOOTSTRAP_HOST/properties
    )

    if [ "${response_code}" = "200" ]; then
        info "${MARKLOGIC_BOOTSTRAP_HOST} - bootstrap security already initialized"
        return 0
    else
        info "initializing bootstrap security"

        # Get last restart timestamp directly before instance-admin call to verify restart after
        timestamp=$( \
            curl -s --anyauth \
            --user "${MARKLOGIC_ADMIN_USERNAME}":"${MARKLOGIC_ADMIN_PASSWORD}" \
            "http://${MARKLOGIC_BOOTSTRAP_HOST}:8001/admin/v1/timestamp" \
        )

        curl_retry_validate false "http://${MARKLOGIC_BOOTSTRAP_HOST}:8001/admin/v1/instance-admin" 202 \
            "-o" "/dev/null" \
            "-H" "Content-type:application/x-www-form-urlencoded; charset=utf-8" \
            "--data-urlencode" "admin-username=${MARKLOGIC_ADMIN_USERNAME}" "--data-urlencode" "admin-password=${MARKLOGIC_ADMIN_PASSWORD}" \
            "--data-urlencode" "realm=${ML_REALM}" "--data-urlencode" "${MARKLOGIC_WALLET_PASSWORD_PAYLOAD}"

        restart_check "${MARKLOGIC_BOOTSTRAP_HOST}" "${timestamp}"

        info "bootstrap security initialized"
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
    retry_count=5

    while [ $retry_count -gt 0 ]; do
        # check if host is already in the cluster
        # if server could not be reached, response_code == 000
        # if host has not join cluster, return 404
        # if bootstrap host not init, return 403
        # if Security DB not set or credential not correct return 401
        # if host is already in cluster, return 200
        response_code=$(bootstrap_host_registration_status "$hostname")

        if [ "${response_code}" = "200" ]; then
            if local_host_uses_shared_security; then
                info "host registration and local shared Security authentication confirm the host has already joined the cluster"
                return 0
            fi
            error "Bootstrap already contains ${hostname}, but the local host does not use shared Security. Refusing to create a duplicate static host registration; remove or recover the stale host registration before retrying." exit
        elif [ "${response_code}" = "401" ]; then
            error "Failed to join the cluster: Security DB not set or credential not correct. Exit."
            exit 1
        elif [ "${response_code}" != "404" ]; then
            info "Response code from bootstrap host: ${response_code}. Retry again in 10s"
            sleep 10s
            ((retry_count--))
            if [ $retry_count -le 0 ]; then
                error "Failed to get the expected response form bootstrap host after 5 times retry. Exit."
                exit 1
            fi
        else
            info "Proceed to joining bootstrap host"
            break
        fi
    done

    # process to join the host
    # Wait until the group is ready
    retry_count=10
    while [ $retry_count -gt 0 ]; do
        GROUP_RESP_CODE=$( curl --anyauth -m 20 -s -o /dev/null -w "%{http_code}" $HTTPS_OPTION -X GET $HTTP_PROTOCOL://${MARKLOGIC_BOOTSTRAP_HOST}:8002/manage/v2/groups/${MARKLOGIC_GROUP} --anyauth --user "${MARKLOGIC_MANAGEMENT_USERNAME}:${MARKLOGIC_MANAGEMENT_PASSWORD}" )
        info "MARKLOGIC_BOOTSTRAP_HOST: $MARKLOGIC_BOOTSTRAP_HOST"
        info "MARKLOGIC_GROUP: $MARKLOGIC_GROUP"
        info "GROUP_RESP_CODE: $GROUP_RESP_CODE"
        if [[ ${GROUP_RESP_CODE} -eq 200 ]]; then
            info "Found the group, process to join the group"
            break
        else 
            info "GROUP_RESP_CODE: $GROUP_RESP_CODE , retry $retry_count times to joining ${MARKLOGIC_GROUP} group in marklogic cluster"
            sleep 10s
            ((retry_count--))
            if [[ $retry_count -le 0 ]]; then
                info "retry_count: $retry_count"
                error "pass timeout to wait for the group ready"
                exit 1
            fi
        fi
    done

    info "joining cluster of group ${MARKLOGIC_GROUP}"
    MARKLOGIC_GROUP_PAYLOAD="group=${MARKLOGIC_GROUP}"
    curl_retry_validate false "http://localhost:8001/admin/v1/server-config" 200 \
        "-o" "/tmp/host.xml" "-X" "GET" "-H" "Accept: application/xml"
    
    info "getting cluster-config from bootstrap host"
    curl_retry_validate false "$HTTP_PROTOCOL://${MARKLOGIC_BOOTSTRAP_HOST}:8001/admin/v1/cluster-config" 200 \
        "--anyauth" "--user" "${MARKLOGIC_MANAGEMENT_USERNAME}:${MARKLOGIC_MANAGEMENT_PASSWORD}" \
        "-X" "POST" "-d" "${MARKLOGIC_GROUP_PAYLOAD}" \
        "--data-urlencode" "server-config@/tmp/host.xml" \
        "-H" "Content-type: application/x-www-form-urlencoded" \
        "-o" "/tmp/cluster.zip" $HTTPS_OPTION

    response_code=${CURL_RESPONSE_CODE:-0}
    cluster_signature=""
    IFS= read -r -N 2 cluster_signature < /tmp/cluster.zip
    if [[ "${response_code}" != "200" ]] || [[ "${cluster_signature}" != "PK" ]]; then
        rm -f /tmp/cluster.zip
        error "Bootstrap host returned an invalid cluster configuration (HTTP ${response_code})." exit
    fi

    timestamp=$(curl -s "http://localhost:8001/admin/v1/timestamp")

    info "joining cluster of group ${MARKLOGIC_GROUP}"
    curl_retry_validate false "http://localhost:8001/admin/v1/cluster-config" 202 \
            "-o" "/dev/null" \
            "-X" "POST" "-H" "Content-type: application/zip" \
            "--data-binary" "@/tmp/cluster.zip"

    response_code=${CURL_RESPONSE_CODE:-0}
    if [[ "${response_code}" != "202" ]]; then
        error "Cluster join request failed with HTTP ${response_code}." exit
    fi
    
    # 202 causes restart
    info "restart triggered"
    restart_check "localhost" "${timestamp}" "${MARKLOGIC_MANAGEMENT_USERNAME}" "${MARKLOGIC_MANAGEMENT_PASSWORD}"
    join_check "${hostname}"

    info "joined group ${MARKLOGIC_GROUP}"
}

################################################################
# Function to configure MarkLogic Group
# 
# return 
################################################################
function configure_group {
    local LOCAL_HTTP_PROTOCOL LOCAL_HTTPS_OPTION
    LOCAL_HTTP_PROTOCOL="http"
    LOCAL_HTTPS_OPTION=""
    bootstrap_protocol=$(get_current_host_protocol $MARKLOGIC_BOOTSTRAP_HOST)
    if [[ $bootstrap_protocol == "https" ]]; then
        LOCAL_HTTP_PROTOCOL="https"
        if [[ -s "/run/secrets/marklogic-certs/cacert.pem" ]]; then
            LOCAL_HTTPS_OPTION="--cacert /run/secrets/marklogic-certs/cacert.pem"
        else
            LOCAL_HTTPS_OPTION="-k"
        fi
    fi  
    log "configuring group"
    if [[ "$IS_BOOTSTRAP_HOST" == "true" ]]; then
        group_cfg_template='{"group-name":"%s", "xdqp-ssl-enabled":"%s"}'
        group_cfg=$(printf "$group_cfg_template" "$MARKLOGIC_GROUP" "$XDQP_SSL_ENABLED") 

        # check if host is already in and get the current cluster
        curl_retry_validate false "$LOCAL_HTTP_PROTOCOL://${MARKLOGIC_BOOTSTRAP_HOST}:8002/manage/v2/hosts/${HOST_FQDN}/properties?format=xml" 200 \
            "--anyauth" "--user" "${MARKLOGIC_MANAGEMENT_USERNAME}:${MARKLOGIC_MANAGEMENT_PASSWORD}" \
            "-o" "/tmp/groups.out" $LOCAL_HTTPS_OPTION

        response_code=$?
        info "response_code: $response_code"
        if [ "${response_code}" = "200" ]; then
            current_group=$( \
                cat "/tmp/groups.out" | 
                grep "<group>" |
                sed 's%^.*<group.*>\(.*\)</group>.*$%\1%' \
            )

            info "current_group: $current_group"
            info "group_cfg: $group_cfg"

            response_code=$( \
                curl -s --anyauth \
                --user "${MARKLOGIC_MANAGEMENT_USERNAME}:${MARKLOGIC_MANAGEMENT_PASSWORD}" \
                -w '%{http_code}' --retry 5  \
                -X PUT \
                -H "Content-type: application/json" \
                $LOCAL_HTTPS_OPTION -d "${group_cfg}" \
                $LOCAL_HTTP_PROTOCOL://${MARKLOGIC_BOOTSTRAP_HOST}:8002/manage/v2/groups/${current_group}/properties \
            )

            info "response_code: $response_code"

            if [[ "${response_code}" = "204" ]]; then
                info "group \"${current_group}\" updated"
            elif [[ "${response_code}" = "202" ]]; then
                # Note: THIS SHOULD NOT HAPPEN WITH THE CURRENT GROUP CONFIG
                info "group \"${current_group}\" updated and a restart of all hosts in the group was triggered"
            else
                error "unexpected response when updating group \"${current_group}\": ${response_code}"
                return 1
            fi
        else
            info "failed to get current group, response code: ${response_code}"
        fi

        if [[ "$MARKLOGIC_CLUSTER_TYPE" == "non-bootstrap" ]]; then
            info "creating group $MARKLOGIC_GROUP"

            # Detect the current protocol of bootstrap host for group operations
            GROUP_HTTP_PROTOCOL="http"
            GROUP_HTTPS_OPTION=""
            group_bootstrap_protocol=$(get_current_host_protocol $MARKLOGIC_BOOTSTRAP_HOST 8002)
            if [[ $group_bootstrap_protocol == "https" ]]; then
                GROUP_HTTP_PROTOCOL="https"
                if [[ -s "/run/secrets/marklogic-certs/cacert.pem" ]]; then
                    GROUP_HTTPS_OPTION="--cacert /run/secrets/marklogic-certs/cacert.pem"
                else
                    GROUP_HTTPS_OPTION="-k"
                fi
                info "Bootstrap host is using HTTPS on port 8002"
            else
                info "Bootstrap host is using HTTP on port 8002"
            fi

            # Create a group if group is not already exits
            GROUP_RESP_CODE=$( curl --anyauth --retry 5 -m 20 -s -o /dev/null -w "%{http_code}" $GROUP_HTTPS_OPTION -X GET $GROUP_HTTP_PROTOCOL://${MARKLOGIC_BOOTSTRAP_HOST}:8002/manage/v2/groups/${MARKLOGIC_GROUP} --anyauth --user "${MARKLOGIC_MANAGEMENT_USERNAME}:${MARKLOGIC_MANAGEMENT_PASSWORD}" )
            if [[ ${GROUP_RESP_CODE} -eq 200 ]]; then
                info "Skipping creation of group $MARKLOGIC_GROUP as it already exists on the MarkLogic cluster." 
            else 
                res_code=$(curl --anyauth --retry 5 --user "${MARKLOGIC_MANAGEMENT_USERNAME}:${MARKLOGIC_MANAGEMENT_PASSWORD}" $GROUP_HTTPS_OPTION -m 20 -s -w '%{http_code}' -X POST -d "${group_cfg}" -H "Content-type: application/json" $GROUP_HTTP_PROTOCOL://${MARKLOGIC_BOOTSTRAP_HOST}:8002/manage/v2/groups)
                if [[ ${res_code} -eq 201 ]]; then
                    log "Info: Successfully configured group $MARKLOGIC_GROUP on the MarkLogic cluster."
                else
                    log "Info: Expected response code 201, got $res_code for group creation"
                fi
            fi
            log "Info: Group $MARKLOGIC_GROUP has been created, configuring App-server App-Services in group $MARKLOGIC_GROUP on the MarkLogic cluster."
            app_services_authentication=""
            if [[ "$PATH_BASED_ROUTING" == "true" ]]; then
                app_services_authentication='"authentication":"basic", '
            fi
            app_services_payload="{\"server-name\":\"App-Services\", ${app_services_authentication}\"root\":\"/\", \"port\":8000,\"modules-database\":\"Modules\", \"content-database\":\"Documents\", \"error-handler\":\"/MarkLogic/rest-api/8000-error-handler.xqy\", \"url-rewriter\":\"/MarkLogic/rest-api/8000-rewriter.xml\"}"
            res_code=`curl --retry 5 --retry-max-time 60 --anyauth --user "${MARKLOGIC_MANAGEMENT_USERNAME}:${MARKLOGIC_MANAGEMENT_PASSWORD}" -m 20 -s ${GROUP_HTTPS_OPTION} -o /tmp/create-app-services-response.json -w '%{http_code}' -X POST -d "${app_services_payload}" -H "Content-type: application/json" "${GROUP_HTTP_PROTOCOL}://${MARKLOGIC_BOOTSTRAP_HOST}:8002/manage/v2/servers?group-id=${MARKLOGIC_GROUP}&server-type=http"`
            if [[ ${res_code} -eq 201 ]]; then
              log "Info: Successfully configured App-server App-Services into group $MARKLOGIC_GROUP on the MarkLogic cluster."
            else
              log "Info: Expected response code 201, got $res_code for App-Services configuration"
                            cat /tmp/create-app-services-response.json
              exit 1
            fi        
        fi
    else
        info "not bootstrap host. Skip group configuration"
    fi

}

function configure_tls {
    local protocol local_manage_protocol local_manage_host
    local -a local_manage_https_option=()
    if [[ "$IS_BOOTSTRAP_HOST" == "true" ]] && [[ $MARKLOGIC_CLUSTER_TYPE == "bootstrap" ]]; then
        protocol=$(get_current_host_protocol)
        log "Info:  Current host protocol: $protocol"
        if [[ $protocol == "https" ]]; then
            log "Info: MarkLogic server has already configured HTTPS for bootstrap host."
            return 0
        fi
    fi

    info "Configuring TLS for App Servers"

    AUTH_CURL=(curl --anyauth --user "${MARKLOGIC_MANAGEMENT_USERNAME}:${MARKLOGIC_MANAGEMENT_PASSWORD}" -m 20 -sS)

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
    "template-description": "Default certificate template created by MarkLogic Kubernetes Operator", 
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

if [[ "$IS_BOOTSTRAP_HOST" == "true" ]] && [[ $MARKLOGIC_CLUSTER_TYPE == "bootstrap" ]]; then
        log "Info:  creating default certificate Template"
        response=$("${AUTH_CURL[@]}" -X POST --header "Content-Type:application/json" -d @defaultCertificateTemplate.json http://localhost:8002/manage/v2/certificate-templates)
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

        local_manage_protocol=$(get_current_host_protocol localhost 8002)
        local_manage_host="localhost"
        if [[ "$local_manage_protocol" == "https" ]]; then
            if [[ -s "/run/secrets/marklogic-certs/cacert.pem" ]]; then
                local_manage_https_option=(--cacert /run/secrets/marklogic-certs/cacert.pem)
                local_manage_host="$HOST_FQDN"
            else
                local_manage_https_option=(-k)
            fi
        fi
        log "Info:  Local Manage protocol for certificate insertion: $local_manage_protocol"
        if ! res=$("${AUTH_CURL[@]}" --fail -w ' HTTP %{http_code}' "${local_manage_https_option[@]}" \
            -X POST --header "Content-Type:application/json" -d @insert_cert_payload.json \
            "${local_manage_protocol}://${local_manage_host}:8002/manage/v2/certificate-templates/defaultTemplate" 2>&1); then
            error "Failed to insert named certificate: $res" exit
        fi
        log "Info:  $res"
        sleep 5s
    fi

    if [[ "$IS_BOOTSTRAP_HOST" == "true" ]]; then
        if [[ $MARKLOGIC_CLUSTER_TYPE == "bootstrap" ]]; then
            log "Info:  Generating Temporary CA Certificate"
            "${AUTH_CURL[@]}" -X POST -i -d @generateCA.xqy \
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
            "${AUTH_CURL[@]}" \
                -X PUT -H "Content-type: application/json" -d '{"ssl-certificate-template":"defaultTemplate"}' \
            http://localhost:8002/manage/v2/servers/${appServer}/properties?group-id=${MARKLOGIC_GROUP}
            sleep 5s
            done
            log "Info:  Configure HTTPS in App Server finished"

            if [[ "$certType" == "self-signed" ]]; then
            log "Info:  Generate temporary certificate if necessary"
            "${AUTH_CURL[@]}" $HTTPS_OPTION -X POST -i -d @createTempCert.xqy -H "Content-type: application/x-www-form-urlencoded" \
            -H "Accept: multipart/mixed; boundary=BOUNDARY" "https://${HOST_FQDN}:8000/v1/eval"
            resp_code=$?
            info "response code for Generate temporary certificate is $resp_code"
        fi
    fi
    
    log "Info: removing cert keys"
    rm -f /run/secrets/marklogic-certs/*.key
}


function configure_app_server_basic_auth {
    local app_server=$1
    local manage_protocol manage_host response_code response_file
    local -a manage_tls_options=()

    manage_protocol=$(get_current_host_protocol localhost 8002)
    manage_host="localhost"
    if [[ "$manage_protocol" == "https" ]]; then
        manage_host="$HOST_FQDN"
        if [[ -s "/run/secrets/marklogic-certs/cacert.pem" ]]; then
            manage_tls_options=(--cacert /run/secrets/marklogic-certs/cacert.pem)
        else
            manage_tls_options=(-k)
        fi
    fi

    response_file="/tmp/configure-${app_server}-basic-auth-response.json"
    response_code=$(curl --anyauth --retry 5 --retry-max-time 60 \
        --user "${MARKLOGIC_MANAGEMENT_USERNAME}:${MARKLOGIC_MANAGEMENT_PASSWORD}" \
        -m 20 -s -o "$response_file" -w '%{http_code}' "${manage_tls_options[@]}" \
        -X PUT -H "Content-type: application/json" -d '{"authentication":"basic"}' \
        "${manage_protocol}://${manage_host}:8002/manage/v2/servers/${app_server}/properties?group-id=${MARKLOGIC_GROUP}")

    case "$response_code" in
        200|202|204)
            log "Info: App Server ${app_server} authentication set to basic (HTTP ${response_code})"
            return 0
            ;;
        *)
            log "Error: Failed to set App Server ${app_server} authentication to basic (HTTP ${response_code})"
            [[ -s "$response_file" ]] && cat "$response_file"
            return 1
            ;;
    esac
}

function configure_path_based_routing {
    info "Path based routing: $PATH_BASED_ROUTING"
    if [[ $PATH_BASED_ROUTING == "true" ]]; then
        log "Info:  path based routing is set. Adapting authentication method"
        for app_server in Admin App-Services Manage; do
            retry 5 configure_app_server_basic_auth "$app_server" || \
                error "Failed to configure basic authentication for App Server ${app_server}." exit
        done
        log "Info:  Default App-Servers authentication set to basic auth"
    else
        log "Info:  Path based routing is not set. Skipping authentication configuration"
    fi
}

function set_status_file {
    mkdir -p $ML_KUBERNETES_FILE_PATH
    fqdn=$(hostname -f)
    status_file="$ML_KUBERNETES_FILE_PATH/status.txt"
    group_name="${MARKLOGIC_GROUP}"
    group_xdqp_ssl_enabled="${XDQP_SSL_ENABLED}"
    https_enabled="${MARKLOGIC_JOIN_TLS_ENABLED}"
    cert_fingerprint=""
    if [[ -f "/run/secrets/marklogic-certs/tls.crt" ]]; then
        cert_fingerprint=$(openssl x509 -in /run/secrets/marklogic-certs/tls.crt -noout -fingerprint -sha256 | cut -d= -f2 | tr -d ':')
    fi
    echo "fqdn=${fqdn}" > $status_file
    echo "group_name=${group_name}" >> $status_file
    echo "group_xdqp_ssl_enabled=${group_xdqp_ssl_enabled}" >> $status_file
    echo "https_enabled=${https_enabled}" >> $status_file
    echo "cert_fingerprint=${cert_fingerprint}" >> $status_file
}

function check_status_file_for_nonbootstrap {
    if [[ -f "$ML_KUBERNETES_FILE_PATH/status.txt" ]]; then
        log "Info: status file exists. Configuration functions will verify state."
        cert_fingerprint=""
        source "$ML_KUBERNETES_FILE_PATH/status.txt"
        current_cert_fingerprint=""
        if [[ -f "/run/secrets/marklogic-certs/tls.crt" ]]; then
            current_cert_fingerprint=$(openssl x509 -in /run/secrets/marklogic-certs/tls.crt -noout -fingerprint -sha256 | cut -d= -f2 | tr -d ':')
        fi
        if [[ "$https_enabled" == "$MARKLOGIC_JOIN_TLS_ENABLED" ]] && [[ "$cert_fingerprint" == "$current_cert_fingerprint" ]]; then
            TLS_CONFIGURATION_CURRENT=true
        fi
        # Don't exit early - let the configuration functions handle state verification
        # They already check if the host is in the cluster and skip if needed
    else
        log "Info: status file does not exist. Continue"
    fi
}

function check_status_file_for_boostrap {
    if [[ -f "$ML_KUBERNETES_FILE_PATH/status.txt" ]]; then
        new_group_name="${MARKLOGIC_GROUP}"
        new_group_xdqp_ssl_enabled="${XDQP_SSL_ENABLED}"
        new_https_enabled="${MARKLOGIC_JOIN_TLS_ENABLED}"
        current_cert_fingerprint=""
        if [[ -f "/run/secrets/marklogic-certs/tls.crt" ]]; then
            current_cert_fingerprint=$(openssl x509 -in /run/secrets/marklogic-certs/tls.crt -noout -fingerprint -sha256 | cut -d= -f2 | tr -d ':')
        fi
        cert_fingerprint=""
        source "$ML_KUBERNETES_FILE_PATH/status.txt"
        
        # Check if configuration values have changed
        if [[ "$new_group_name" != "$group_name" ]] || [[ "$new_group_xdqp_ssl_enabled" != "$group_xdqp_ssl_enabled" ]] || [[ "$new_https_enabled" != "$https_enabled" ]]; then
            log "Info: Configuration values changed. Removing status file and continuing."
            rm -f "$ML_KUBERNETES_FILE_PATH/status.txt"
            return 0
        fi
        
        # Values match - let configuration functions verify and skip if already done
        log "Info: Configuration values match. Configuration functions will verify current state."
        if [[ "$cert_fingerprint" == "$current_cert_fingerprint" ]]; then
            TLS_CONFIGURATION_CURRENT=true
        fi
        # Don't exit early - the init functions already check and skip if configured
    else
        log "Info: status file does not exist. Continue"
    fi
}

# Wait for current pod ready

info "Start configuring MarkLogic for $HOST_FQDN"
info "Bootstrap host: $MARKLOGIC_BOOTSTRAP_HOST"

# Only do this if the bootstrap host is in the statefulset we are configuring
if [[ "$IS_BOOTSTRAP_HOST" == "true" ]]; then
    check_status_file_for_boostrap
    init_marklogic $HOST_FQDN
    if [[ "${MARKLOGIC_CLUSTER_TYPE}" == "bootstrap" ]]; then
        log "Info:  bootstrap host is ready"
        init_security_db
        activate_operator_credentials
        retry 5 configure_group
    else 
        log "Info:  bootstrap host is ready"
        activate_operator_credentials
        retry 5 configure_group
        join_cluster $HOST_FQDN
    fi
    configure_path_based_routing
else 
    check_status_file_for_nonbootstrap
    wait_bootstrap_ready
    activate_operator_credentials
    if local_host_uses_shared_security; then
        info "Local shared Security authentication succeeded. Skipping host initialization."
    else
        response_code=$(bootstrap_host_registration_status "$HOST_FQDN")
        case "$response_code" in
            404)
                init_marklogic "$HOST_FQDN"
                ;;
            200)
                error "Bootstrap already contains ${HOST_FQDN}, but the local host does not use shared Security. Refusing to reinitialize the static host and change its identity; recover or remove the stale host registration before retrying." exit
                ;;
            401)
                error "Failed to inspect the bootstrap host registration: credential rejected. Exit." exit
                ;;
            *)
                error "Failed to inspect the bootstrap host registration: HTTP ${response_code}. Exit." exit
                ;;
        esac
    fi
    join_cluster $HOST_FQDN
fi

if [[ $MARKLOGIC_JOIN_TLS_ENABLED == "true" ]]; then
    if [[ "$TLS_CONFIGURATION_CURRENT" == "true" ]]; then
        log "Info: TLS configuration matches the persisted certificate. Skipping configuration."
    else
        log "configuring tls"
        configure_tls
    fi
fi

set_status_file

info "cluster configuration completed"
