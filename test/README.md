# Test MarkLogic Kubernetes Operator with e2e-framework

## How to run the test

```
make e2e-setup-minikube
make e2e-test
make e2e-delete-minikube
```

To run selected tests
```
go test -v ./test/e2e -count=1 -args --labels="type=tls-named-cert"
```