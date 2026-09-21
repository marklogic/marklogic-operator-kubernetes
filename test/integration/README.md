# Kubernetes integration tests

Use this directory to add opt-in scenarios against an existing Kubernetes
cluster, with shared namespace ownership, cleanup, diagnostics, and JSON/JUnit
reports. Local helper tests do not need a cluster. The runner does not install
an operator or provision cloud infrastructure.

## Discover and validate locally

From the repository root, with Go installed:

```sh
make integration-list
make integration-describe SCENARIO=platform-smoke
make integration-check
make integration-test-local
```

`list` and `describe` read the compiled catalog without requiring kubectl,
kubeconfig, or cluster access. `check` compiles the registered packages and uses
`go test -list` to verify every exact test selection, with all live gates off.
`integration-test-local` also turns every registered gate off before running all
integration helper and contract tests, even when your shell has live gates set.
Some local tests use loopback HTTP servers. The first build may need to download
Go modules; no Kubernetes access is needed.

## Run one live scenario

```sh
INTEGRATION_CONTEXT=<explicit-context> \
make integration-test SCENARIO=platform-smoke
```

The original `make integration-test SCENARIO=oauth-resource-server` command is
preserved. Use `integration-describe` for its required variables and prerequisites.
Equivalent script commands are:

```sh
bash test/integration/scripts/run.sh list
bash test/integration/scripts/run.sh describe platform-smoke
bash test/integration/scripts/run.sh check
bash test/integration/scripts/run.sh local
INTEGRATION_CONTEXT=<explicit-context> bash test/integration/scripts/run.sh run platform-smoke
```

The runner validates metadata and required environment variables, verifies that
exactly one named test exists in the registered package, and enables only that
scenario's gate. It streams readable Go test output and checks JSON test events:
a missing target, top-level skip, all skipped subcases, or failed process is an
error. Partial subcase skips remain visible and must be reviewed against the
scenario's acceptance criteria. Always check the command exit status as well as
`run.json` and `junit.xml`.

## Add your first scenario

Start from the runnable [platform example](marklogic-server/platform/README.md),
then follow the [contribution guide](CONTRIBUTING.md) and
[scenario template](SCENARIO_TEMPLATE.md). Register one entry in
[`scenarios/catalog.json`](scenarios/catalog.json); no shell case or package list
needs updating. See the [lifecycle and artifact contract](marklogic-server/README.md)
and [OAuth suites](marklogic-server/oauth/README.md) for existing product tests.

## Validation boundaries and follow-up work

All actual cluster tests so far have run on EKS. The September 20, 2026 EKS
bearer-token run passed four cases and has cleanup evidence. The original
Authorization Code live validation on MarkLogic 12.1+ remains pending. PDC cloud
runs on AKS, but that does not establish AKS test coverage; AKS tests are unverified.
The new platform example has local contract coverage only until a live run is
recorded. It is a teaching example, not a PDC requirement or product test.

Follow-up work should use a real second product requirement: separate environment
configuration from behavior (including an unverified AKS example), narrow
preflight permissions by scenario, extract fixtures/readiness when reused, and
agree on named owners and maintenance responsibilities. Root PR acceptance
policy remains a team decision. CI, UI, and cloud provisioning are outside this
first increment.
