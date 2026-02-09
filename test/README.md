# Test MarkLogic Kubernetes Operator with e2e-framework

## How to run the test

```
make e2e-setup-minikube
make e2e-test
make e2e-delete-minikube
```
## Run tests with ISTIO Ambient Mode

To test the operator with ISTIO Ambient mode enabled:

```bash
# Setup minikube cluster with ISTIO Ambient mode
make e2e-setup-minikube-istio

# Run only ISTIO ambient tests
go test -v ./test/e2e -count=1 -args --labels="type=istio-ambient"

# Cleanup
make e2e-cleanup-minikube
```

## Run Specific Test Types
Each test is assigned a “type” label, allowing you to run only the tests of a specified type.

For example, to run only the test for TLS named cert test:
```
go test -v ./test/e2e -count=1 -args --labels="type=tls-named-cert"
```

To run only the TLS self signed cert test:
```
go test -v ./test/e2e -count=1 -args --labels="type=tls-self-signed"
```

To run only ISTIO Ambient mode tests:
```
go test -v ./test/e2e -count=1 -args --labels="type=istio-ambient"
```

## ISTIO Ambient Mode Testing

The ISTIO Ambient mode test ([6_istio_ambient_test.go](e2e/6_istio_ambient_test.go)) validates:

1. **ISTIO Installation**: Verifies ISTIO components (istiod, ztunnel) are running
2. **Namespace Labeling**: Creates namespace with `istio.io/dataplane-mode: ambient` label
3. **Pod Deployment**: MarkLogic pods start successfully in the mesh
4. **Wrapper Script Phases**: Validates all 7 phases of cluster-init-wrapper.sh
5. **Mesh Connectivity**: Confirms Phase 4 mesh connectivity checks pass
6. **PID Refresh Logic**: Tests Phase 5.5 PID refresh after config
7. **Crash Detection**: Simulates MarkLogic crash and verifies pod restart
8. **Cluster Health**: Queries MarkLogic API to confirm operational status

### Prerequisites

- Minikube with Docker driver
- kubectl configured
- istioctl (automatically downloaded by install script)
- 8GB+ RAM allocated to minikube

### Manual ISTIO Setup

If you need to set up ISTIO manually:

```bash
# Install ISTIO with ambient profile
curl -L https://istio.io/downloadIstio | ISTIO_VERSION=1.24.1 sh -
cd istio-1.24.1
export PATH=$PWD/bin:$PATH

istioctl install --set profile=ambient --skip-confirmation

# Verify installation
kubectl get pods -n istio-system
```

### Troubleshooting

**Pods not starting:**
- Check ISTIO ztunnel logs: `kubectl logs -n istio-system -l app=ztunnel`
- Verify namespace has ambient label: `kubectl get ns istio-test --show-labels`

**Mesh connectivity timeout:**
- Increase timeout in wrapper script (currently 2 minutes)
- Check pod logs: `kubectl logs node-0 -n istio-test`

**Test failures:**
- Run with verbose output: `go test -v ./test/e2e -count=1 -args --labels="type=istio-ambient"`
- Check wrapper logs in pod: `kubectl exec node-0 -n istio-test -c marklogic -- cat /var/opt/MarkLogic/Logs/wrapper.log`
