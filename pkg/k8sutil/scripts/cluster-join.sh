#!/bin/bash
# Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

# Return ordinary shell success/failure, never an HTTP status modulo 256.
# Response bodies stay in private files and are not printed to the log.
cluster_join_request() {
    local expected=$1 output=$2 url=$3 attempt code transport
    shift 3
    for ((attempt = 1; attempt <= N_RETRY; attempt++)); do
        transport=0
        code=$(curl -sS --connect-timeout 5 --max-time 30 -o "$output" \
            -w '%{http_code}' "$@" "$url") || transport=$?
        if [[ $transport -eq 0 && "$code" == "$expected" ]]; then
            return 0
        fi
        info "Cluster join request failed: HTTP ${code:-000}, transport=$transport, expected=$expected, attempt=$attempt/$N_RETRY"
        # Retry transport failures and transient HTTP errors only.
        if [[ "$code" == 404 && "${CLUSTER_JOIN_RETRY_NOT_FOUND:-false}" == true ]]; then
            : # Another group bootstrap may still be creating the target group.
        elif [[ $transport -eq 0 && "$code" != "000" && "$code" != "408" \
              && "$code" != "429" && ! "$code" =~ ^5[0-9][0-9]$ ]]; then
            return 1
        fi
        if ((attempt < N_RETRY)); then sleep "$RETRY_INTERVAL"; fi
    done
    return 1
}

# Print the text of every <tag>value</tag> element, one per line (namespace prefixes allowed).
cluster_join_xml_values() {
    local tag=$1 file=$2
    [[ -r "$file" ]] || return 1
    tr -d '\r\n' < "$file" | grep -oE "<([A-Za-z0-9_-]+:)?${tag}>[^<]*</([A-Za-z0-9_-]+:)?${tag}>" | sed -E 's/<[^>]*>//g'
    return 0
}

# Classify only positive local configuration evidence. A failed HTTP request
# does not imply an unjoined host and must never authorize a rejoin.
cluster_join_local_state() {
    local bootstrap_id=$1 hostname=$2 hosts_file=$3 count own target
    [[ -r "$hosts_file" ]] || return 1
    count=$(cluster_join_xml_values host-id "$hosts_file" | wc -l | tr -d ' ')
    own=$(cluster_join_xml_values host-name "$hosts_file" | grep -Fxc -- "$hostname") || true
    target=$(cluster_join_xml_values host-id "$hosts_file" | grep -Fxc -- "$bootstrap_id") || true
    if [[ "$own" == 1 && "$target" == 1 ]]; then
        echo joined
    elif [[ "$count" == 1 && "$own" == 1 && "$target" == 0 ]]; then
        echo standalone
    else
        return 1
    fi
}

# Join has two separate commits: registration on bootstrap and installation
# of the returned ZIP on the joining host. Recover a partial join by repeating
# this supported API sequence with the same host identity; never delete it.
join_cluster_checked() (
    local hostname=$1 work bootstrap_id local_state response_code timestamp
    local hosts_file="${MARKLOGIC_DATA_DIR:-/var/opt/MarkLogic}/hosts.xml"
    [[ "$hostname" =~ ^[a-zA-Z0-9_.-]+$ ]] || { error "Invalid join hostname"; return 1; }
    umask 077
    work=$(mktemp -d) || return 1
    trap 'rm -rf -- "$work"' EXIT
    local tls=()
    [[ "$HTTPS_OPTION" == "-k" ]] && tls=(-k)
    local auth=(--anyauth --user "${MARKLOGIC_ADMIN_USERNAME}:${MARKLOGIC_ADMIN_PASSWORD}")

    cluster_join_request 200 "$work/bootstrap.xml" "$HTTP_PROTOCOL://${MARKLOGIC_BOOTSTRAP_HOST}:8001/admin/v1/server-config" \
        "${auth[@]}" "${tls[@]}" || { error "Cannot verify bootstrap identity"; return 1; }
    bootstrap_id=$(cluster_join_xml_values host-id "$work/bootstrap.xml" | head -n 1)
    [[ "$bootstrap_id" =~ ^[0-9]+$ ]] || { error "Invalid bootstrap identity response"; return 1; }
    local_state=$(cluster_join_local_state "$bootstrap_id" "$hostname" "$hosts_file") || {
        error "Local membership is missing, invalid, or belongs to another cluster; refusing to overwrite it"
        return 1
    }
    if [[ "$local_state" == joined ]]; then
        cluster_join_request 200 "$work/registered.xml" "$HTTP_PROTOCOL://${MARKLOGIC_BOOTSTRAP_HOST}:8002/manage/v2/hosts/${hostname}/properties?format=xml" \
            "${auth[@]}" "${tls[@]}" || { error "Local and bootstrap membership do not agree"; return 1; }
        info "Host membership verified locally and on bootstrap; skipping join"
        return 0
    fi

    info "Local host has not completed cluster join; completing registration and configuration installation"
    CLUSTER_JOIN_RETRY_NOT_FOUND=true cluster_join_request 200 "$work/group.xml" "$HTTP_PROTOCOL://${MARKLOGIC_BOOTSTRAP_HOST}:8002/manage/v2/groups/${MARKLOGIC_GROUP}" \
        "${auth[@]}" "${tls[@]}" || { error "Target group is unavailable"; return 1; }
    cluster_join_request 200 "$work/host.xml" "http://localhost:8001/admin/v1/server-config" \
        "${auth[@]}" -H 'Accept: application/xml' || { error "Cannot export joining host configuration"; return 1; }
    cluster_join_request 200 "$work/cluster.zip" "$HTTP_PROTOCOL://${MARKLOGIC_BOOTSTRAP_HOST}:8001/admin/v1/cluster-config" \
        "${auth[@]}" "${tls[@]}" -X POST --data-urlencode "group=${MARKLOGIC_GROUP}" \
        --data-urlencode "server-config@$work/host.xml" -H 'Content-type: application/x-www-form-urlencoded' || {
        error "Bootstrap registration/configuration download failed; local installation was not attempted"
        return 1
    }
    if [[ "$(od -An -tx1 -N4 "$work/cluster.zip" | tr -d ' \n')" != "504b0304" ]]; then
        error "Bootstrap did not return a ZIP configuration; refusing to install the response"
        return 1
    fi
    cluster_join_request 200 "$work/timestamp" "http://localhost:8001/admin/v1/timestamp" \
        "${auth[@]}" || { error "Cannot read pre-join startup timestamp"; return 1; }
    timestamp=$(cat "$work/timestamp")
    cluster_join_request 202 "$work/install-response" "http://localhost:8001/admin/v1/cluster-config" \
        -X POST -H 'Content-type: application/zip' --data-binary "@$work/cluster.zip" || {
        error "Local cluster configuration installation failed; join remains incomplete"
        return 1
    }
    restart_check localhost "$timestamp"
    local_state=$(cluster_join_local_state "$bootstrap_id" "$hostname" "$hosts_file") || return 1
    [[ "$local_state" == joined ]] || { error "Restart completed without installing target cluster membership"; return 1; }
    info "Cluster join completed and local membership verified"
)
