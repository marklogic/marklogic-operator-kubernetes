# ISTIO Ambient Mode E2E Testing Setup

This document summarizes the ISTIO Ambient mode e2e test infrastructure for the MarkLogic Kubernetes Operator.

## Overview

The ISTIO Ambient mode test suite validates that the MarkLogic operator works correctly when deployed in an ISTIO service mesh using Ambient mode. This includes testing the cluster-init-wrapper.sh script's mesh connectivity checks and PID refresh logic.

## Files Created/Modified

### 1. Test Infrastructure

**test/scripts/install-istio-ambient.sh** (NEW)
- Automated ISTIO installation script
- Downloads and installs istioctl if needed
- Installs ISTIO with ambient profile
- Verifies ISTIO components are ready

**Makefile** (MODIFIED)
- Added `e2e-setup-istio-ambient` target
- Added `e2e-setup-minikube-istio` target (combines minikube + ISTIO setup)

### 2. Test Cases

**test/e2e/6_istio_ambient_test.go** (NEW)
- Complete test suite for ISTIO Ambient mode
- Test label: `type=istio-ambient`
- Namespace: `istio-test` (with `istio.io/dataplane-mode: ambient` label)

#### Test Assessments

1. **ISTIO ztunnel pods are running**
   - Verifies ISTIO Ambient data plane pods are operational

2. **MarklogicCluster pods are running with ISTIO**
   - Deploys 2-replica cluster in ISTIO mesh
   - Waits for all pods to reach Ready state (5 min timeout)

3. **Wrapper script completed all ISTIO phases**
   - Validates all 7 phases from cluster-init-wrapper.sh
   - Confirms mesh connectivity check passed
   - Checks wrapper logs for expected phase markers

4. **PID refresh logic handles restarts correctly**
   - Tests Phase 5.5 PID refresh functionality
   - Verifies PID change detection after config-triggered restarts
   - Confirms stable PID monitoring when no restart occurs

5. **Crash detection works correctly**
   - Simulates MarkLogic crash by killing daemon process
   - Verifies wrapper detects crash and triggers pod restart
   - Confirms restart counter increases

6. **MarkLogic cluster is operational**
   - Queries MarkLogic admin API
   - Validates cluster responds to requests
   - Confirms basic functionality

### 3. Test Utilities

**test/utils/utils.go** (MODIFIED)
- Added `GetPodLogs()` function for retrieving container logs
- Complements existing `ExecCmdInPod()` for log inspection

### 4. Documentation

**test/README.md** (UPDATED)
- Added ISTIO Ambient mode testing section
- Documented test execution commands
- Included prerequisites and troubleshooting guide

## Usage

### Quick Start

```bash
# Setup minikube with ISTIO Ambient mode
make e2e-setup-minikube-istio

# Run ISTIO ambient tests only
go test -v ./test/e2e -count=1 -args --labels="type=istio-ambient"

# Cleanup
make e2e-cleanup-minikube
```

### Step-by-Step

```bash
# 1. Setup minikube cluster
make e2e-setup-minikube

# 2. Install ISTIO Ambient mode
make e2e-setup-istio-ambient

# 3. Run all e2e tests (including ISTIO)
make e2e-test

# 4. Run ISTIO tests only
go test -v ./test/e2e -count=1 -args --labels="type=istio-ambient"

# 5. Cleanup
make e2e-cleanup-minikube
```

## Test Coverage

The ISTIO Ambient test suite covers:

✅ ISTIO component readiness (istiod, ztunnel)
✅ Namespace ambient labeling
✅ Pod deployment in service mesh
✅ Wrapper script Phase 1-7 execution
✅ Mesh connectivity checks (Phase 4)
✅ PID refresh after config (Phase 5.5)
✅ Crash detection and pod restart
✅ MarkLogic cluster health verification

## Integration with Wrapper Script

The test validates these wrapper script features:

### Phase 4: ISTIO Ambient Network Gatekeeper
```bash
# Test verifies this phase completes successfully
MESH_TIMEOUT=120
MESH_ATTEMPTS=60
# Checks mesh connectivity before cluster config
```

### Phase 5.5: PID Refresh Logic
```bash
# Test verifies PID refresh detects restarts
if [ ! -f /var/run/MarkLogic.pid ]; then
  log "ERROR: PID file missing after cluster-config.sh"
  exit 1
fi
REAL_ML_PID=$(cat /var/run/MarkLogic.pid)
```

### Phase 7: Dual Process Monitoring
```bash
# Test verifies both vendor script and MarkLogic daemon are monitored
while true; do
  if ! kill -0 "$VENDOR_SCRIPT_PID" 2>/dev/null; then
    log "ERROR: Vendor script died"
    exit 1
  fi
  if ! kill -0 "$REAL_ML_PID" 2>/dev/null; then
    log "ERROR: MarkLogic process died"
    exit 1
  fi
  sleep 5
done
```

## Expected Test Results

When running successfully, you should see:

```
=== RUN   TestIstioAmbientMode
=== RUN   TestIstioAmbientMode/ISTIO_Ambient_Mode_Test
    Creating namespace with ISTIO ambient mode enabled
    Verifying ISTIO components are ready
    Creating MarklogicCluster in ISTIO ambient mesh
    ✓ ztunnel pod ztunnel-xxxxx is running
    ✓ Found: Phase 1: Starting vendor script in background
    ✓ Found: Phase 2: Capturing MarkLogic PID
    ✓ Found: Phase 3: Waiting for localhost:8001
    ✓ Found: Phase 4: ISTIO ambient network gatekeeper
    ✓ ISTIO mesh connectivity check passed
    ✓ Found: Phase 5: Cluster initialization via cluster-config.sh
    ✓ Found: Phase 6: Signal readiness
    ✓ Found: Phase 7: Monitoring processes
    ✓ PID refresh phase found
    ✓ Pod restarted correctly after crash (restarts: 0 -> 1)
    ✓ MarkLogic is responding to queries
--- PASS: TestIstioAmbientMode (450.23s)
```

## Troubleshooting

### ISTIO Installation Fails

```bash
# Check minikube status
minikube status

# Manually install ISTIO
curl -L https://istio.io/downloadIstio | ISTIO_VERSION=1.24.1 sh -
cd istio-1.24.1
export PATH=$PWD/bin:$PATH
istioctl install --set profile=ambient --skip-confirmation
```

### Pods Not Starting in Mesh

```bash
# Check namespace labels
kubectl get ns istio-test --show-labels

# Check ztunnel logs
kubectl logs -n istio-system -l app=ztunnel

# Check pod events
kubectl describe pod node-0 -n istio-test
```

### Mesh Connectivity Timeout

```bash
# Check wrapper logs
kubectl logs node-0 -n istio-test -c marklogic | grep "Phase 4"

# Check pod-to-pod connectivity
kubectl exec node-0 -n istio-test -c marklogic -- curl -v http://node-1:8001
```

### Test Hangs

```bash
# Increase timeouts in test or wrapper script
# Current timeouts:
# - Pod ready: 300s (5 min)
# - Mesh connectivity: 120s (2 min)
# - ISTIO components: 300s (5 min)
```

## Next Steps

After setting up ISTIO Ambient e2e tests, consider:

1. **CI/CD Integration**: Add ISTIO test job to GitHub Actions workflow
2. **Performance Testing**: Measure latency impact of ISTIO mesh
3. **Multi-Cluster Testing**: Test cross-cluster mesh scenarios
4. **Security Testing**: Validate mTLS enforcement
5. **Upgrade Testing**: Test ISTIO version upgrades with running clusters

## References

- [ISTIO Ambient Mode Documentation](https://istio.io/latest/docs/ambient/)
- [Kubernetes e2e-framework](https://github.com/kubernetes-sigs/e2e-framework)
- [MarkLogic Operator Documentation](../README.md)
- [Wrapper Script Implementation](../pkg/k8sutil/scripts/cluster-init-wrapper.sh)
