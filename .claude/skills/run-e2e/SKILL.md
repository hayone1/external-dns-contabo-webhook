---
name: run-e2e
description: Run and debug the end-to-end test of the webhook in a real Kubernetes cluster, against the fake Contabo API (default, no account) or the real Contabo API. Use when asked to "run e2e", test in a cluster, or check a change before a release.
---

# Run the end-to-end test

The test lives in `test/e2e/run.sh`. It installs the external-dns Helm chart
with the webhook as a sidecar, creates Services, and checks the records.

| Mode | Talks to | Needs | Used by |
|---|---|---|---|
| `E2E_MODE=fake` (default) | fake API pod in the cluster (`test/e2e/fakecontabo`) | nothing | CI, every PR |
| `E2E_MODE=real` | api.contabo.com and a real zone | Contabo credentials, `E2E_ZONE` | maintainers, locally, before a release |

**Never put Contabo credentials into GitHub.** CI must stay on the fake.

## Steps

1. Build both images: `make e2e-images`
2. Make them available to the cluster:
   - kind: `kind load docker-image external-dns-contabo-webhook:e2e fake-contabo:e2e`
   - k3s over ssh: `docker save external-dns-contabo-webhook:e2e fake-contabo:e2e | ssh root@HOST k3s ctr -n k8s.io images import -`
3. Run:
   - fake: `make e2e`
   - real: ask the user which zone to use if `E2E_ZONE` is not set (never guess),
     load credentials with `set -a; . ./.env; set +a` (do not print them),
     then `E2E_MODE=real make e2e`.
   - `E2E_TEST_APEX=true` tests the bare domain. On by default with the fake.
     With `real`, only on a zone nothing else manages.
   - `E2E_KEEP=true` keeps the namespace for debugging. Clean up afterwards.

In fake mode, step 3 makes the next 15 deletes fail with HTTP 500
(`POST /_fake/faults`), like a real Contabo outage. Every record must still
be gone at the end, which proves no record is left without its ownership record.

## Reading a failure

The script prints the records it saw plus the last logs of every container.

| Symptom | Likely cause |
|---|---|
| `zone ... not found` | Fake: the fake pod got a different `-zones` value. Real: the zone does not exist in Contabo. |
| `fake Contabo API did not answer` | The fake image is not in the cluster, or the port-forward port (`E2E_FAKE_PORT`) is taken. |
| external-dns logs `No endpoints could be generated` | Annotations use the old `external-dns.alpha.kubernetes.io/` prefix. external-dns v0.23+ reads `external-dns.kubernetes.io/`. |
| external-dns logs `nodes is forbidden` | Chart installed with `namespaced: true`. The service source needs a ClusterRole. |
| webhook logs `skipping change: no Contabo zone found` with an apex hint | `txtPrefix` is missing or does not end in a dot. |
| `Invalid record name: ...*...` | `--txt-wildcard-replacement` is missing. |
| `must be valid external IPV4` | A target is a private IP. |
| `permanent diff` failure | `AdjustEndpoints` and `Records` disagree. Reproduce in `test/integration` first. |
| A real-mode failure that fake mode does not show | The fake is missing a real API quirk. Use the `probe-contabo-api` skill and update the fake. |

## Cleanup

The script deletes its namespace (and, in real mode, every record under
`e2e-<id>.<zone>`) on exit. If a real-mode run was killed, list leftover
records with the API and delete only names under `e2e-<id>.` or the
`extdns-` records that carry the test owner ID.
