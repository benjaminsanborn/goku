# 07 — Roadmap, open questions, risks

## The scope problem, stated honestly

This design contains four products: a PaaS control plane, an IaC engine, a git host, and a CI/CD system — each of which has killed startups on its own. The design survives only because each is radically cut down: 5 resource types, 1 pipeline shape, 1 git operation that matters (push to main), CFN as the IaC engine instead of our own. Guard that reduction jealously; every "just add a knob" request is scope creep against the core thesis.

## Phases

### Phase 0 — Walking skeleton (weeks, not months)
One org, one project, hardcoded policy. Prove the riskiest seam end-to-end:
- Go API + Postgres + river worker
- AWS connect flow (role + ExternalId + verification)
- Network stack + **API + Load Balancer** resources only
- Minimal smart-HTTP git push → CodeBuild → ECS deploy → health-gated rollout
- CLI-level output; no UI, no MCP

*Exit criterion: `git push goku main` on a Dockerfile repo puts a container behind HTTPS in a fresh customer account, in one command from empty.*

### Phase 1 — Agent-ready MVP
- Remaining catalog: Database, Storage, Web page
- Agent identities, scoped tokens, audit log (hash-chained from day one — retrofitting is miserable)
- MCP server with the [05](05-interfaces.md) tool set
- Approval gates + spend ceiling policies
- UI: project overview, deployments, approvals inbox, audit view

*Exit criterion: a Claude instance with only the MCP install + git credential ships a full-stack app (web + api + db) to a customer account with a human approving one resource change.*

### Phase 2 — Compliance product
- Evidence exports, control-mapping docs, drift detection schedule
- SOC 2 Type I → II engagement for the platform itself
- Backup restore verification jobs
- `release` command / migrations, custom domains polish, log retention policies

### Phase 3 — Expansion (demand-driven, pick fights carefully)
- Environments/promotion or "clone project" flows
- More catalog entries (Worker service, Queue=SQS, Cache=ElastiCache, Cron)
- Multi-region, BAA/HIPAA program, bring-your-own-GitHub mirror (maybe never — see below)

## Open questions

1. **Environments.** Is "staging = second project" good enough? Cheap answer that preserves the model; promotion flows (deploy the *same digest* to prod) argue for first-class envs eventually. Leaning: stay with projects until Phase 3.
2. **Buildpacks vs Dockerfile-only at MVP.** Dockerfile-only is far less machinery; agents are good at writing Dockerfiles. Leaning: Dockerfile-only Phase 0–1, buildpacks later if ever.
3. **Static credential fallback.** Supporting access keys doubles the credential-handling surface and weakens the story. Leaning: role-assumption only; keys never.
4. **Region strategy.** Per-project region choice from a shortlist (us-east-1/us-west-2/eu-west-1) vs us-east-1-only for Phase 0/1.
5. **GitHub mirroring.** Teams will ask to mirror the platform repo to GitHub for human visibility. Read-only mirror preserves the audit chain; accepting GitHub as a *source* breaks it. Leaning: outbound mirror only, Phase 3.
6. **Who is the customer?** Companies deploying agent-written internal apps, or agent-platform companies deploying per-user apps at fan-out (thousands of projects)? The second changes quota math (VPCs/account, ALBs) and pricing entirely. Needs an answer before Phase 2 pricing.
7. **Control plane hosting**: platform's own AWS account region(s); does the git volume become the scaling bottleneck first, or Postgres?

## Top risks

| Risk | Severity | Mitigation |
|---|---|---|
| **Scope**: four products in one | ☠️ | Phase 0 skeleton before any breadth; refuse knobs |
| **AWS quota walls** at fan-out (VPCs, ALBs, EIPs per account) | High | Quota checks in connect verification; document limits; account-per-project pattern later |
| CFN latency/limitations frustrate iteration | Med | Acceptable for CD cadence; templates versioned so engine is swappable |
| Git server durability (single-writer volume) | Med | S3 bundle replication, tested restore; smallness of surface is the safety |
| CodeBuild cold starts feel slow vs Vercel-class DX | Med | Set expectation: this is compliant CD, not preview-deploy DX; cache images |
| "Compliant" overclaim liability | High | Language discipline ([06](06-security-compliance.md)); legal review before marketing |
| MCP spec churn | Low-Med | MCP layer is a thin adapter over domain services by design |
| Aptible/Porter/Flightcontrol add agent interfaces before we ship | Med | The moat is agent-native audit + guardrails, not deployment mechanics — build Phase 1 fast |
