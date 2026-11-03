# Deployment guide

This guide takes you from zero to working DNS records, step by step.

**You need:**

- a Kubernetes cluster and `kubectl` connected to it
- [Helm](https://helm.sh/docs/intro/install/) 3 or newer
- a Contabo account
- a domain name (it can be bought anywhere)

## Step 1: Move your domain's DNS to Contabo

DNS records only work if the internet asks Contabo's servers about your
domain. Two things make that happen.

**a) Create a DNS zone in Contabo.** A zone is the list of records for one
domain. Create one in the [Contabo customer panel](https://my.contabo.com/dns)
for your domain, for example `example.com`.

**b) Point your domain at Contabo's name servers.** Log in where you bought
the domain (your *registrar*). Find the setting called "nameservers" or "DNS
servers", choose "custom", and enter:

```text
ns1.contabo.net
ns2.contabo.net
ns3.contabo.net
```

This can take from a few minutes to a day to spread around the internet.
You can check with:

```sh
dig +short NS example.com
```

When you see the three `contabo.net` names, you are done.

> **Already have records somewhere else?** Copy them (mail records like MX,
> for example) into the Contabo zone **before** you switch the name servers,
> or they stop working.

## Step 2: Get API credentials

Go to [my.contabo.com/api/details](https://my.contabo.com/api/details). You
will see four values:

| On the page | What the webhook calls it |
|---|---|
| Client ID | `CONTABO_CLIENT_ID` |
| Client Secret | `CONTABO_CLIENT_SECRET` |
| API User (your email) | `CONTABO_API_USER` |
| API Password | `CONTABO_API_PASSWORD` |

The API password is **not** your login password. If you never set one,
set it on that same page.

> These credentials can manage your whole Contabo account, not only DNS.
> Keep them secret. If you can, create a separate Contabo user with only the
> DNS permissions and use its credentials.

## Step 3: Store the credentials in Kubernetes

```sh
kubectl create namespace external-dns

kubectl -n external-dns create secret generic contabo-credentials \
  --from-literal=client-id='INT-12345678' \
  --from-literal=client-secret='your-client-secret' \
  --from-literal=api-user='you@example.com' \
  --from-literal=api-password='your-api-password'
```

Tip: put a space before the command so your shell does not save it in its
history. Or use a tool like
[External Secrets Operator](https://external-secrets.io/) or
[Sealed Secrets](https://github.com/bitnami-labs/sealed-secrets).

## Step 4: Install external-dns with the webhook

Download [deploy/helm/values.yaml](../deploy/helm/values.yaml). Change the
lines marked `CHANGE ME`:

- `tag`: the webhook version, for example `0.1.0` (image tags have no `v`)
- `DOMAIN_FILTER` and `domainFilters`: your domain
- `txtOwnerId`: any name for this cluster, for example `prod-cluster`. If two
  clusters write to the same zone, each needs its own name.

Then install:

```sh
helm repo add external-dns https://kubernetes-sigs.github.io/external-dns/
helm repo update
helm upgrade --install external-dns external-dns/external-dns \
  --namespace external-dns -f values.yaml
```

Check that both containers are running (`2/2`):

```sh
kubectl -n external-dns get pods
kubectl -n external-dns logs deploy/external-dns -c webhook
```

You should see `Contabo credentials accepted` and `webhook server listening`.

## Step 5: Create a record

Pick any Service or Ingress and add a hostname:

```yaml
apiVersion: v1
kind: Service
metadata:
  name: my-app
  annotations:
    external-dns.kubernetes.io/hostname: app.example.com
    # Optional: how long DNS servers may cache the answer, in seconds
    external-dns.kubernetes.io/ttl: "300"
spec:
  type: LoadBalancer
  ports:
    - port: 80
  selector:
    app: my-app
```

For an Ingress, external-dns uses the hosts in the Ingress rules, so no
annotation is needed.

If your load balancer only has a private IP (common on a single server or
home lab), tell external-dns your public IP:

```yaml
    external-dns.kubernetes.io/target: "203.0.113.10"
```

After about a minute:

```sh
dig +short app.example.com @ns1.contabo.net
```

## Upgrading

1. Read the [release notes](https://github.com/hayone1/external-dns-contabo-webhook/releases).
2. Change `provider.webhook.image.tag` in your values file.
3. Run the same `helm upgrade --install` command again.

## Removing

```sh
helm uninstall external-dns --namespace external-dns
```

This stops external-dns but **leaves your DNS records in place**. To remove
records first, delete the Services or Ingresses (or their hostname
annotations) and wait one loop before you uninstall.

## Checking an image before you use it

Every image is signed. To check that it was built by this project's GitHub
workflow, install [cosign](https://docs.sigstore.dev/cosign/system_config/installation/)
version 3 or newer and run:

```sh
cosign verify ghcr.io/hayone1/external-dns-contabo-webhook:0.1.0 \
  --certificate-identity-regexp '^https://github.com/hayone1/external-dns-contabo-webhook/.github/workflows/release.yml@refs/tags/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

The same works for `docker.io/hayone/external-dns-contabo-webhook:0.1.0`.
cosign 2.x says `no signatures found`, because it cannot read the newer
signature format. Upgrade cosign.

## Without Helm

The webhook is a normal container. Run it as a second container in the
external-dns pod, set the environment variables from
[configuration.md](configuration.md), and start external-dns with:

```text
--provider=webhook
--webhook-provider-url=http://localhost:8888
--txt-prefix=extdns-%{record_type}.
--txt-wildcard-replacement=wildcard
--webhook-provider-read-timeout=30s
--webhook-provider-write-timeout=60s
```

Use `/healthz` on port 8080 for the liveness probe and `/readyz` for the
readiness probe.
