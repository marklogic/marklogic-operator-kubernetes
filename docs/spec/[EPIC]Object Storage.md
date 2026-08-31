# Object Storage

## Problem

*Business problem, why and what?*

Customers deploying MarkLogic with the Kubernetes Operator must configure object storage access (AWS S3 and/or Azure Blob) after cluster deployment. This requires manual steps, increases operational friction, and leads to inconsistent and error‑prone setups. As a result, clusters are not fully usable “out of the box,” especially for backups and storage integration.

## Personas

*People impacted by this.*

- Platform Engineers / SREs – standardize and automate cluster provisioning

- DevOps Engineers – manage GitOps workflows and environment consistency

- MarkLogic Administrators – configure storage and backups

- Security Teams – ensure credentials are handled securely and consistently

## Outcome

*What does success look like for the customer?*

- Users can declaratively configure AWS and/or Azure object storage access at cluster creation time via the Kubernetes Operator.

- MarkLogic clusters are deployed with required infrastructure access already in place.

- Storage credentials are managed in a Kubernetes‑native, secure way.

## Value

*How does this benefit Progress, and why should we address this need?*

- Faster time to value: clusters are usable immediately after deployment.

- Reduced operational overhead: eliminates manual post‑deployment configuration.

- Improved security posture: standardized handling of cloud credentials.

- Stronger Kubernetes positioning: MarkLogic fits more naturally into cloud‑native platforms.

## Acceptance Criteria

*Essential conditions or attributes that must be satisfied.*

- The Kubernetes Operator supports configuration of AWS S3 and Azure Blob credentials as part of the MarkLogic cluster spec.

- Credentials are provided via Kubernetes‑native mechanisms (e.g., Secrets or cloud identity references).

- The configuration applies automatically during cluster reconciliation.

- Operator status clearly indicates whether object storage configuration succeeded or failed.

- The feature supports configuring storage access for backups and supported forest storage scenarios.

- Configuration is fully declarative and compatible with GitOps workflows.

## Notes and Assumptions

- MarkLogic already supports accessing AWS S3 and Azure Blob storage when properly configured.

- Credential handling must not expose secrets in logs or status fields.

- Only supported object‑storage usage patterns should be configurable via the Operator.

- Further research is required to assess whether Kubernetes CSI abstractions are a viable integration option.

