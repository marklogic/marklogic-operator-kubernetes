#!/bin/bash
# Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

# Combined Wrapper: Istio Resilience + Rootless Support + Auto-Restart Handling

# --- Safety: Reset Readiness State ---
rm -f /tmp/marklogic_ready

# --- Port Liveness Helper ---
# Checks if MarkLogic port 8001 is accepting connections.
# Uses -k to ignore TLS cert errors and tries both http and https so this
# works before AND after cluster-config.sh enables TLS on the Admin port.
# Returns 0 for any HTTP response (including 401 Unauthorized), non-zero
# only when the connection is refused (i.e. the process is truly down).
ml_port_open() {
    curl -s -k -m 3 -o /dev/null http://localhost:8001 2>/dev/null || \
    curl -s -k -m 3 -o /dev/null https://localhost:8001 2>/dev/null
}

# --- Define Graceful Shutdown Handler ---
shutdown_handler() {
    echo "[Wrapper] SIGTERM received. Shutting down MarkLogic gracefully..."
    
    # Trigger the standard stop script
    if [ -f "/etc/init.d/MarkLogic" ]; then
        /etc/init.d/MarkLogic stop
    else
        /etc/MarkLogic/MarkLogic-service.sh stop
    fi
    
    # Wait for MarkLogic to stop by watching port 8001 close.
    # Note: kill -0 is not used here - EPERM is indistinguishable from ESRCH in
    # rootless containers, causing immediate false exit before data is flushed.
    echo "[Wrapper] Waiting for MarkLogic to stop..."
    for i in {1..30}; do
        if ! ml_port_open; then
            echo "[Wrapper] Port 8001 closed. Process exited."
            break
        fi
        sleep 1
    done
    
    exit 0
}

trap 'shutdown_handler' SIGTERM SIGINT

# --- Phase 1: Background Application Startup ---
echo "[Wrapper] Starting MarkLogic vendor script..."
# We run the ORIGINAL script. It will hang on 'tail -f'. That is fine.
# This avoids permission issues in rootless containers.
/usr/local/bin/start-marklogic.sh &
SCRIPT_PID=$!
echo "[Wrapper] Vendor script started with PID: $SCRIPT_PID"

# --- Phase 2: Capture Real PID (The "Zombie" Fix) ---
# Even though the script is monitoring 'tail', MarkLogic writes its PID to a file.
# We MUST capture this to know if the DB actually crashes.
PID_FILE="${MARKLOGIC_PID_FILE:-/var/run/MarkLogic.pid}"

echo "[Wrapper] Waiting for MarkLogic PID file..."
count=0
until [ -f "$PID_FILE" ]; do
    # Check if the vendor script crashed early
    if ! kill -0 "$SCRIPT_PID" 2>/dev/null; then
        echo "[Wrapper] ERROR: Vendor script died before PID file creation."
        exit 1
    fi
    sleep 1
    count=$((count+1))
    if [ $count -ge 60 ]; then
        echo "[Wrapper] ERROR: Timeout waiting for PID file."
        exit 1
    fi
done

REAL_ML_PID=$(cat "$PID_FILE")
echo "[Wrapper] MarkLogic is running with PID: $REAL_ML_PID"

# --- Phase 3: Local Readiness Gate ---
echo "[Wrapper] Waiting for local socket (localhost:8001)..."
MAX_STARTUP_WAIT=60
startup_count=0
until ml_port_open; do
    # Note: kill -0 is not used here - EPERM is indistinguishable from ESRCH in
    # rootless containers (MarkLogic runs as different user). Rely on timeout instead.
    startup_count=$((startup_count+1))
    if [ $startup_count -ge $MAX_STARTUP_WAIT ]; then
        echo "[Wrapper] WARNING: Timeout waiting for localhost:8001 after $MAX_STARTUP_WAIT attempts."
        echo "[Wrapper] This may indicate stale cluster configuration."
        ML_KUBERNETES_FILE_PATH="/var/opt/MarkLogic/Kubernetes"
        if [[ -f "$ML_KUBERNETES_FILE_PATH/status.txt" ]]; then
            echo "[Wrapper] Found stale status file. Removing to force reconfiguration..."
            rm -f "$ML_KUBERNETES_FILE_PATH/status.txt"
            echo "[Wrapper] Exiting to let Kubernetes restart the pod cleanly..."
            exit 1
        else
            echo "[Wrapper] ERROR: Timeout waiting for localhost:8001 and no status file found."
            exit 1
        fi
    fi
    sleep 2
done
echo "[Wrapper] Localhost is UP."

# --- Phase 4: Istio Ambient Network Gatekeeper ---
# Skip mesh connectivity gate on the bootstrap pod
if [[ "$MARKLOGIC_CLUSTER_TYPE" == "non-bootstrap" ]]; then
    echo "[Wrapper] Checking mesh connectivity to Bootstrap Host: $MARKLOGIC_BOOTSTRAP_HOST..."
    MAX_RETRIES=60
    count=0
    until curl -s -k -o /dev/null -m 2 "http://${MARKLOGIC_BOOTSTRAP_HOST}:8001/" 2>/dev/null || \
          curl -s -k -o /dev/null -m 2 "https://${MARKLOGIC_BOOTSTRAP_HOST}:8001/" 2>/dev/null; do
        # Note: Both http and https are tried (-k ignores cert errors) so this works
        # whether or not the Bootstrap node has already enabled TLS via cluster-config.sh.
        # kill -0 is not used here - EPERM is indistinguishable from ESRCH in
        # rootless containers (MarkLogic runs as different user). Rely on timeout instead.
        count=$((count+1))
        if [ $count -ge $MAX_RETRIES ]; then
            echo "[Wrapper] WARNING: Network check timed out. Proceeding with risk..."
            break
        fi
        echo "[Wrapper] Waiting for mesh network... ($count/$MAX_RETRIES)"
        sleep 2
    done
    echo "[Wrapper] Mesh Network is Ready."
fi

# --- Phase 5: Cluster Initialization ---
echo "[Wrapper] Executing Cluster Init/Join Logic..."
if [ -f "/tmp/helm-scripts/cluster-config.sh" ]; then
    /bin/bash /tmp/helm-scripts/cluster-config.sh
    if [ $? -ne 0 ]; then
        echo "[Wrapper] ERROR: Initialization failed!"
        exit 1
    fi
else
    echo "[Wrapper] No init script found. Skipping."
fi

# --- Phase 5.5: Stability Monitor ---
# MarkLogic restarts itself after cluster-config changes (security DB, XDQP SSL group config).
# Instead of tracking PIDs (unreliable), we use port 8001 availability as the health signal.
# We require N consecutive successful port checks before proceeding.
echo "[Wrapper] Cluster configuration complete. Stabilizing..."

# Check vendor script status
if ! kill -0 "$SCRIPT_PID" 2>/dev/null; then
    echo "[Wrapper] WARNING: Vendor script (PID $SCRIPT_PID) has died! This should not happen."
else
    echo "[Wrapper] Vendor script (PID $SCRIPT_PID) is alive."
fi

echo "[Wrapper] Monitoring MarkLogic stability after cluster configuration (port-based)..."
echo "[Wrapper] Current PID: $REAL_ML_PID"

STABLE_REQUIRED=8    # consecutive successful port checks needed (8 × 3s = 24 sec stable)
STABLE_COUNT=0
PORT_WAS_DOWN=false
MAX_TOTAL_WAIT=120   # seconds total (10 min equivalent in checks below)
TOTAL_CHECKS=$((MAX_TOTAL_WAIT / 3))

for stability_check in $(seq 1 $TOTAL_CHECKS); do
    sleep 3

    if ml_port_open; then
        # Port is up
        if [ "$PORT_WAS_DOWN" = true ]; then
            echo "[Wrapper] Port 8001 recovered after restart - refreshing PID..."
            PORT_WAS_DOWN=false
            STABLE_COUNT=0
            # Refresh PID from file
            if [ -f "$PID_FILE" ]; then
                NEW_PID=$(cat "$PID_FILE")
                if [ -n "$NEW_PID" ] && [ "$NEW_PID" != "$REAL_ML_PID" ]; then
                    echo "[Wrapper] PID refreshed: $REAL_ML_PID -> $NEW_PID"
                    REAL_ML_PID=$NEW_PID
                fi
            fi
        fi

        STABLE_COUNT=$((STABLE_COUNT + 1))
        echo "[Wrapper] Port 8001 stable (check $STABLE_COUNT/$STABLE_REQUIRED)..."

        if [ $STABLE_COUNT -ge $STABLE_REQUIRED ]; then
            echo "[Wrapper] MarkLogic is stable. Proceeding."
            # Final PID refresh
            if [ -f "$PID_FILE" ]; then
                NEW_PID=$(cat "$PID_FILE")
                if [ -n "$NEW_PID" ] && [ "$NEW_PID" != "$REAL_ML_PID" ]; then
                    echo "[Wrapper] Final PID refresh: $REAL_ML_PID -> $NEW_PID"
                    REAL_ML_PID=$NEW_PID
                fi
            fi
            break
        fi
    else
        # Port is down - restart or startup in progress
        if [ "$PORT_WAS_DOWN" = false ]; then
            echo "[Wrapper] Port 8001 down - MarkLogic restarting..."
        fi
        PORT_WAS_DOWN=true
        STABLE_COUNT=0
    fi
done

if [ $STABLE_COUNT -lt $STABLE_REQUIRED ]; then
    echo "[Wrapper] WARNING: MarkLogic did not stabilize within timeout (stable=$STABLE_COUNT/$STABLE_REQUIRED)."
fi

# Final PID refresh from file (port-based monitoring already confirmed MarkLogic is up)
echo "[Wrapper] Final PID check..."
if [ -f "$PID_FILE" ]; then
    FINAL_PID=$(cat "$PID_FILE")
    echo "[Wrapper] PID file contains: $FINAL_PID"
    if [ -n "$FINAL_PID" ] && [ "$FINAL_PID" != "$REAL_ML_PID" ]; then
        echo "[Wrapper] Updating tracked PID: $REAL_ML_PID -> $FINAL_PID"
        REAL_ML_PID=$FINAL_PID
    fi
fi

# Verify MarkLogic is still responsive (confirming stability monitor result holds)
if ! ml_port_open; then
    echo "[Wrapper] FATAL: MarkLogic port 8001 not responding after stability check!"
    if [ -f "/var/opt/MarkLogic/Logs/ErrorLog.txt" ]; then
        echo "[Wrapper] Last 80 lines of ErrorLog:"
        tail -80 /var/opt/MarkLogic/Logs/ErrorLog.txt
    fi
    exit 1
fi
echo "[Wrapper] MarkLogic PID $REAL_ML_PID confirmed running and stable."

# --- Phase 5.6: Wait for MarkLogic healthcheck endpoint ---
echo "[Wrapper] Waiting for MarkLogic to be fully healthy after configuration..."
echo "[Wrapper] Current tracked PID: $REAL_ML_PID"

# Determine the healthcheck endpoint based on MarkLogic version
HEALTHCHECK_PORT="7997"
if [[ "$MARKLOGIC_VERSION" =~ "10" ]] || [[ "$MARKLOGIC_VERSION" =~ "9" ]]; then
    HEALTHCHECK_PATH=""
else
    HEALTHCHECK_PATH="/LATEST/healthcheck"
fi

# Detect protocol for healthcheck
HEALTHCHECK_PROTOCOL="http"
test_response=$(curl -s http://localhost:7997 2>&1)
if [[ "$test_response" =~ "HTTPS server using HTTP" ]]; then
    HEALTHCHECK_PROTOCOL="https"
fi

# Wait for healthcheck to return 200
echo "[Wrapper] Checking healthcheck endpoint: ${HEALTHCHECK_PROTOCOL}://localhost:${HEALTHCHECK_PORT}${HEALTHCHECK_PATH}"
health_retry=0
MAX_HEALTH_RETRIES=60
until [ $health_retry -ge $MAX_HEALTH_RETRIES ]; do
    # Use port 8001 as liveness signal (kill -0 is unreliable during heavy I/O)
    # Port-based stability was already confirmed in Phase 5.5; here we just track
    # any additional restart that may occur during health check wait.
    if ! ml_port_open; then
        echo "[Wrapper] Port 8001 down during health check (attempt $health_retry/$MAX_HEALTH_RETRIES) - MarkLogic may be restarting..."
        health_retry=$((health_retry + 1))
        sleep 2
        continue
    fi
    # Refresh PID from file in case MarkLogic restarted
    if [ -f "$PID_FILE" ]; then
        CURRENT_PID=$(cat "$PID_FILE")
        if [ -n "$CURRENT_PID" ] && [ "$CURRENT_PID" != "$REAL_ML_PID" ]; then
            echo "[Wrapper] NOTICE: PID changed during health check ($REAL_ML_PID -> $CURRENT_PID)."
            REAL_ML_PID=$CURRENT_PID
        fi
    fi

    # Check healthcheck endpoint
    HEALTH_CODE=$(curl -s -o /dev/null -w '%{http_code}' -m 5 ${HEALTHCHECK_PROTOCOL}://localhost:${HEALTHCHECK_PORT}${HEALTHCHECK_PATH} -k 2>/dev/null || echo "000")
    
    if [ "$HEALTH_CODE" == "200" ]; then
        echo "[Wrapper] MarkLogic healthcheck returned 200. Server is fully ready."
        break
    elif [ "$HEALTH_CODE" == "404" ] && [ -n "$HEALTHCHECK_PATH" ]; then
        # Try old healthcheck endpoint for upgrades
        HEALTH_CODE=$(curl -s -o /dev/null -w '%{http_code}' -m 5 ${HEALTHCHECK_PROTOCOL}://localhost:${HEALTHCHECK_PORT} -k 2>/dev/null || echo "000")
        if [ "$HEALTH_CODE" == "200" ]; then
            echo "[Wrapper] MarkLogic healthcheck (old endpoint) returned 200. Server is fully ready."
            break
        fi
    fi
    
    health_retry=$((health_retry + 1))
    if [ $health_retry -ge $MAX_HEALTH_RETRIES ]; then
        echo "[Wrapper] WARNING: Healthcheck did not return 200 after $MAX_HEALTH_RETRIES attempts (last code: $HEALTH_CODE)."
        echo "[Wrapper] Proceeding anyway, but the server may not be fully configured."
        break
    fi
    
    # Log progress every 10 attempts
    if [ $((health_retry % 10)) -eq 0 ]; then
        echo "[Wrapper] Still waiting for healthcheck... (attempt $health_retry/$MAX_HEALTH_RETRIES, last code: $HEALTH_CODE)"
    fi
    
    sleep 2
done

# Final PID confirmation
if [ -f "$PID_FILE" ]; then
    FINAL_PID=$(cat "$PID_FILE")
    if [ "$FINAL_PID" != "$REAL_ML_PID" ]; then
         echo "[Wrapper] NOTICE: Final PID refresh detected change ($REAL_ML_PID -> $FINAL_PID)."
         REAL_ML_PID=$FINAL_PID
    else
         echo "[Wrapper] PID stable at $REAL_ML_PID."
    fi
else
    echo "[Wrapper] ERROR: PID file missing before monitoring!"
    exit 1
fi

# --- Phase 6: Signal Readiness ---
touch /tmp/marklogic_ready

# --- Phase 7: Port-Based Watchdog ---
# NOTE: kill -0 cannot be used to check the MarkLogic process liveness in rootless
# containers (UBI9) because the wrapper runs as a different user than MarkLogic.
# kill -0 returns EPERM (not ESRCH) which is indistinguishable from "process dead"
# in shell. Port 8001 availability is the authoritative liveness signal instead.
echo "[Wrapper] Initialization complete. Monitoring MarkLogic via port 8001..."

PORT_DOWN_COUNT=0
PORT_DOWN_THRESHOLD=6  # 6 × 5s = 30 seconds sustained outage before declaring crash

while true; do
    sleep 5

    # 1. Check if the Vendor Script is alive
    if ! kill -0 "$SCRIPT_PID" 2>/dev/null; then
        echo "[Wrapper] ERROR: Vendor script (PID $SCRIPT_PID) exited unexpectedly."
        exit 1
    fi

    # 2. Check MarkLogic liveness via port (reliable in rootless containers)
    if ml_port_open; then
        # Port is up - MarkLogic is alive
        if [ $PORT_DOWN_COUNT -gt 0 ]; then
            echo "[Wrapper] Port 8001 recovered after $PORT_DOWN_COUNT down checks. MarkLogic restarted."
            # Refresh PID from file after a restart
            if [ -f "$PID_FILE" ]; then
                NEW_PID=$(cat "$PID_FILE")
                if [ -n "$NEW_PID" ] && [ "$NEW_PID" != "$REAL_ML_PID" ]; then
                    echo "[Wrapper] PID refreshed: $REAL_ML_PID -> $NEW_PID"
                    REAL_ML_PID=$NEW_PID
                fi
            fi
        fi
        PORT_DOWN_COUNT=0
    else
        # Port is down
        PORT_DOWN_COUNT=$((PORT_DOWN_COUNT + 1))
        echo "[Wrapper] Port 8001 down (count $PORT_DOWN_COUNT/$PORT_DOWN_THRESHOLD) - MarkLogic may be restarting..."

        if [ $PORT_DOWN_COUNT -ge $PORT_DOWN_THRESHOLD ]; then
            echo "[Wrapper] CRITICAL: MarkLogic port 8001 has been down for $((PORT_DOWN_COUNT * 5))s. Declaring crash."

            if [ -f "/var/opt/MarkLogic/Logs/ErrorLog.txt" ]; then
                echo "[Wrapper] ========== Last 50 lines of ErrorLog.txt =========="
                tail -50 /var/opt/MarkLogic/Logs/ErrorLog.txt
                echo "[Wrapper] ========== End of ErrorLog.txt =========="
            fi

            echo "[Wrapper] Disk space:"
            # Try specific path, then general df, then gracefully print unavailable
            df -h /var/opt/MarkLogic 2>/dev/null || df -h 2>/dev/null || echo "  [Unavailable: 'df' command not found]"

            echo "[Wrapper] Memory usage:"
            # Try 'free', fallback to raw kernel meminfo, then gracefully print unavailable
            free -h 2>/dev/null || head -n 5 /proc/meminfo 2>/dev/null || echo "  [Unavailable: memory tools not found]"

            echo "[Wrapper] Terminating vendor script to trigger Pod restart..."
            kill -TERM "$SCRIPT_PID" 2>/dev/null
            exit 1
        fi
    fi
done