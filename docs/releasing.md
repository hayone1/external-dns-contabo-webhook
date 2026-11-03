# Releasing

This page is for maintainers.

## How a release works

Pushing a tag like `v0.2.0` on `main` starts `.github/workflows/release.yml`:

1. **Checks** that the tag points to a commit on `main`.
2. **Tests**: unit and integration tests, then the end-to-end test (kind +
   fake Contabo API). Run the end-to-end test against the real Contabo API
   on your own machine first (`E2E_MODE=real`, see [development.md](development.md)).
3. **Builds** the image for `linux/amd64` and `linux/arm64`.
4. **Pushes** it to Docker Hub (`hayone/external-dns-contabo-webhook`) and
   GHCR (`ghcr.io/hayone1/external-dns-contabo-webhook`) with these tags:
   `0.2.0`, `v0.2.0`, `0.2`, `latest` (only for normal releases), `sha-<commit>`.
   From `v1.0.0` on, a `1` tag is added too.
5. **Signs** both images with cosign (keyless, through GitHub's OIDC token).
6. **Attaches** an SBOM and build provenance, and a GitHub attestation.
7. **Scans** the image with Trivy and uploads the results to the Security tab.
8. **Creates** the GitHub release with notes.
9. **Updates** the Docker Hub description from [dockerhub.md](dockerhub.md).

If any test fails, nothing is published.

A tag with a dash, like `v0.2.0-rc.1`, makes a pre-release: no `latest`
tag and no Docker Hub description update.

## Steps

1. Make sure CI on `main` is green.
2. Update `CHANGELOG.md` and the version in `deploy/helm/values.yaml` in a PR. Merge it.
3. Tag and push:

   ```sh
   git switch main && git pull
   git tag -a v0.2.0 -m "v0.2.0"
   git push origin v0.2.0
   ```

4. Watch the run under **Actions > Release**.

## Repository setup (once)

| Where | Name | What |
|---|---|---|
| Environment `release` | secret `DOCKER_USERNAME` | Docker Hub user name |
| Environment `release` | secret `DOCKER_PASSWORD` | Docker Hub access token with read and write scope |
| Environment `release` | secret `DOCKERHUB_DESCRIPTION_TOKEN` (optional) | Docker Hub token that may edit the repository page. Without it the page is not updated. |

The `release` environment only allows `v*` tags, so a pull request can never
read these secrets. There are no Contabo secrets: CI only uses the fake API.

## Versioning

We use [semantic versioning](https://semver.org/):

- **patch** (`0.1.0` to `0.1.1`): bug fixes only
- **minor** (`0.1.0` to `0.2.0`): new features; on `0.x`, also breaking changes
- **major** (`1.0.0` to `2.0.0`): breaking changes after 1.0

A breaking change is anything that makes a working setup stop working after
an upgrade: a renamed environment variable, a changed default, a removed feature.
