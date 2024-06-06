#!/bin/bash

MARKLOGIC_ADMIN_USERNAME="$(< /run/secrets/ml-secrets/username)"            
MARKLOGIC_ADMIN_PASSWORD="$(< /run/secrets/ml-secrets/password)"
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