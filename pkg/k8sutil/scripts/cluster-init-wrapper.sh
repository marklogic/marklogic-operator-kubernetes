#!/bin/bash
# Copyright (c) 2024-2026 Progress Software Corporation and/or its subsidiaries or affiliates. All Rights Reserved.

# Combined Wrapper: Istio Resilience + Robust Signal Handling

# --- Safety: Reset Readiness State ---
# Force probe failure immediately on startup to prevent false positives
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
    
    # Wait for the actual database process to exit
    if [ -n "$REAL_ML_PID" ]; then
        wait "$REAL_ML_PID" 2>/dev/null || true
    fi
    
    echo "[Wrapper] Shutdown complete."
    exit 0
}

# Trap signals: Forward SIGTERM and SIGINT to our handler
trap 'shutdown_handler' SIGTERM SIGINT

# --- Patch Vendor Script (The "Zombie" Fix) ---
echo "[Wrapper] Patching vendor script to remove blocking tail..."
# Copy to writable location since /usr/local/bin may be read-only (rootless containers)
cp /usr/local/bin/start-marklogic.sh /tmp/start-marklogic-patched.sh
if [ $? -ne 0 ]; then
    echo "[Wrapper] ERROR: Failed to copy vendor script."
    exit 1
fi

# Remove 'tail -f /dev/null' with flexible whitespace matching
sed -i 's/tail[[:space:]][[:space:]]*-f[[:space:]][[:space:]]*\/dev\/null//g' /tmp/start-marklogic-patched.sh
if [ $? -ne 0 ]; then
    echo "[Wrapper] ERROR: Failed to patch vendor script."
    exit 1
fi

# Make the patched script executable
chmod +x /tmp/start-marklogic-patched.sh

# --- Phase 1: Background Application Startup ---
echo "[Wrapper] Starting MarkLogic vendor script..."

# Run the patched vendor script in the background
/tmp/start-marklogic-patched.sh &
SCRIPT_PID=$!

# Wait for the vendor script to finish its setup
wait $SCRIPT_PID
VENDOR_EXIT_CODE=$?

if [ $VENDOR_EXIT_CODE -ne 0 ]; then
    echo "[Wrapper] ERROR: Vendor script failed with exit code $VENDOR_EXIT_CODE"
    exit 1
fi

# --- Phase 2: Capture Real PID ---
PID_FILE="${MARKLOGIC_PID_FILE:-/var/run/MarkLogic.pid}"

echo "[Wrapper] Waiting for MarkLogic PID file..."
count=0
until [ -f "$PID_FILE" ]; do
    sleep 1
    count=$((count+1))
    if [ $count -ge 30 ]; then
        echo "[Wrapper] ERROR: MarkLogic failed to start (No PID file found)."
        exit 1
    fi
done

REAL_ML_PID=$(cat "$PID_FILE")
echo "[Wrapper] MarkLogic is running with PID: $REAL_ML_PID"

# --- Phase 3: Local Readiness Gate ---
echo "[Wrapper] Waiting for local socket (localhost:8001)..."
until curl -s localhost:8001 > /dev/null; do 
    if ! kill -0 "$REAL_ML_PID" 2>/dev/null; then
         echo "[Wrapper] ERROR: MarkLogic process died during local startup."
         exit 1
    fi
    sleep 2
done
echo "[Wrapper] Localhost is UP."

# --- Phase 4: Istio Ambient Network Gatekeeper ---
if [[ -n "$MARKLOGIC_BOOTSTRAP_HOST" ]] && [[ "$HOSTNAME" != *"$MARKLOGIC_BOOTSTRAP_HOST"* ]]; then
    echo "[Wrapper] Checking mesh connectivity to Bootstrap Host: $MARKLOGIC_BOOTSTRAP_HOST..."
    MAX_RETRIES=60
    count=0
    until curl -s -o /dev/null -m 2 "http://${MARKLOGIC_BOOTSTRAP_HOST}:8001/"; do
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
    echo "[Wrapper] No init script found (/tmp/helm-scripts/cluster-config.sh). Skipping."
fi

# --- Phase 6: Signal Readiness ---
touch /tmp/wrapper_ready

# --- Phase 7: Process Guardian ---
echo "[Wrapper] Initialization complete. Monitoring main process (PID $REAL_ML_PID)..."

wait "$REAL_ML_PID" 2>/dev/null
EXIT_CODE=$?

# Propagate any non-zero exit code from MarkLogic process
# This triggers Kubernetes pod restart for crash recovery
if [ $EXIT_CODE -ne 0 ]; then
    echo "[Wrapper] MarkLogic process exited with code $EXIT_CODE"
    exit $EXIT_CODE
fi

echo "[Wrapper] MarkLogic process terminated normally."
exit 0