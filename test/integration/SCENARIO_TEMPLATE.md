# <Scenario name>

Status: Proposed / Implemented / Validated (choose one)

## Requirement and ownership

- Business/release requirement:
- Source Jira or specification:
- Requirements owner (Server/PDC/other): TBD
- Test maintainer: TBD
- Kubernetes fixture reviewer: TBD
- Product behavior reviewer: TBD
- Failure triage contact: TBD

## Prerequisites

- Kubernetes and operator versions/capabilities:
- Product image and version:
- Supporting services/images:
- Storage, CPU/memory, image pull access, and network requirements:
- Required permissions:
- Version-specific behavior:

## Cases

| Case | Input / action | Required result | Errors that must fail the test |
| --- | --- | --- | --- |
| Positive | | | |
| Negative | | | |

State what the scenario does not cover and what evidence proves the expected behavior.

## Run

```sh
# Replace placeholders and register the scenario with the runner first.
INTEGRATION_CONTEXT=<context> \
INTEGRATION_OPERATOR_NAMESPACE=<operator-namespace> \
INTEGRATION_OPERATOR_DEPLOYMENT=<operator-deployment> \
MARKLOGIC_IMAGE=<image> \
make integration-test SCENARIO=<scenario-name>
```

List additional settings, expected duration, and how to run local helper tests.

## Lifecycle and troubleshooting

- Use a fresh namespace from `testutil.NewRun`; apply resources through the run.
- List any resources created outside the namespace and their ownership/cleanup.
- Record useful failure signals, diagnostic commands, and known limitations.
- Explain how to inspect and clean up a retained environment.

## Validation evidence

- Date and source commit (include uncommitted changes if applicable):
- Cluster/environment and component versions:
- Exact command:
- Passed / failed / skipped cases:
- Cleanup result, including external resources if applicable:
- Evidence/log location (redacted):
