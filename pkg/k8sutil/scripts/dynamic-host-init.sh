#! /bin/bash

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
    message="${TIMESTAMP} [initContainer] $@"
    echo $message >> /proc/1/fd/1
    echo $message >> /tmp/script.log
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

    for ((retry_count = 0; retry_count < 5; retry_count = retry_count + 1)); do
        response=$(curl -v -m 30 -w '%{http_code}' "${curl_options[@]}" "$endpoint")
        response_code=$(tail -n1 <<< "$response")
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

MARKLOGIC_ADMIN_USERNAME="$(< /run/secrets/ml-secrets/username)"
MARKLOGIC_ADMIN_PASSWORD="$(< /run/secrets/ml-secrets/password)"

function wait_bootstrap_ready {
    info "Calling Bootstrap host at http://$MARKLOGIC_BOOTSTRAP_HOST:8001/admin/v1/timestamp"
    resp=$(curl -w '%{http_code}' -o /dev/null http://$MARKLOGIC_BOOTSTRAP_HOST:8001/admin/v1/timestamp )
    if [[ "$MARKLOGIC_JOIN_TLS_ENABLED" == "true" ]]; then
        # return 403 if tls is enabled
        if [[ $resp -eq 403 ]]; then
            info "Bootstrap host is ready with TLS enabled"
        else
            info "Calling Bootstrap host with response code:$resp. Bootstrap host is not ready with TLS enabled, try again in 10s"
            sleep 10s
            wait_bootstrap_ready
            return 0
        fi
    else
        if [[ $resp -eq 401 ]]; then
            info "Bootstrap host is ready with no TLS"
        else
            info "Calling Bootstrap host with response code:$resp. Bootstrap host is not ready, try again in 10s"
            sleep 10s
            wait_bootstrap_ready
            return 0
        fi
    fi
}

function allow_dynamic_host {
    info "configure bootstrap host to allow dynamic host"
    curl_retry_validate true "http://$MARKLOGIC_BOOTSTRAP_HOST:8002/manage/v2/groups/Default/properties" 204 \
    --anyauth -u "$MARKLOGIC_ADMIN_USERNAME:$MARKLOGIC_ADMIN_PASSWORD" -X PUT \
    -H "Content-type: application/json" -d '{"allow-dynamic-hosts":true}' 
    return_code=$?
    if [[ $return_code -ne 204 ]]; then
        error "Failed to configure bootstrap host to allow dynamic host, response code: $return_code" exit
    fi

    info "Continue to Configure the group to allow API-token-authentication"

    curl_retry_validate true "http://$MARKLOGIC_BOOTSTRAP_HOST:8002/manage/v2/servers/Admin/properties?group-id=Default" 204 \
    -X PUT --anyauth -u "$MARKLOGIC_ADMIN_USERNAME:$MARKLOGIC_ADMIN_PASSWORD" \
    -H "Content-type: application/json" -d '{"API-token-authentication":true}'
    return_code=$?
    if [[ $return_code -ne 204 ]]; then
        error "Failed to configure the group to allow API-token-authentication, response code: $return_code" exit
    fi
    info "Successfully configured the group to allow API-token-authentication"

    info "Successfully configured bootstrap host to allow dynamic host"
}

function fetch_token {
    info "Fetch API token from bootstrap host"
    xml_response=$(curl --anyauth -s -u "$MARKLOGIC_ADMIN_USERNAME:$MARKLOGIC_ADMIN_PASSWORD" \
        -H "Content-Type: application/json" \
        -d '{"dynamic-host-token": {"group": "Default","host": "dnode-0.dnode.default.svc.cluster.local", "port":8001, "duration": "PT15M","comment": "genereate from Kubernetes Operator"}}' \
        -X POST "http://$MARKLOGIC_BOOTSTRAP_HOST:8002/manage/v2/clusters/Default/dynamic-host-token")
    info "Response from bootstrap host: $token_response"
        if [[ $? -ne 0 ]]; then
        error "Failed to fetch API token from bootstrap host" 
    fi

    api_token=$(echo "$xml_response" | \
        echo "$xml_response" | sed -n 's/.*<dynamic-host-token[^>]*>\(.*\)<\/dynamic-host-token>.*/\1/p')

    if [ -z "$api_token" ]; then
        error "Failed to extract token from response"
        error "Response was: $xml_response"
    else
        info "Extracted token: $api_token"
    fi

    echo "$api_token" > /var/tokens/cluster-api-token
    chmod 644 /var/tokens/cluster-api-token

    # api_token=$(echo $token_response | jq -r '.token')
    # if [[ -z "$api_token" || "$api_token" == "null" ]]; then
    #     error "API token is empty or null" exit
    # fi
    # echo $api_token > /tmp/ml-api-token
    info "Successfully fetched API token from bootstrap host"
}


info "Waiting for bootstrap host to be ready: $MARKLOGIC_BOOTSTRAP_HOST"
info "Bootstrap host: $MARKLOGIC_BOOTSTRAP_HOST"
info "MARKLOGIC_ADMIN_USERNAME: $MARKLOGIC_ADMIN_USERNAME"
info "MARKLOGIC_ADMIN_PASSWORD: $MARKLOGIC_ADMIN_PASSWORD"
wait_bootstrap_ready
allow_dynamic_host
info "Dynamic host initialization script completed"
fetch_token
info "API token stored in /var/tokens/cluster-api-token"
exit 0