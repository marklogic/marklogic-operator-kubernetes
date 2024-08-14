#!/bin/bash

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