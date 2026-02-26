#!/bin/bash
# Copyright (c) 2024-2025 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

# Combined Wrapper: Istio Resilience + Rootless Support + Auto-Restart Handling

# --- Safety: Reset Readiness State ---
rm -f /tmp/wrapper_ready

# --- Define Graceful Shutdown Handler ---
shutdown_handler() {
    echo "[Wrapper] SIGTERM received. Shutting down MarkLogic gracefully..."
    
    # Trigger the standard stop script
    if [ -f "/etc/init.d/MarkLogic" ]; then
        /etc/init.d/MarkLogic stop
    else
        /etc/MarkLogic/MarkLogic-service.sh stop
    fi
    
    # Wait for the REAL database process to exit
    if [ -n "$REAL_ML_PID" ]; then
        echo "[Wrapper] Waiting for MarkLogic (PID $REAL_ML_PID) to stop..."
        for i in {1..30}; do
            if ! kill -0 "$REAL_ML_PID" 2>/dev/null; then
                echo "[Wrapper] Process exited."
                break
            fi
            sleep 1
        done
    fi
    
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
until curl -s -m 2 localhost:8001 > /dev/null; do 
    if ! kill -0 "$REAL_ML_PID" 2>/dev/null; then
         echo "[Wrapper] ERROR: MarkLogic process died during local startup."
         exit 1
    fi
    startup_count=$((startup_count+1))
    if [ $startup_count -ge $MAX_STARTUP_WAIT ]; then
        echo "[Wrapper] WARNING: Timeout waiting for localhost:8001 after $MAX_STARTUP_WAIT attempts."
        echo "[Wrapper] This may indicate stale cluster configuration. Checking for status file..."
        ML_KUBERNETES_FILE_PATH="/var/opt/MarkLogic/Kubernetes"
        if [[ -f "$ML_KUBERNETES_FILE_PATH/status.txt" ]]; then
            echo "[Wrapper] Found stale status file. Removing and restarting MarkLogic..."
            rm -f "$ML_KUBERNETES_FILE_PATH/status.txt"
            # Trigger restart by stopping MarkLogic
            if [ -f "/etc/init.d/MarkLogic" ]; then
                /etc/init.d/MarkLogic stop
            else
                /etc/MarkLogic/MarkLogic-service.sh stop
            fi
            # Wait for process to stop
            for i in {1..30}; do
                if ! kill -0 "$REAL_ML_PID" 2>/dev/null; then
                    break
                fi
                sleep 1
            done
            # Start MarkLogic again
            if [ -f "/etc/init.d/MarkLogic" ]; then
                /etc/init.d/MarkLogic start
            else
                /etc/MarkLogic/MarkLogic-service.sh start
            fi
            # Update PID
            sleep 5
            if [ -f "$PID_FILE" ]; then
                REAL_ML_PID=$(cat "$PID_FILE")
                echo "[Wrapper] MarkLogic restarted with new PID: $REAL_ML_PID"
            else
                echo "[Wrapper] ERROR: Failed to restart MarkLogic"
                exit 1
            fi
            # Reset counter to give it another chance
            startup_count=0
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
    until curl -s -o /dev/null -m 2 "http://${MARKLOGIC_BOOTSTRAP_HOST}:8001/"; do
        # Safety Check: Did MarkLogic die?
        if ! kill -0 "$REAL_ML_PID" 2>/dev/null; then
             echo "[Wrapper] ERROR: MarkLogic process died during network wait."
             exit 1
        fi
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

# --- Phase 5.5: Stabilization & PID Refresh (CRITICAL FIX) ---
# MarkLogic often restarts itself after cluster-config updates (e.g., security DB install).
# If we don't refresh the PID, the wrapper will think the old PID dying is a crash.
echo "[Wrapper] Cluster configuration complete. Stabilizing..."
sleep 5

if [ -f "$PID_FILE" ]; then
    NEW_ML_PID=$(cat "$PID_FILE")
    if [ "$NEW_ML_PID" != "$REAL_ML_PID" ]; then
         echo "[Wrapper] NOTICE: MarkLogic PID changed during config ($REAL_ML_PID -> $NEW_ML_PID)."
         REAL_ML_PID=$NEW_ML_PID
    fi
else
    echo "[Wrapper] ERROR: PID file missing after config!"
    exit 1
fi

# --- Phase 6: Signal Readiness ---
touch /tmp/wrapper_ready

# --- Phase 7: The "Dual" Watchdog ---
echo "[Wrapper] Initialization complete. Monitoring MarkLogic (PID $REAL_ML_PID)..."

while true; do
    # 1. Check if the MarkLogic Database is alive
    if ! kill -0 "$REAL_ML_PID" 2>/dev/null; then
        echo "[Wrapper] CRITICAL: MarkLogic Database (PID $REAL_ML_PID) crashed!"
        echo "[Wrapper] Terminating vendor script to trigger Pod restart..."
        kill -TERM "$SCRIPT_PID" 2>/dev/null
        exit 1
    fi
    
    # 2. Check if the Vendor Script is alive (unlikely to die, but good to check)
    if ! kill -0 "$SCRIPT_PID" 2>/dev/null; then
         echo "[Wrapper] ERROR: Vendor script exited unexpectedly."
         exit 1
    fi
    
    sleep 5
done