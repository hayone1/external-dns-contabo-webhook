# Metrics and health checks

The webhook runs a small HTTP server on port **8080** (change with `HEALTH_PORT`).

## Health checks

| Path | Answers 200 when | Use it for |
|---|---|---|
| `/healthz` | the process is running and not shutting down | liveness probe |
| `/readyz` | the credentials worked and the webhook API is listening | readiness probe |

While the webhook cannot log in to Contabo (for example during a network
outage at startup), `/healthz` says ok but `/readyz` does not. Kubernetes
then waits instead of restarting the pod again and again.

## Prometheus metrics

`/metrics` serves Prometheus metrics. With the external-dns Helm chart, enable
`serviceMonitor` and the webhook is scraped too
(see `provider.webhook.serviceMonitor` in the chart values).

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `contabo_webhook_api_requests_total` | counter | `operation`, `code` | Calls to the Contabo API. `code="0"` means no answer (network error). |
| `contabo_webhook_api_request_duration_seconds` | histogram | `operation` | How long those calls took. |
| `contabo_webhook_api_retries_total` | counter | `operation`, `reason` | Retries, by HTTP status or `network`. |
| `contabo_webhook_api_ratelimit_remaining` | gauge | | Requests left in Contabo's current rate limit window. |
| `contabo_webhook_record_changes_total` | counter | `action`, `result` | Records created, updated and deleted, and whether it worked. |
| `contabo_webhook_records` | gauge | | Record sets returned to external-dns on the last read. |
| `contabo_webhook_zones` | gauge | | Zones that match the domain filter. |
| `contabo_webhook_last_success_timestamp_seconds` | gauge | | Time of the last successful read or write. |
| `contabo_webhook_auth_failures_total` | counter | | Failed logins to Contabo. |
| `contabo_webhook_skipped_endpoints_total` | counter | `reason` | Records dropped because Contabo cannot store them. |
| `contabo_webhook_build_info` | gauge | `version` | Always 1; tells you which version runs. |

Standard Go and process metrics (`go_*`, `process_*`) are there too.

## Alert ideas

```yaml
# No successful sync for 15 minutes
- alert: ContaboWebhookStale
  expr: time() - contabo_webhook_last_success_timestamp_seconds > 900
  for: 5m

# Login keeps failing
- alert: ContaboWebhookAuthFailing
  expr: increase(contabo_webhook_auth_failures_total[15m]) > 3

# Changes are failing
- alert: ContaboWebhookChangesFailing
  expr: increase(contabo_webhook_record_changes_total{result="error"}[15m]) > 0
```
