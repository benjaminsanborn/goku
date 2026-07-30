# 02 — Data model

## ERD

```mermaid
erDiagram
    ORGANIZATION ||--o{ USER_MEMBERSHIP : has
    ORGANIZATION ||--o{ AGENT : registers
    ORGANIZATION ||--o{ PROJECT : owns
    USER ||--o{ USER_MEMBERSHIP : joins
    PROJECT ||--|| AWS_CONNECTION : "deploys via"
    PROJECT ||--|| REPOSITORY : has
    PROJECT ||--o{ RESOURCE : contains
    PROJECT ||--o{ DEPLOYMENT : records
    PROJECT ||--o{ CHANGESET : proposes
    CHANGESET ||--o{ PREVIEW_DEPLOY : previews
    PROJECT ||--o{ POLICY : "is governed by"
    RESOURCE ||--o{ RESOURCE_REVISION : versions
    DEPLOYMENT ||--|| BUILD : from
    AGENT ||--o{ API_TOKEN : authenticates
    USER ||--o{ API_TOKEN : authenticates
    ORGANIZATION ||--o{ AUDIT_EVENT : accumulates
    APPROVAL ||--|| AUDIT_EVENT : resolves

    ORGANIZATION { uuid id  text name  text plan }
    USER { uuid id  text email  text name }
    AGENT { uuid id  uuid org_id  text name  text description  timestamptz last_seen_at }
    API_TOKEN { uuid id  text hash  uuid actor_id  text actor_type  jsonb scopes  timestamptz expires_at }
    PROJECT { uuid id  uuid org_id  text name  text region  text status }
    AWS_CONNECTION { uuid id  uuid project_id  text role_arn  text external_id  text account_id  text status }
    REPOSITORY { uuid id  uuid project_id  text default_branch  bigint size_bytes }
    RESOURCE { uuid id  uuid project_id  text type  text name  text status  text stack_name  jsonb outputs }
    RESOURCE_REVISION { uuid id  uuid resource_id  int revision  jsonb config  uuid created_by  text change_set_id }
    BUILD { uuid id  uuid project_id  text git_sha  text image_uri  text status  text codebuild_id  text log_url }
    DEPLOYMENT { uuid id  uuid project_id  uuid build_id  int number  text status  uuid triggered_by  timestamptz finished_at }
    CHANGESET { uuid id  uuid project_id  int number  text branch  text status  text head_sha  jsonb manifest_delta  uuid opened_by }
    PREVIEW_DEPLOY { uuid id  uuid changeset_id  text url  text status  timestamptz expires_at }
    POLICY { uuid id  uuid project_id  text kind  jsonb rules }
    APPROVAL { uuid id  uuid project_id  text subject_type  uuid subject_id  uuid requested_by  uuid decided_by  text decision }
    AUDIT_EVENT { bigint seq  uuid org_id  uuid actor_id  text actor_type  text action  text subject  jsonb detail  bytea prev_hash  timestamptz at }
```

## Notes on the interesting tables

### `agent` — machine identity is first-class
Agents are not users. They belong to an org, have names and descriptions ("release-bot for project X"), and get their own tokens. Every audit event records `actor_type ∈ {user, agent, system}`. This is the core differentiator: when an auditor asks "who deployed this," the answer can be *"agent `claude-backend-dev`, token `tk_…`, from deployment request `dep_123`"* rather than a shared bot account.

### `api_token` — capability-scoped
Tokens carry explicit scopes (`repo:push`, `deploy:trigger`, `resource:propose`, `logs:read`, …) and a project binding. MCP sessions authenticate with these. Hash-only storage; revocation is a delete.

### `resource` + `resource_revision` — config as versioned document
A resource's user-facing config is small (the catalog is opinionated), e.g. Database: `{instance_class, storage_gb?, ha: bool}`. Every change creates a revision pointing at the CloudFormation change-set it produced, so infra history is: revision → change set → stack events. Nothing mutates in place.

### `deployment` — the compliance artifact
Immutable, monotonically numbered per project. Links git SHA → build → image digest → deploy outcome → who/what triggered it. This table *is* the change-management evidence for SOC 2.

### `audit_event` — append-only, hash-chained
`prev_hash` chains each event to the previous one per org (tamper-evidence). No UPDATE or DELETE grants on this table for the app role; written via a `SECURITY DEFINER` function or dedicated writer role. Exportable to the customer's S3 for retention.

### `policy` — operator guardrails
Rows like:
- `{kind: "approval", rules: {resource_changes: "require_human", deploys: "auto"}}`
- `{kind: "spend", rules: {monthly_ceiling_usd: 500}}`
- `{kind: "deploy_window", rules: {cron: "* 9-17 * * MON-FRI"}}` (optional)

Evaluated in the domain layer before any mutation; failures create an `approval` row instead of erroring, so agent workflows are "propose → wait" rather than "denied."

## Deliberate omissions (MVP)

- **Environments**: one project = one environment. "staging" = a second project (cheap because bootstrap is automated). Revisit if users demand promotion flows — see [07-roadmap](07-roadmap.md).
- **Teams/RBAC granularity**: org roles are just `owner | member | auditor` initially.
- **Multi-region**: `project.region` is singular.
