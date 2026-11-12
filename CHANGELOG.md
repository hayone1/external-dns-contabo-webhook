# Changelog

All notable changes are listed here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[semantic versioning](https://semver.org/).

## [Unreleased]

## [0.1.1] - 2026-11-12

### Security

- Built with Go 1.27.4. Go 1.27.3 had GO-2026-6859, a bug that can crash an
  HTTP/1 server.
- Updated `golang.org/x/net` to v0.62.0 (GO-2026-6860, GO-2026-6866).
- The request log no longer contains line breaks from the request path, so
  a crafted URL can't add fake lines to the log.

### Fixed

- The fake Contabo API used in tests treats quoted and unquoted TXT values
  as duplicates, like the real API.

### Changed

- CI runs the end-to-end test against a fake Contabo API instead of a real
  account.
- Docs use the real image tags (no `v`) and explain the Docker Hub token
  scopes and the cosign 3 requirement.
- Updated `golang.org/x/sync`.

## [0.1.0] - 2026-11-03

First release.

### Added

- ExternalDNS webhook provider for Contabo DNS, built on external-dns v0.24.
- Create, update and delete A, AAAA, CNAME, TXT, MX, SRV and CAA records.
- Domain filters: include, exclude and regular expressions.
- Dry-run mode.
- Contabo client with OAuth2 password grant, automatic re-login, rate
  limiting, retries with backoff for 429 and 5xx, and pagination.
- Ownership-safe ordering: ownership TXT records are created before and
  deleted after the records they own, so a Contabo outage can never leave
  an orphaned record behind.
- Desired endpoints Contabo would refuse (private IPs, NS/PTR, set-identifier)
  are dropped with a warning instead of failing the whole batch.
- Credentials can be read from files (`*_FILE` variables).
- `/healthz`, `/readyz` and Prometheus `/metrics` on port 8080.
- `check`, `healthcheck` and `version` commands.
- Signed multi-arch images on Docker Hub and GHCR, with SBOM and provenance.

[Unreleased]: https://github.com/hayone1/external-dns-contabo-webhook/compare/v0.1.1...HEAD
[0.1.1]: https://github.com/hayone1/external-dns-contabo-webhook/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/hayone1/external-dns-contabo-webhook/releases/tag/v0.1.0
