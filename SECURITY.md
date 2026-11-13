# Security policy

## Supported versions

Only the latest release is supported, currently the 0.2.x line. Fixes ship in a new patch release. There are no backports.

## Reporting a vulnerability

Report it privately through [GitHub private vulnerability reporting](https://github.com/zemanlx/kar/security/advisories/new). Do not open a public issue.

Include:

- the affected version of the image or chart
- steps to reproduce, with the policies, bindings and objects involved
- the impact: what an attacker can read, write or bypass

kar is maintained by one person on a best-effort basis. I will acknowledge the report within 14 days and coordinate the fix and its disclosure through the advisory.

## Scope

The default ClusterRole grants get/list/watch on every resource, Secrets included, because the audit reads whatever a policy matches. [README](README.md#configuration) explains how to narrow it with `rbac.rules`.

kar writes only:

- `ValidatingAdmissionPolicyBindings` it derives from a source binding (`<source>-enforce`)
- `PolicyExemptions` and their status
- `Events`
- the leader election Lease `kar.zeman.dev` in the release namespace

Reports that kar reads or exposes data beyond what the audited policies need, or writes anything outside the list above, are in scope.

## Security design

What kar can harm, if it fails or is compromised:

- **Confidentiality.** kar reads every object its ClusterRole allows, Secrets included by default. It keeps only a stub per object in memory (name, namespace, resourceVersion and, for a violator, the binding and the policy's message) and serves only metrics and health probes. The only object content it writes out is what a policy's `message` or `messageExpression` quotes, which kar logs with each violation.
- **Enforcement.** kar can create, change and delete any `ValidatingAdmissionPolicyBinding`, not only the ones it derives, because RBAC cannot limit `create` by name and the derived names are not known in advance. A compromised kar could remove admission enforcement or deny requests cluster-wide.
- **Availability.** A wrong exemption list makes the Deny binding block workloads that already violate a policy, which is what kar exists to prevent.

Trust boundaries:

- Every input comes from the API server, which has already authenticated, authorized and validated it: policies, bindings, the objects they match, namespaces and params. Who can steer kar is decided by who can write those objects, so the annotations on a source binding are as sensitive as the binding itself (see [Using kar securely](README.md#security)).
- kar has no inbound API. The metrics and health endpoints are read-only plain HTTP.
- Policy expressions are evaluated by the API server's own admission code, through [kat](https://github.com/zemanlx/kat). kar does not parse or execute anything else from the cluster.

How the design limits the harm:

- **Additive enforcement.** kar never edits or deletes a source binding. The Deny binding it writes adds enforcement; the exemptions only narrow that Deny binding, and the source binding's own actions still apply in exempt namespaces. If kar stops, the last Deny binding keeps enforcing with a frozen exemption list.
- **Fail-safe for workloads.** When kar cannot evaluate a policy as admission would, for example because it reads `request.userInfo`, it marks it `Auditable=False` and writes no Deny binding rather than guessing. kar does not fake request inputs, and it never truncates its state: a status the API server rejects as too large leaves the last status and Deny binding in place (`StateTooLarge`).
- **Least privilege.** The only cluster-wide writes are admission bindings, `PolicyExemptions` and their status, and Events. Reads can be narrowed with `rbac.rules`. The leader election Role is namespaced and allows `get` and `update` only on kar's own Lease. The service account token is projected with a 1h expiry. The pod meets Pod Security `restricted` and runs a static binary on `scratch` with a read-only root filesystem.
- **Small mechanism.** One binary, no cryptography of its own, no storage other than Kubernetes objects. kar's durable state is the `PolicyExemption` status, which anyone with read access can inspect.
- **Supply chain.** Dependencies and CI actions are pinned and updated by Dependabot. CodeQL and golangci-lint, including gosec, run on every pull request, and the tests run under the race detector. Releases are built in GitHub Actions and carry build provenance attestations. [OpenSSF Scorecard](https://scorecard.dev/viewer/?uri=github.com/zemanlx/kar) checks the repository weekly.

## Verifying releases

The image `ghcr.io/zemanlx/kar` and the chart `oci://ghcr.io/zemanlx/charts/kar` carry GitHub build provenance attestations:

```bash
gh attestation verify oci://ghcr.io/zemanlx/kar:<version> --owner zemanlx
gh attestation verify oci://ghcr.io/zemanlx/charts/kar:<version> --owner zemanlx
```

From 0.2.6, each GitHub release also attaches the chart's provenance bundle as `kar-<version>.tgz.intoto.jsonl`. Verify the downloaded chart against it:

```bash
gh attestation verify kar-<version>.tgz --bundle kar-<version>.tgz.intoto.jsonl --owner zemanlx
```
