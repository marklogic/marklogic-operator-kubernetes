#!/bin/bash
# Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

MARKLOGIC_OPERATOR_USERNAME="marklogic-kubernetes-operator"
MARKLOGIC_OPERATOR_PASSWORD=""
if [[ -s /run/secrets/ml-operator-secrets/password ]]; then
    MARKLOGIC_OPERATOR_PASSWORD="$(< /run/secrets/ml-operator-secrets/password)"
fi

log () {
    local TIMESTAMP=$(date +"%Y-%m-%d %T.%3N")
    echo "${TIMESTAMP} $@" > /proc/1/fd/1
}

log "Info: [prestop] Prestop Hook Execution"

my_host=$(hostname -f)

HTTP_PROTOCOL="http"
MANAGE_HOST="localhost"
HTTPS_OPTION=""
if [[ "$MARKLOGIC_JOIN_TLS_ENABLED" == "true" ]]; then
    HTTP_PROTOCOL="https"
    if [[ -s "/run/secrets/marklogic-certs/cacert.pem" ]]; then
        HTTPS_OPTION="--cacert /run/secrets/marklogic-certs/cacert.pem"
        MANAGE_HOST="$my_host"
    else
        HTTPS_OPTION="-k"
    fi
fi
log "Info: [prestop] MarkLogic Pod Hostname: "$my_host

if [[ -z "${MARKLOGIC_OPERATOR_PASSWORD}" ]]; then
    log "Info: [prestop] Operator credentials are unavailable; skipping authenticated shutdown request."
    exit 0
fi

request_shutdown() {
    local username="$1"
    local password="$2"
    curl --anyauth --user "${username}:${password}" \
        -o /dev/null -m 10 -s -w %{http_code} \
        -i -X POST ${HTTPS_OPTION} --data "state=shutdown&failover=true" \
        -H "Content-type: application/x-www-form-urlencoded" \
        ${HTTP_PROTOCOL}://${MANAGE_HOST}:8002/manage/v2/hosts/$my_host?format=json
}

for ((i = 0; i < 5; i = i + 1)); do
    res_code=$(request_shutdown "${MARKLOGIC_OPERATOR_USERNAME}" "${MARKLOGIC_OPERATOR_PASSWORD}")

    if [[ "${res_code}" == "202" ]]; then
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