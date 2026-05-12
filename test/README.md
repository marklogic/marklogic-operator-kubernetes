# Test MarkLogic Kubernetes Operator with e2e-framework

The operator has two e2e test environments: **Minikube** (local, for development)
and **EKS** (the Jenkins CI target).

---

## Minikube (local development)

```bash
make e2e-setup-minikube
make e2e-test
make e2e-delete-minikube
```

---

## EKS (CI / full test run)

The EKS environment uses a persistent cluster (`jenkins-kube-ninjas`, `us-west-1`).
Worker nodes are scaled to **0** between runs to save cost; the test targets scale
them up and back down automatically.

```bash
# One-time bootstrap (first time only):
bash test/eks-config/setup-eks.sh

# Build and push the operator image, then run all tests:
make e2e-setup-eks
E2E_MARKLOGIC_IMAGE_VERSION=<ecr-image-uri> make e2e-test-eks
make e2e-cleanup-eks
```

For full setup and operational details see [test/eks-config/README.md](eks-config/README.md).

---

## Run Specific Test Types
Each test is assigned a "type" label, allowing you to run only the tests of a specified type.

For example, to run only the test for TLS named cert test:
```
go test -v ./test/e2e -count=1 -args --labels="type=tls-named-cert"
```

To run only the TLS self signed cert test:
```
go test -v ./test/e2e -count=1 -args --labels="type=tls-self-signed"
```
