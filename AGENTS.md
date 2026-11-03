# Notes for AI coding agents

This file tells AI agents (Claude Code, Codex, Copilot, Cursor, ...) how to
work in this repository. Humans may find it useful too.

## What this project is

An [ExternalDNS](https://github.com/kubernetes-sigs/external-dns) webhook
provider for Contabo DNS. external-dns runs it as a sidecar and calls it over
HTTP on `localhost:8888`. The webhook turns those calls into Contabo API
calls. Health and metrics are on `0.0.0.0:8080`.

## Layout

| Path | What is there |
|---|---|
| `cmd/webhook/main.go` | Startup, signals, `check` / `healthcheck` / `version` commands |
| `internal/config` | Environment variables, validation, `*_FILE` secrets |
| `internal/contabo` | Small Contabo DNS client: OAuth2 password grant, retries, rate limit, pagination |
| `internal/contabo/contabotest` | In-memory fake Contabo API that copies the real API's quirks |
| `internal/provider` | The `provider.Provider` implementation: `Records`, `AdjustEndpoints`, `ApplyChanges` |
| `internal/server` | HTTP servers; reuses the upstream external-dns webhook handlers |
| `internal/metrics` | Prometheus metrics |
| `test/integration` | Real external-dns planner + TXT registry + webhook client against the fake API |
| `test/e2e` | Real cluster + real Contabo zone test (`run.sh`) |
| `deploy/` | Example Helm values and Secret |
| `docs/` | User and maintainer docs |

## Commands

```sh
make test    # go test -race ./... (no network or Contabo account needed)
make lint    # golangci-lint v2
make fmt     # gofmt -s
make build
make e2e     # needs a cluster, Contabo credentials and E2E_ZONE; see docs/development.md
```

Run `make test` and `make lint` before you say a change is done.

## Rules

1. **Never print, log or commit secrets.** `.env`, `.mcp.json`, kubeconfigs and
   anything under `.notes/` or `_debug/` are private. Do not read `.env`
   values into the conversation; pass them to commands through the
   environment instead.
2. **Never create or delete Contabo zones** from code. Zone lifecycle belongs
   to the user.
3. **The fake API is the contract.** Contabo's real behavior is written down
   in `docs/contabo-api.md` and copied in `internal/contabo/contabotest`.
   If you learn something new about the real API, update both, plus a test.
4. **No permanent diff.** `test/integration` runs the external-dns loop
   twice and expects zero changes the second time. Keep it that way.
   `AdjustEndpoints` must produce exactly what `Records` returns.
5. **One bad record must not block the rest.** `ApplyChanges` keeps going and
   returns all errors at the end.
6. **Idempotent writes.** Create of an existing record and delete of a missing
   record are both success.
7. **Plain language in docs.** Write for a 9th grader: short sentences, explain
   terms, give copy-paste examples.
8. **Do not mention specific domain registrars** in docs or code. Say "your
   domain registrar".
9. **Pin GitHub Actions by full commit SHA** with the version in a comment.
   Every job sets its own minimal `permissions`.

## Contabo API facts that surprise people

- Record names are stored exactly as sent. `www`, `www.example.com` and
  `www.example.com.` are the same record. The apex can be an empty name, the
  zone name, or no name at all (`@` is refused). See `recordFQDN` in
  `internal/provider/mapping.go`.
- A and AAAA data must be a **public** IP. Private, CGNAT and documentation
  ranges get HTTP 400. `AdjustEndpoints` drops them early.
- `DELETE` with `Content-Type: application/json` and no body is a 400.
- `PATCH` on an unknown record ID is a 500, not a 404.
- Exact duplicates on create are a 409.
- Tokens live 300 seconds; refresh tokens 1800 seconds. The client re-runs the
  password grant instead of using refresh tokens.
- Rate limit: about 15 requests per second. The client defaults to 10.

## Skills

Step-by-step guides live in `.claude/skills/`:

- `run-e2e` - run and debug the end-to-end test
- `probe-contabo-api` - safely check how the real Contabo API behaves
- `add-record-type` - add or change how a record type is mapped
- `release` - cut a release and check the published image
