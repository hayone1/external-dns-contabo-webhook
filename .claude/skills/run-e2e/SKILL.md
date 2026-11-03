---
name: run-e2e
description: Run and debug the end-to-end test of the webhook against a real Kubernetes cluster and a real Contabo DNS zone. Use when asked to "run e2e", test against Contabo for real, or check a change before a release.
---

# Run the end-to-end test

The test lives in `test/e2e/run.sh`. It installs the external-dns Helm chart
with the webhook as a sidecar, creates Services, and checks the records in
Contabo through the API and through `ns1.contabo.net`.

## Before you start

- Ask the user which zone to use if `E2E_ZONE` is not set. Never guess.
- Credentials come from `.env`. Load them with `set -a; . ./.env; set +a`.
  Do not print them.
- Needs: `kubectl`, `helm`, `curl`, `jq` (and `dig` if available), plus a
  cluster in the current kube context.

## Steps

1. Build the image:
   `docker build -t external-dns-contabo-webhook:e2e --build-arg VERSION=e2e .`
2. Make the image available to the cluster:
   - kind: `kind load docker-image external-dns-contabo-webhook:e2e`
   - k3s over ssh: `docker save external-dns-contabo-webhook:e2e | ssh root@HOST k3s ctr -n k8s.io images import -`
3. Run: `./test/e2e/run.sh`
   - `E2E_TEST_APEX=true` also tests the bare domain. Only on a zone nothing else manages.
   - `E2E_KEEP=true` keeps the namespace for debugging. Clean up afterwards.

## Reading a failure

The script prints the records it saw plus the last logs of both containers.

| Symptom | Likely cause |
|---|---|
| external-dns logs `No endpoints could be generated` | Annotations use the old `external-dns.alpha.kubernetes.io/` prefix. external-dns v0.23+ reads `external-dns.kubernetes.io/` by default. |
| external-dns logs `nodes is forbidden` | Chart installed with `namespaced: true`. The service source needs a ClusterRole. |
| webhook logs `skipping change: no Contabo zone found` with a hint about the apex | `txtPrefix` is missing or does not end in a dot. |
| `Invalid record name: ...*...` | `--txt-wildcard-replacement` is missing. |
| `must be valid external IPV4` | A target is a private IP. |
| `permanent diff` failure | `AdjustEndpoints` and `Records` disagree. Reproduce in `test/integration` first. |

## Cleanup

The script deletes its namespace and every record under `e2e-<id>.<zone>`
on exit. If a run was killed, list leftover records with the API and delete
only names that start with `e2e-` or `extdns-` and carry the test owner ID.
