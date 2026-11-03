# Development

## Setup

Install [Go](https://go.dev/dl/) (the version in `go.mod`) and `make`. Then:

```sh
make help     # list every target
make test     # all tests, with the race detector
make lint     # golangci-lint
make build    # ./bin/webhook
```

## How the code is organized

```text
cmd/webhook/              program entry: config, servers, signals, extra commands
internal/config/          environment variables and validation
internal/contabo/         Contabo API client (login, retries, rate limit, paging)
internal/contabo/contabotest/  fake Contabo API for tests
internal/provider/        the external-dns provider: Records, AdjustEndpoints, ApplyChanges
internal/server/          HTTP servers and health checks
internal/metrics/         Prometheus metrics
test/integration/         real external-dns code against the webhook and the fake API
test/e2e/                 real cluster + real Contabo zone
deploy/                   example Helm values and Secret
```

## Three levels of tests

1. **Unit tests** (`internal/...`). Fast, no network.
2. **Integration tests** (`test/integration`). They wire the real
   external-dns planner, TXT registry and webhook HTTP client to our webhook
   server and the fake Contabo API, then run the reconcile loop twice. The
   second run must plan zero changes. This catches a "permanent diff" before
   it reaches a cluster.
3. **End-to-end test** (`test/e2e/run.sh`). Real external-dns Helm chart,
   real cluster, real Contabo zone.

`make test` runs levels 1 and 2. They need no account and no network.

## Running locally against Contabo

```sh
cp .env.example .env        # fill in your credentials
make check-config           # log in and list zones, changes nothing
DRY_RUN=true make run       # run the webhook; changes are only logged
```

The webhook then listens on `localhost:8888`. You can call it like external-dns does:

```sh
curl -H 'Accept: application/external.dns.webhook+json;version=1' localhost:8888/
curl localhost:8888/records
```

## The end-to-end test

You need a cluster in your current kube context, `kubectl`, `helm`, `curl`,
`jq`, and a Contabo zone you can use for tests.

```sh
cp .env.example .env                       # credentials and E2E_ZONE
docker build -t external-dns-contabo-webhook:e2e .
kind create cluster                        # or use any cluster
kind load docker-image external-dns-contabo-webhook:e2e
set -a; . ./.env; set +a
./test/e2e/run.sh
```

On k3s, load the image with:

```sh
docker save external-dns-contabo-webhook:e2e | ssh root@your-server k3s ctr -n k8s.io images import -
```

Every name the test creates is under `e2e-<random>.<zone>`, and it removes
everything when it ends, even when it fails. Options:

| Variable | What it does |
|---|---|
| `E2E_TEST_APEX=true` | also test a record on the bare domain. Only use a zone nothing else manages. |
| `E2E_KEEP=true` | keep the namespace and records to debug. |
| `E2E_TARGET_IPS` | the two public IPs records point to (default `1.1.1.1,1.0.0.1`). |
| `E2E_CHART_VERSION` | external-dns Helm chart version. |

In GitHub Actions the same script runs in a kind cluster (`.github/workflows/e2e.yml`).
It uses the `e2e` environment, which holds the `CONTABO_*` secrets and the
`E2E_ZONE` variable. It never runs for pull requests from forks.

## Changing behavior that touches Contabo

1. Check the real API first (see `.claude/skills/probe-contabo-api`).
2. Write down what you found in [contabo-api.md](contabo-api.md).
3. Make the fake server in `internal/contabo/contabotest` behave the same.
4. Write a test, then the code.

## Building the image

```sh
make docker-build               # your platform
make docker-buildx              # amd64 + arm64
```

The Dockerfile builds a static binary and copies it into
`gcr.io/distroless/static-debian13:nonroot`. Base images are pinned by digest.
