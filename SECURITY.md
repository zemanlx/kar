# Security policy

## Supported versions

Only the latest release is supported, currently the 0.2.x line. Fixes ship in a new patch release. There are no backports.

## Reporting a vulnerability

Report it privately through [GitHub private vulnerability reporting](https://github.com/zemanlx/kar/security/advisories/new). Do not open a public issue.

Include:

- the affected version of the image or chart
- steps to reproduce, with the policies, bindings and objects involved
- the impact: what an attacker can read, write or bypass

kar is maintained by one person on a best-effort basis. I will acknowledge the report and coordinate the fix and its disclosure through the advisory.

## Scope

The default ClusterRole grants get/list/watch on every resource, Secrets included, because the audit reads whatever a policy matches. [README](README.md#configuration) explains how to narrow it with `rbac.rules`.

kar writes only:

- `ValidatingAdmissionPolicyBindings` it derives from a source binding (`<source>-enforce`)
- `PolicyExemptions` and their status
- `Events`
- the leader election Lease `kar.zeman.dev` in the release namespace

Reports that kar reads or exposes data beyond what the audited policies need, or writes anything outside the list above, are in scope.

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
