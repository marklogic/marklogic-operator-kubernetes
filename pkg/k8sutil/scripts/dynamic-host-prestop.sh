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
    # If PID value is empty preStop hook logs are not recorded
    message="${TIMESTAMP} [preStop] $@"
    echo $message >> /proc/1/fd/1 2>&1
    echo $message >> /tmp/script.log
}

info "Pre-stop script started"


MARKLOGIC_ADMIN_USERNAME="$(< /run/secrets/ml-secrets/username)"
MARKLOGIC_ADMIN_PASSWORD="$(< /run/secrets/ml-secrets/password)"
HOSTNAME=$(hostname -f)

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

function fetch_marklogic_host_id {
    info "Fetching MarkLogic host ID for $HOSTNAME"

    response=$(curl --anyauth -u $MARKLOGIC_ADMIN_USERNAME:$MARKLOGIC_ADMIN_PASSWORD \
    -H "Content-type:application/xml" \
    -s \
    "http://localhost:8002/manage/v2/hosts/$HOSTNAME" )

    if [ $? -ne 0 ]; then
        error "Failed to fetch host information from MarkLogic" exit
    fi

    info "Response from MarkLogic: $response"
    # Extract host ID from XML response
    # The response typically contains: <host-default-list><list-items><list-item><idref>12345678901234567890</idref>...
    host_id=$(echo "$response" | grep '<id>' | sed -n 's/.*<id>\(.*\)<\/id>.*/\1/p')

    if [ -z "$host_id" ]; then
        error "Failed to extract host ID from response" exit
    fi

    info "Host ID found: $host_id"
    export MARKLOGIC_HOST_ID="$host_id"
    return 0
}

function delete_dynamic_host {
    info "Attempting to delete dynamic host with ID $MARKLOGIC_HOST_ID"

    response=$(curl --anyauth -u $MARKLOGIC_ADMIN_USERNAME:$MARKLOGIC_ADMIN_PASSWORD \
    -H "Content-type:application/xml" \
    -X DELETE \
    -w "%{http_code}" \
    -s \
    -d "<dynamic-hosts><dynamic-host>$MARKLOGIC_HOST_ID</dynamic-host></dynamic-hosts>" \
    "http://$MARKLOGIC_BOOTSTRAP_HOST:8002/manage/v2/clusters/Default/dynamic-hosts")

    response_code=$(echo "$response" | tail -n1)
    response_body=$(echo "$response" | sed '$ d')

    info "Delete response code: $response_code"
    info "Response body: $response_body"

    if [ "$response_code" -ne 204 ]; then
        error "Failed to delete host from MarkLogic"
    else 
        info "Host deletion accepted by MarkLogic"
    fi

    return 0
}


###############################################################

info "Pre-stop script started"
info "Using admin user: $MARKLOGIC_ADMIN_USERNAME"
info "Using admin password: $MARKLOGIC_ADMIN_PASSWORD"
info "Using hostname: $HOSTNAME"
fetch_marklogic_host_id
info "MarkLogic Host ID: $MARKLOGIC_HOST_ID"
if [ -z "$MARKLOGIC_HOST_ID" ]; then
    error "No MarkLogic Host ID found, cannot delete dynamic host" exit
fi
delete_dynamic_host
exit 0