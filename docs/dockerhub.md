# external-dns-contabo-webhook

Make Kubernetes keep your **Contabo DNS** records up to date, all by itself.

This image is a webhook provider for [ExternalDNS](https://github.com/kubernetes-sigs/external-dns).
It runs next to external-dns, in the same pod, and turns its requests into
calls to the [Contabo DNS API](https://api.contabo.com/). Add a hostname to a
Service or Ingress, and the record appears in your Contabo zone. Delete it,
and the record goes away.

- **Source code and docs:** https://github.com/hayone1/external-dns-contabo-webhook
- **Install guide:** https://github.com/hayone1/external-dns-contabo-webhook/blob/main/docs/deployment.md
- **All settings:** https://github.com/hayone1/external-dns-contabo-webhook/blob/main/docs/configuration.md
- **Troubleshooting:** https://github.com/hayone1/external-dns-contabo-webhook/blob/main/docs/troubleshooting.md
- **Releases and changelog:** https://github.com/hayone1/external-dns-contabo-webhook/releases
- **Report a bug:** https://github.com/hayone1/external-dns-contabo-webhook/issues

## Quick start with Helm

```sh
kubectl create namespace external-dns
kubectl -n external-dns create secret generic contabo-credentials \
  --from-literal=client-id='INT-12345678' \
  --from-literal=client-secret='...' \
  --from-literal=api-user='you@example.com' \
  --from-literal=api-password='...'

curl -LO https://raw.githubusercontent.com/hayone1/external-dns-contabo-webhook/main/deploy/helm/values.yaml
# edit values.yaml: set your domain, cluster name and image tag

helm repo add external-dns https://kubernetes-sigs.github.io/external-dns/
helm upgrade --install external-dns external-dns/external-dns \
  --namespace external-dns -f values.yaml
```

Get the four Contabo values at https://my.contabo.com/api/details.

## Supported tags

- `X.Y.Z` - one exact release (use this in production)
- `X.Y` - the newest patch of a minor version
- `latest` - the newest release (not recommended for production)

Platforms: `linux/amd64`, `linux/arm64`.

## About the image

- Built from source by GitHub Actions on every release
- Based on `gcr.io/distroless/static`: no shell, no package manager
- Runs as non-root user `65532`, works with a read-only root file system
- Signed with cosign (keyless), with SBOM and build provenance attached
- Scanned with Trivy

Ports: `8888` (webhook API, `localhost` only by default), `8080` (`/healthz`,
`/readyz`, `/metrics`).

The same image is also on GitHub: `ghcr.io/hayone1/external-dns-contabo-webhook`.

## License

Apache License 2.0. Not made by or connected to Contabo GmbH.
