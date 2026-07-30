# 06 — Security & compliance

## What "compliant" means here (and what it can't)

Precision matters commercially. The platform:

- **Provides**: compliance-grade *technical controls by default* (encryption, isolation, audit, change management, backups) in the customer's account, plus exportable evidence.
- **Pursues**: SOC 2 Type II for the platform itself (the control plane is in every customer's audit scope the moment they use us).
- **Enables**: customers' SOC 2 / HIPAA programs — control mapping docs, evidence exports, and (eventually, like Aptible) BAAs.
- **Cannot claim**: that a customer "is compliant" by using us. Compliance includes their org processes. Marketing must say "compliance-ready infrastructure," not "makes you HIPAA compliant."

## Identity & access model

| Actor | Authenticates via | Can | Cannot |
|---|---|---|---|
| Operator (human) | SSO/password + MFA, session | Everything incl. policy, tokens, deletes, approvals | — |
| Member (human) | Same | Deploy, propose changes, read | Policy, deletes, token mint for others |
| Auditor (human) | Same | Read everything incl. audit export | Any write |
| Agent | Scoped bearer token (hashed at rest, expiring) | Push, deploy, propose, read logs/status | Delete, mint tokens, change policy, approve own proposals |
| Platform → customer AWS | STS AssumeRole + ExternalId, 15–60 min creds | Catalog services on `platform-*`-tagged resources, CFN | Anything outside scoped policy; IAM beyond permissions-boundary roles |

Invariants worth stating as design law:
1. **Agents can never widen their own permissions** — no path from agent token to policy/token endpoints.
2. **Agents can never destroy state** — deletes are human-confirmed, always.
3. **Separation of duties is policy-selectable** — orgs that need it can require that the approving human ≠ the proposing actor.

## Agent-specific guardrails

This is the novel surface. A misbehaving agent (prompt-injected, hallucinating, or just wrong) is the threat model GitHub never designed for:

- **Bounded action space**: the curated catalog + fixed pipeline means the worst-case config change is enumerable. There is no tool that opens a security group, makes a bucket public, or disables encryption — those aren't parameters.
- **Approval gates** (per-project policy): resource changes and/or deploys can require a human click. Default: resource changes gated, deploys auto.
- **Spend ceiling**: provisioning worker estimates monthly cost of a change set (instance pricing tables); changes pushing the project over the org ceiling become approvals regardless of policy.
- **Rate limits per agent identity**: deploy frequency caps, token-scoped throttles — a looping agent can't redeploy 400× overnight.
- **No secrets exposure**: agents reference secret *names*; values live in Secrets Manager (customer account) and are injected into tasks at runtime. No MCP tool or API returns a secret value.
- **Prompt-injection posture**: MCP tool results (logs!) can contain attacker-controlled text. Log output returned to agents is clearly typed as untrusted data in tool result framing, and no tool takes free-text that becomes shell/IAM/SQL anywhere in the control plane.

## Platform security

- Control plane single-tenant Postgres, encrypted at rest; per-org row-level scoping enforced in the domain layer (and RLS as backstop).
- Static credential fallback (if offered): KMS envelope encryption, decrypt only inside provisioning workers, never returned by any API.
- Git repo storage encrypted volume; repo access only through authenticated smart-HTTP.
- All platform infra itself managed as code; platform runs *on itself* as soon as feasible (dogfooding is the best QA for the catalog).

## Data plane controls (defaults, non-negotiable)

Encryption in transit (TLS everywhere incl. ALB→task, app→Aurora) and at rest (project KMS CMK across Aurora/S3/logs/ECR); private-by-default networking per [03](03-provisioning.md); VPC flow logs; ALB/S3/CloudFront access logs; Aurora automated backups 14 d + deletion protection; container read-only root FS, non-root user enforced at task-def level.

## Audit & evidence

- Append-only, hash-chained `audit_event` (see [02](02-data-model.md)); covers auth events, pushes, builds, deploys, config revisions, approvals, policy changes, token lifecycle, AWS connection changes.
- **Evidence exports**: signed CSV/JSON bundles per audit period — deployment history (change management), access reviews (token/member list + last-used), drift reports, backup verification. This is the feature auditors actually ask for.

## Control mapping (excerpt)

| Control area (SOC 2) | Platform mechanism |
|---|---|
| Change management (CC8.1) | Immutable deployment records: sha → image digest → deploy outcome → actor; approval gates; no console mutation path |
| Logical access (CC6.1–6.3) | Scoped tokens, RBAC, MFA for humans, quarterly access-review export |
| System monitoring (CC7.x) | Health checks, drift detection, deploy failure alerting, flow/access logs |
| Encryption (CC6.7) | CMK-encrypted everything, TLS-only policies baked into templates |
| Availability (A1.x) | Multi-AZ options, automated backups, tested restore (roadmap), control-plane-down ≠ workload-down |

HIPAA technical safeguards map similarly (access control, audit controls, integrity, transmission security); BAA posture is a business decision for later — it requires the platform org itself to operate a HIPAA program.
