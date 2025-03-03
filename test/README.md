# Test MarkLogic Kubernetes Operator with e2e-framework

## How to run the test

```
make e2e-setup-minikube
make e2e-test
make e2e-delete-minikube
```

Each test is assigned a “type” label, allowing you to run only the tests of a specified type.

For example, to run only the test for TLS named cert test:
```
go test -v ./test/e2e -count=1 -args --labels="type=tls-named-cert"
```

To run only the TLS self signed cert test:
```
go test -v ./test/e2e -count=1 -args --labels="type=tls-self-signed"
```