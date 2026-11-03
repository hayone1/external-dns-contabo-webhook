# Changelog

All notable changes are listed here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[semantic versioning](https://semver.org/).

## [Unreleased]

## [0.1.0] - 2026-11-03

First release.

### Added

- ExternalDNS webhook provider for Contabo DNS, built on external-dns v0.24.
- Create, update and delete A, AAAA, CNAME, TXT, MX, SRV and CAA records.
- Domain filters: include, exclude and regular expressions.
- Dry-run mode.
- Contabo client with OAuth2 password grant, automatic re-login, rate
  limiting, retries with backoff for 429 and 5xx, and pagination.
- Desired endpoints Contabo would refuse (private IPs, NS/PTR, set-identifier)
  are dropped with a warning instead of failing the whole batch.
- Credentials can be read from files (`*_FILE` variables).
- `/healthz`, `/readyz` and Prometheus `/metrics` on port 8080.
- `check`, `healthcheck` and `version` commands.
- Signed multi-arch images on Docker Hub and GHCR, with SBOM and provenance.

[Unreleased]: https://github.com/hayone1/external-dns-contabo-webhook/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/hayone1/external-dns-contabo-webhook/releases/tag/v0.1.0
