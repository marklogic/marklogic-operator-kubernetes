# Contributing Integration Scenarios

These tests connect a product requirement to observable behavior on Kubernetes.
Start with a small scenario and reuse the existing fixtures and test utilities.
The repository [contribution policy](../../CONTRIBUTING.md) still applies. This
technical guide does not change PR acceptance policy or establish an internal
exception; maintainers must agree on any such process separately.

## Adding a scenario

1. Copy [SCENARIO_TEMPLATE.md](SCENARIO_TEMPLATE.md) into your suite directory,
   complete the requirement and acceptance criteria, and link the source issue.
2. Identify the requirements owner, test maintainer, and reviewers. The proposed
   division is for Server/PDC to own product expectations and Kubernetes
   maintainers to own deployment fixtures and cluster lifecycle. Confirm the
   actual people and responsibilities in the scenario issue.
3. Put behavior tests in `marklogic-server/<suite>/`, reusable Kubernetes object
   builders in `marklogic-server/fixtures/`, and cluster lifecycle utilities in
   `marklogic-server/testutil/`. Keep protocol-specific composition in its suite.
4. Gate live tests explicitly. Start each enabled scenario with `testutil.NewRun`
   and apply its namespaced resources through `run.ApplyObjects`. Never use a
   shared namespace or register an unconditional namespace deletion.
5. Add the scenario name and exact test selection to `scripts/run.sh`. Use an
   explicit context and document extra preflight requirements.
6. Add focused local regression tests for failure-prone helper behavior. Run
   `go test ./test/integration/marklogic-server/...` without live gates, then run
   the scenario on its documented environment when available.
7. Include the command, source commit/working changes, versions, executed cases,
   skips, and cleanup outcome in the PR. Record an untested environment as
   untested, rather than inferring coverage from another cloud.

## Review checklist

- The requirement has observable success and failure criteria; unrelated service
  errors cannot pass as expected authentication failures.
- The scenario has named owners and a traceable requirement, or clearly marked
  ownership gaps awaiting agreement.
- Each run owns a fresh namespace and only cleans up its own resources.
- Prerequisites, version constraints, resource needs, and coverage limits are explicit.
- Output is useful to another team and excludes credentials and session tokens.
- Shared helpers are extracted because multiple scenarios need them.

## Troubleshooting

A preflight failure identifies a missing context, permission, CRD, available
operator deployment, or storage class before deploying the workload. It cannot
prove image pull access, sufficient capacity, or operator compatibility from an
image tag alone. Diagnose workload failures from the test's namespace and logs.

Use `INTEGRATION_RETAIN_NAMESPACE=true` to retain a run for inspection. The test
prints its namespace and context-specific inspection and cleanup commands. The
normal cleanup waits up to five minutes for namespace and bound PV deletion and
fails the test if cleanup does not complete. External cloud disk deletion still
requires provider-specific verification when that is part of the acceptance criteria.
