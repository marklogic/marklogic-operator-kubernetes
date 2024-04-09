# Kubernetes Operator end to end tests

Kubernetes operatore2e tests are implemented using KUbernetes Test TooL (KUTTL). 
Following are the prerequisites for running tests:
1. Set up Kubernetes cluster or Kind cluster in version 1.13 or later.
2. Install [kubectl](https://kubernetes.io/docs/tasks/tools/install-kubectl/) in version 1.13 or later.
3. Install the kubectl kuttl plugin. To do so, please follow the CLI plugin installation instructions described [here](https://kuttl.dev/docs/#install-kuttl-cli).

Use below command to run the tests:

```
make e2e-tests
```

Below are some best practices and guidelines to be followed while writing tests:

1. Each test case should focus on a specific scenario or behavior of the operator.
2. Name of the the steps and test directories should reflect the scenarios being tested. Please refer to https://kuttl.dev/docs/kuttl-test-harness.html#write-a-second-test-step for a quick example.
3. Use asserts to define the state of operator in YAML manifests and use command steps for actions that cannot be expressed in YAML.
4. Tests should cover positive, negative and entire lifecycle scenarios of operator including create, update and delete to ensure the transition is handled correctly.
5. Include comments for tests that involve intricate logic and edge cases for the operator.
6. Mock external dependencies to ensure reliability of tests and not impacted by external factors.

For a detailed guide on KUTTL see [https://kuttl.dev/docs/what-is-kuttl.html#overview](https://kuttl.dev/docs/what-is-kuttl.html#overview)