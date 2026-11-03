# Configuration

The webhook reads its settings from environment variables. Only the four
credentials are required.

## Credentials

| Variable | Required | What it is |
|---|---|---|
| `CONTABO_CLIENT_ID` | yes | Client ID from [my.contabo.com/api/details](https://my.contabo.com/api/details) |
| `CONTABO_CLIENT_SECRET` | yes | Client Secret from the same page |
| `CONTABO_API_USER` | yes | API User, which is your account email |
| `CONTABO_API_PASSWORD` | yes | API Password (not your login password) |

Each credential can also come from a file. Set `CONTABO_CLIENT_SECRET_FILE=/path/to/file`
instead of `CONTABO_CLIENT_SECRET`, for example when you mount a Secret as a
volume. Spaces and line breaks around the value are removed. Setting both
the value and the `_FILE` version is an error.

## Which records to manage

| Variable | Default | What it does |
|---|---|---|
| `DOMAIN_FILTER` | (all zones) | Comma-separated list of domains, like `example.com,example.org`. Only zones and records under these names are read and changed. |
| `EXCLUDE_DOMAIN_FILTER` | (none) | Comma-separated domains to leave alone, like `internal.example.com`. |
| `REGEX_DOMAIN_FILTER` | (none) | A regular expression for names to manage. Use instead of `DOMAIN_FILTER`, not together with it. |
| `REGEX_DOMAIN_EXCLUSION` | (none) | A regular expression for names to leave alone. |

You can name a sub-domain in `DOMAIN_FILTER`. `DOMAIN_FILTER=app.example.com`
works with a Contabo zone called `example.com`, and only touches names that
end in `app.example.com`.

Use the same domains here as in external-dns's `--domain-filter`
(`domainFilters` in Helm). external-dns also filters, but setting it on the
webhook too saves API calls.

## Behavior

| Variable | Default | What it does |
|---|---|---|
| `DRY_RUN` | `false` | `true` logs every change but sends nothing to Contabo. Good for a first test. |
| `CONTABO_DEFAULT_TTL` | `3600` | TTL in seconds for records that do not set one with the `external-dns.kubernetes.io/ttl` annotation. |
| `CONTABO_RATE_LIMIT` | `10` | Most API requests per second. Contabo allows about 15. |
| `CONTABO_MAX_RETRIES` | `5` | How often to retry a request that failed because Contabo was busy (HTTP 429) or had an error (HTTP 5xx). |
| `CONTABO_REQUEST_TIMEOUT` | `30s` | Time limit for one API request. Use Go duration format: `10s`, `1m`. |

## Network

| Variable | Default | What it does |
|---|---|---|
| `WEBHOOK_HOST` | `localhost` | Address of the API that external-dns calls. Keep `localhost` so only the pod can reach it. |
| `WEBHOOK_PORT` | `8888` | Port of that API. |
| `HEALTH_HOST` | `0.0.0.0` | Address of the health and metrics server. |
| `HEALTH_PORT` | `8080` | Port of the health and metrics server. The Helm chart expects 8080. |

## Advanced

| Variable | Default | What it does |
|---|---|---|
| `CONTABO_API_URL` | `https://api.contabo.com` | Contabo API address. Only change for testing. |
| `CONTABO_AUTH_URL` | `https://auth.contabo.com/auth/realms/contabo/protocol/openid-connect/token` | Contabo login address. Only change for testing. |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`. `debug` logs every API call (never the credentials). |
| `LOG_FORMAT` | `text` | `text` or `json`. |

## Recommended external-dns settings

These are external-dns settings, not webhook settings. The example
[values file](../deploy/helm/values.yaml) already has them.

| Helm value / flag | Recommended | Why |
|---|---|---|
| `txtPrefix` / `--txt-prefix` | `extdns-%{record_type}.` | Keeps ownership records for the bare domain inside the zone. **Must end in a dot.** |
| `--txt-wildcard-replacement` | `wildcard` | Contabo refuses `*` in the middle of a name. |
| `--webhook-provider-read-timeout` | `30s` | Large batches take time at Contabo's rate limit. |
| `--webhook-provider-write-timeout` | `60s` | Same reason. |
| `policy` / `--policy` | `sync` or `upsert-only` | `sync` deletes records you remove from Kubernetes. `upsert-only` never deletes. |
| `txtOwnerId` / `--txt-owner-id` | a unique name per cluster | Lets several clusters share one zone without fighting. |

## Commands

The image has a few extra commands. They read the same environment variables.

```sh
# Check the credentials and list the zones the webhook would manage
docker run --rm --env-file .env ghcr.io/hayone1/external-dns-contabo-webhook:0.1.0 check

# Print the version
docker run --rm ghcr.io/hayone1/external-dns-contabo-webhook:0.1.0 version
```

`healthcheck` is used by the image's Docker `HEALTHCHECK` and checks the local
`/healthz` endpoint.
