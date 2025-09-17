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
    message="${TIMESTAMP} [postStart] $@"
    echo $message >> /proc/1/fd/1
    echo $message >> /tmp/script.log
}


MARKLOGIC_ADMIN_USERNAME="$(< /run/secrets/ml-secrets/username)"
MARKLOGIC_ADMIN_PASSWORD="$(< /run/secrets/ml-secrets/password)"

if [ ! -f /var/tokens/cluster-api-token ]; then
    error "Token file not found at /var/tokens/cluster-api-token" 
fi
CLUSTER_API_TOKEN="$(< /var/tokens/cluster-api-token)"

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

###############################################################


function join_dynamic_host {
    info "Attempting to join cluster using dynamic host token"
    
    response=$(curl --anyauth -H "Content-type:application/xml" \
        -d "<init xmlns=\"http://marklogic.com/manage\"><dynamic-host-token>$CLUSTER_API_TOKEN</dynamic-host-token></init>" \
        -X POST \
        -w "%{http_code}" \
        -s \
        http://localhost:8001/admin/v1/init)
    
    response_code=$(echo "$response" | tail -n1)
    response_body=$(echo "$response" | sed '$ d')
    
    info "HTTP Response Code: $response_code"
    info "Response Body: $response_body"
    
    if [ "$response_code" = "202" ]; then
        info "Successfully joined cluster"
        info "Response: $response_body"
    else
        error "Join failed with code $response_code: $response_body" 
    fi
}

info "Waiting for bootstrap host to be ready: $MARKLOGIC_BOOTSTRAP_HOST"
info "Using admin user: $MARKLOGIC_ADMIN_USERNAME"
info "Using admin password: $MARKLOGIC_ADMIN_PASSWORD"
info "checking cluster-api-token file existence"
sleep 5s
info "Cluster API Token: $CLUSTER_API_TOKEN"
join_dynamic_host
exit 0