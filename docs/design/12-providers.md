# 12 — Cloud providers

A **provider** is a cloud account goku is allowed to deploy into. Providers
are the layer under the fleet (doc 10): the fleet says *which machines exist*,
providers say *which accounts machines can be created in*.

## Model

`cloud_providers` is org-scoped, one row per connected account:

| column        | meaning                                                     |
| ------------- | ----------------------------------------------------------- |
| `kind`        | `aws` \| `azure` \| `digitalocean` \| `tailscale`            |
| `credentials` | jsonb, write-only through the API (`json:"-"`, like ssh keys) |
| `region`      | default region for anything provisioned from this account   |
| `status`      | `verifying` → `ready` \| `pending` \| `invalid`             |
| `account`     | identity the credentials resolved to (ARN, email, tenant)   |
| `check_log`   | last verification transcript                                 |

Adding a provider verifies asynchronously against the provider's own identity
endpoint — the enroll-then-check shape the fleet already uses:

- **AWS** — STS `GetCallerIdentity`, records the caller ARN.
- **Azure** — client-credentials token exchange for the management scope.
- **DigitalOcean** — `GET /v2/account`, records the account email.
- **Tailscale** — OAuth client-credentials exchange, then a device list.

Providers have a **role**. `aws`, `azure` and `digitalocean` are *compute*:
they supply machines. `tailscale` is *network*: it supplies no machines, it
supplies the path the control plane reaches them over. There is no provision
button on a network provider — connecting one changes how every compute
provider's instances are launched.

Instances gain `provider_id`, `external_id` (e.g. an EC2 instance id) and
`key_name`, all empty for machines an operator attached by hand.

## AWS is the implemented path

`internal/cloud` speaks the AWS wire protocols directly — SigV4 over the EC2
query API and the SSM JSON API — rather than pulling in the AWS SDK, keeping
the control plane on the standard library.

Provisioning (`POST /v1/providers/{id}/instances`):

1. Resolve the current Ubuntu 24.04 AMI from Canonical's public SSM parameter,
   so no image ids are hardcoded per region.
2. Find the default VPC and the security group for the ingress model below.
3. Create a key pair dedicated to this instance — the private half is stored
   as the instance's ssh key and never leaves the deploy path.
4. `RunInstances` (default `t3.small`, 30GB gp3, IMDSv2 required) with a
   cloud-init that installs docker and grants the `ubuntu` login access.
5. Wait for `running`, resolve the address the control plane should use, then
   retry ordinary fleet verification until docker answers.

The machine is then an unremarkable `ssh` fleet instance: the existing deploy
engine (doc 10 — remote build from a piped tar, containers over ssh) needs no
changes to target it. Removing a provisioned instance also terminates it and
deletes its key pair; hand-attached instances are only deregistered.

## Azure and DigitalOcean are pending

Both verify credentials and settle at status `pending`. Provisioning is
rejected with a 422 explaining that only AWS can launch instances today, and
the UI shows a "deployments pending" pill instead of a provision button. The
credential storage, verification, and audit path is identical, so implementing
them is a matter of adding a `Provision`/`Terminate` pair alongside `ec2.go`.

## Ingress

Two inbound paths matter, not one: the control plane SSHes to instances *and*
the central Caddy proxy dials app ports on them for routed traffic (ports
30000–39999, per `deploy.Port`). Database ports stay bound to the instance's
own loopback and are never exposed.

**With a tailnet connected (preferred).** Provisioning mints a short-lived,
single-use, pre-authorized auth key tagged `tag:goku-fleet`, and cloud-init
runs `tailscale up` with it. The instance's security group is created with
**no ingress rules at all**; its recorded address is its `100.x` tailnet
address, which serves both ssh and proxy traffic. Because the address comes
from the Tailscale API rather than MagicDNS, the control plane needs no
special resolver — but it does need to be on the tailnet itself.

Auth keys are minted per instance rather than stored, so a leaked key expires
in 15 minutes and admits one machine. Terminating an instance also deletes its
tailnet device.

**Without one (fallback).** The provisioner discovers its own egress address
and opens ssh plus the app port range to that `/32` only. Rules are
re-authorized on every launch, so a control plane that has changed address
re-adds itself rather than locking the fleet out. This is strictly a stopgap:
it is AWS-only, it breaks when the egress address changes between launches,
and each new provider would have to re-solve it. Launching with neither a
tailnet nor a discoverable egress address is refused outright.

## Open edges

- Credentials are plaintext in Postgres, like `project_secrets` — at-rest
  encryption is still the pending KMS phase (doc 06).
- The control plane container must itself be on the tailnet (host networking
  or a sidecar) for tailnet addressing to work.
- Authentication is still EC2 key pairs; Tailscale is the network only.
  Adopting Tailscale SSH would remove private keys from the database
  entirely — worth doing once the mesh is load-bearing.
- No autoscaling or placement policy: an operator picks the instance per
  deployment, exactly as with hand-attached machines.
