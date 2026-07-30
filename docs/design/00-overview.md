# 00 — Overview

## Problem

AI agents can write production-quality code, but giving an agent the keys to ship that code is where things fall apart:

- **Raw AWS is too much rope.** An agent with `AdministratorAccess` and a Terraform repo can do anything, which is exactly the problem. Every additional degree of freedom is something a reviewer (human or automated) has to check.
- **GitHub + Actions is human-shaped.** PRs, reviews, checks, and environments assume a human org chart. Agents need machine identities, scoped capabilities, and an audit trail that attributes every change to a specific agent run.
- **Compliance is retrofitted.** Encryption, network isolation, audit logging, and change management are things teams bolt on. For regulated workloads they need to be defaults you can't turn off.

## Thesis

Constrain the problem the way Aptible did for HIPAA startups: offer a **small catalog of curated, opinionated resources** and an **opinionated deploy pipeline**, and in exchange give customers compliance-grade defaults with zero infrastructure expertise required. Then make the primary interface **agent-native**: an MCP server and a built-in git remote, so "install the platform into your Claude" is the onboarding story.

The constraint is the product. Because the resource catalog is small and the pipeline is fixed, the blast radius of any agent action is bounded and auditable by construction.

## Personas

| Persona | Who | What they do |
|---|---|---|
| **Operator** | Human org owner/admin | Creates the org, connects AWS, sets guardrails and approval policies, holds the billing relationship |
| **Agent** | Claude (or other) instance with the MCP server installed | Pushes code to project repos, triggers deploys, reads logs and status, proposes resource changes |
| **Auditor** | Human (compliance, security) | Reads the audit trail, deployment history, and control evidence; never writes |

## Product principles

1. **Curated over general.** Five resource types, not five hundred. Each one is the production-grade version of the thing (Aurora, Fargate, ALB, S3, CloudFront) with security settings pre-decided and non-negotiable.
2. **Customer's account, customer's data.** All workload resources live in the customer's AWS account via cross-account role assumption. The platform stores metadata, not workloads. This is both the compliance story and the billing story.
3. **Every write is attributable.** Every mutation — git push, deploy, config change, resource creation — is attributed to a human or a named agent identity, in an append-only audit log.
4. **Push-to-deploy, nothing else.** `git push goku main` is the deployment interface. No deploy YAML, no pipeline DSL. The pipeline is the same for everyone.
5. **Guardrails, then autonomy.** Operators set policy (approval gates, spend ceilings, protected resources); within policy, agents act autonomously.

## Non-goals

- General-purpose IaC or arbitrary AWS resource types
- Kubernetes
- Being a git *collaboration* platform (code review UI, issues, discussions) — the repo exists to receive deployable code, not to host a community
- Multi-cloud (AWS only, single region per project initially)
- Formally certifying customers' compliance (we provide controls and evidence; see [06](06-security-compliance.md) for what we can and can't claim)

## Comparables

| | Aptible | Heroku | GitHub + Actions | **This platform** |
|---|---|---|---|---|
| Runs in customer's cloud account | ✕ (Aptible-owned) | ✕ | n/a | **✓** |
| Curated resource catalog | ✓ | ✓ | ✕ | **✓** |
| Compliance-first defaults | ✓ | ✕ | ✕ | **✓** |
| Built-in git remote | ✓ | ✓ | ✓ | **✓** |
| Agent-native (MCP, machine identity) | ✕ | ✕ | partial | **✓** |
