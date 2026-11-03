# Troubleshooting

First, look at both containers' logs:

```sh
kubectl -n external-dns logs deploy/external-dns -c webhook
kubectl -n external-dns logs deploy/external-dns -c external-dns
```

For more detail, set `LOG_LEVEL=debug` on the webhook (and `logLevel: debug`
in the Helm values for external-dns).

You can also test your credentials on their own:

```sh
docker run --rm \
  -e CONTABO_CLIENT_ID -e CONTABO_CLIENT_SECRET -e CONTABO_API_USER -e CONTABO_API_PASSWORD \
  ghcr.io/hayone1/external-dns-contabo-webhook:v0.1.0 check
```

It prints `login ok` and the list of zones.

## The webhook will not start

**`invalid configuration: CONTABO_CLIENT_ID is required`**
The Secret is missing or a key name is wrong. Compare the keys in your Secret
(`kubectl -n external-dns get secret contabo-credentials -o yaml`) with the
`secretKeyRef` keys in your Helm values.

**`Contabo rejected the credentials`**
One of the four values is wrong. Common mistakes:

- using your login password instead of the **API password**
- extra spaces or a line break copied into the Secret
- an old client secret after you reset it in the panel

**`cannot reach Contabo yet, retrying`**
The pod cannot reach `auth.contabo.com` or `api.contabo.com`. Check network
policies, proxies and DNS inside the cluster. The webhook keeps retrying and
stays "not ready" until it gets through.

## No records are created

**external-dns logs `No endpoints could be generated from 'service/...'`**
external-dns v0.23 and newer only read annotations that start with
`external-dns.kubernetes.io/`. Change `external-dns.alpha.kubernetes.io/hostname`
to `external-dns.kubernetes.io/hostname`, or start external-dns with
`--enable-legacy-annotation-prefix`.

**Webhook logs `dropping target: Contabo only accepts public IP addresses`**
Your Service or Ingress points to a private IP (like `10.0.0.5`). Contabo
refuses those. Add the annotation `external-dns.kubernetes.io/target: "<your public IP>"`.

**Webhook logs `skipping change: no Contabo zone found for this name`**
There is no Contabo zone for that name. Create the zone in the Contabo panel,
or fix the hostname. If the log also has a `hint` about the zone apex, see the next item.

**Records for the bare domain (`example.com`) are created but never deleted**
Your `txtPrefix` is missing or does not end in a dot. Without it, the
ownership record for `example.com` would be `a-example.com`, which is outside
the zone, so external-dns never knows it owns the record. Set
`txtPrefix: "extdns-%{record_type}."` and delete the stray record by hand once.

**`Invalid record name: extdns-a.*.example.com`**
Add `--txt-wildcard-replacement=wildcard` to external-dns's arguments.

**external-dns logs `nodes is forbidden`**
You installed the chart with `namespaced: true`. The Service source needs
to read Nodes, which a namespaced Role does not allow. Use `namespaced: false`,
and add `--namespace=<name>` to `extraArgs` if you want to limit which
namespace external-dns watches.

## Records keep changing on every loop

That is called a permanent diff. Please
[open a bug](https://github.com/hayone1/external-dns-contabo-webhook/issues/new?template=bug_report.yml)
with debug logs of both containers. Until it is fixed, check:

- the same name is not managed by two clusters with the same `txtOwnerId`
- the record was not also created by hand with different values

## Changes are slow or time out

**external-dns logs `context deadline exceeded` or `failed to apply changes with code 500`
after a timeout**
A big batch takes longer than external-dns waits. Raise
`--webhook-provider-read-timeout` and `--webhook-provider-write-timeout`
(see the example values). The webhook finishes the work anyway, and the next
loop picks up the rest.

**Webhook logs `retrying Contabo API request` with status 429**
You hit Contabo's rate limit. Lower `CONTABO_RATE_LIMIT`, or make external-dns
loop less often (`interval: 5m`). Several webhooks on the same Contabo account
share one limit.

## Still stuck?

[Open an issue](https://github.com/hayone1/external-dns-contabo-webhook/issues/new/choose).
Include your versions, your Helm values (without secrets) and the logs.
