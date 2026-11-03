# Security policy

## Supported versions

Only the newest release gets security fixes. Please upgrade before you
report a problem.

## Reporting a problem

**Do not open a public issue for a security problem.**

Use GitHub's private reporting instead:

1. Go to the [Security tab](https://github.com/hayone1/external-dns-contabo-webhook/security).
2. Click **Report a vulnerability**.
3. Tell us what you found, how to reproduce it, and what an attacker could do with it.

We will answer within 7 days. When the fix is out, we will credit you in the
release notes unless you ask us not to.

## What counts

Good examples:

- the webhook leaks Contabo credentials or access tokens (in logs, metrics or errors)
- the webhook changes or deletes DNS records it should not touch
- a published image contains a known, fixable, high or critical vulnerability
- a weakness in the GitHub workflows that could let someone publish a bad image

Not security problems (open a normal issue instead):

- Contabo API outages or rate limits
- a record type Contabo itself does not support

## How releases are protected

- Images are built by GitHub Actions only, never on a laptop.
- Every image is signed with [cosign](https://github.com/sigstore/cosign)
  (keyless) and has a signed build provenance and SBOM.
- Every image is scanned with [Trivy](https://github.com/aquasecurity/trivy).
- The image has no shell, runs as a non-root user and works with a read-only file system.
- All GitHub Actions are pinned to a commit SHA and updated by Dependabot.

You can check that an image really came from this repository (needs cosign 3 or newer):

```sh
cosign verify ghcr.io/hayone1/external-dns-contabo-webhook:0.1.0 \
  --certificate-identity-regexp '^https://github.com/hayone1/external-dns-contabo-webhook/.github/workflows/release.yml@refs/tags/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```
