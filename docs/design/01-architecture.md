# 01 — Architecture

## Control plane vs data plane

The fundamental split: the **control plane** (our Go API, Postgres, git server, workers) holds metadata and orchestrates; the **data plane** (customer AWS accounts) holds all workload resources, built and mutated only through short-lived assumed-role credentials.

```mermaid
flowchart TB
    subgraph clients["Clients"]
        agent["Claude agent<br/>(MCP client)"]
        human["Operator / Auditor<br/>(browser)"]
        gitcli["git CLI<br/>(agent or human)"]
    end

    subgraph cp["Control plane (platform-operated)"]
        ui["React UI"]
        api["Go API<br/>REST + auth"]
        mcp["MCP server<br/>(streamable HTTP)"]
        gitsrv["Git server<br/>(smart HTTP)"]
        workers["Worker pool<br/>(provisioning, pipeline,<br/>drift, status polling)"]
        pg[("PostgreSQL<br/>orgs, projects, resources,<br/>deployments, audit log")]
        repos[("Repo storage<br/>bare git repos")]
    end

    subgraph customer["Customer AWS account (data plane)"]
        role["IAM role<br/>(trusts platform, ExternalId)"]
        cfn["CloudFormation stacks"]
        subgraph vpc["Project VPC"]
            alb["ALB"]
            fargate["Fargate service (API)"]
            aurora[("Aurora PostgreSQL")]
        end
        cb["CodeBuild (image builds)"]
        ecr["ECR"]
        s3["S3 buckets"]
        cf["CloudFront"]
    end

    agent -->|MCP tools| mcp
    human --> ui --> api
    gitcli -->|git push| gitsrv
    mcp --> api
    gitsrv -->|post-receive → enqueue| workers
    api --> pg
    workers --> pg
    gitsrv --> repos
    workers -->|"AssumeRole (STS)"| role
    role --> cfn --> vpc
    workers -->|start builds| cb
    cb --> ecr
    fargate --> aurora
    alb --> fargate
    cf --> s3
```

## Control plane: modular monolith

One Go binary, multiple internal modules, deployed as N identical replicas behind a load balancer. No microservices until scale forces it — the modules share a schema and a transaction boundary, which matters for audit consistency (a deploy row and its audit entry commit together).

```mermaid
flowchart LR
    subgraph binary["platform (single Go binary)"]
        direction TB
        httpmux["HTTP mux"]
        resthandlers["REST handlers"]
        mcphandlers["MCP handlers"]
        githandlers["Git smart-HTTP handlers"]
        authz["AuthN/AuthZ<br/>(tokens, sessions, RBAC, policy)"]
        domain["Domain layer<br/>orgs / projects / resources /<br/>deployments / audit"]
        queue["Job queue (river,<br/>Postgres-backed)"]
        provisioner["Provisioner worker<br/>(CFN orchestration)"]
        pipeline["Pipeline worker<br/>(build → deploy state machine)"]
        poller["Pollers<br/>(stack status, ECS health, drift)"]
    end
    httpmux --> resthandlers & mcphandlers & githandlers
    resthandlers & mcphandlers & githandlers --> authz --> domain
    domain --> queue
    queue --> provisioner & pipeline & poller
```

Key choices:

- **Job queue in Postgres** ([riverqueue/river](https://github.com/riverqueue/river)): provisioning and deploys are multi-minute async jobs; keeping the queue in the same Postgres as the domain data gives transactional enqueue (job + audit row + state change commit atomically) and one fewer piece of infrastructure.
- **MCP and REST share the domain layer.** MCP tools are thin adapters over the same service methods the REST API uses — same authz, same audit, no drift between interfaces.
- **Git server is in-process** for the MVP: smart-HTTP (`git-upload-pack`/`git-receive-pack`) handlers over bare repos on a persistent volume, with async replication of repo contents to S3 for durability. A push's post-receive step enqueues a pipeline job in the same transaction that records the push. (See [04](04-git-and-cicd.md) for the build-vs-embed discussion.)
- **All AWS access is jittered, short-lived STS credentials** obtained per job via `AssumeRole` with a per-project `ExternalId`. The control plane never holds long-lived customer credentials in memory beyond a job's lifetime and never persists them (static-key fallback, if we allow it at all, is KMS envelope-encrypted at rest).

## Data plane: what lives in the customer account

Everything workload-related: VPC, Aurora, Fargate, ALB, S3, CloudFront, ECR, CodeBuild, CloudWatch logs, Secrets Manager secrets, KMS keys. The customer's AWS bill is their own; the platform bills for the control plane service. Detailed layout in [03-provisioning](03-provisioning.md).

## Trust boundaries

```mermaid
flowchart LR
    A["Agent<br/>(untrusted code author)"] -- "scoped token<br/>MCP + git only" --> B["Control plane<br/>(policy enforcement point)"]
    B -- "STS AssumeRole<br/>ExternalId, scoped policy" --> C["Customer AWS account"]
    D["Operator (human)"] -- "session auth,<br/>sets policy" --> B
```

1. **Agent → control plane**: agents hold project-scoped tokens that can push code, trigger deploys, and read state. They can *request* resource changes; policy decides whether those apply immediately or wait for operator approval.
2. **Control plane → customer account**: the assumed role's policy is scoped to platform-tagged resources and the specific services in the catalog — not `AdministratorAccess`. CloudFormation is the only mutation path, which makes every infra change diffable and reviewable.
3. **Operator → control plane**: humans own policy, credentials, and guardrails. An agent can never widen its own permissions.

## Availability posture (MVP)

- Control plane: multi-AZ Postgres, ≥2 stateless API replicas; git repo volume is the one stateful component (single writer, S3-replicated, RPO ≈ minutes).
- An outage of the control plane must **not** take down customer workloads — data-plane resources run independently; only new deploys/changes are blocked. This is a hard requirement and a nice compliance/DR story.
