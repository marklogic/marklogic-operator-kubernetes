# <Scenario name>

Status: Proposed / Implemented / Live validated (choose one; name validated environments)

Purpose: Product requirement / Teaching example (choose one)

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

## Registration and local contracts

- Catalog entry in `test/integration/scenarios/catalog.json`:
- Unique scenario name and live gate (must match the test):
- One package path and exact top-level test name:
- `requiredEnv` (always includes `INTEGRATION_CONTEXT`):
- `minMarkLogicVersion` if needed (also require `MARKLOGIC_VERSION`):
- `NewRun` namespace prefix and `needsMarkLogic` value:
- Fixture/helper contracts and expected disabled-suite behavior:

Start from the runnable [platform example](marklogic-server/platform/README.md)
and follow [CONTRIBUTING.md](CONTRIBUTING.md); adjust links after copying this file.
Check the gate before setup, and record stages/cases with `run.Stage`/`run.Case`.
Do not put cluster access in initializers or `TestMain`.

```sh
make integration-describe SCENARIO=<scenario-name>
make integration-check
make integration-test-local
```

## Run

```sh
# Replace placeholders; register in scenarios/catalog.json first.
# Remove operator/image settings for scenarios that do not use MarkLogic.
INTEGRATION_CONTEXT=<context> \
INTEGRATION_OPERATOR_NAMESPACE=<operator-namespace> \
INTEGRATION_OPERATOR_DEPLOYMENT=<operator-deployment> \
MARKLOGIC_IMAGE=<image> \
make integration-test SCENARIO=<scenario-name>
```

List additional settings and expected duration. Explain which prerequisites are
checked by the runner/preflight and which need live validation. Record skips
explicitly; a passing package alone does not prove the target executed.

## Lifecycle and troubleshooting

- Use a fresh namespace from `testutil.NewRun`; apply resources through the run.
- List any resources created outside the namespace and their ownership/cleanup.
- Record useful failure signals, diagnostic commands, and known limitations.
- Explain how to inspect and clean up a retained environment.

## Validation evidence

- Date and source commit (include uncommitted changes if applicable):
- Cluster/environment and component versions (list unverified environments separately):
- Exact command:
- Passed / failed / skipped cases:
- Cleanup result, including external resources if applicable:
- Run results directory (`run.json`, `junit.xml`, and filtered diagnostics):
