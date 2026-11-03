# Contabo API notes

The [Contabo OpenAPI spec](https://api.contabo.com/api-v1.yaml) does not
tell you everything. These are the things we found by testing the real API,
last checked on **2026-11-03**. The fake server in
`internal/contabo/contabotest` copies all of them, so tests fail if the code
forgets one.

## Login

- OAuth2 **password grant** at
  `https://auth.contabo.com/auth/realms/contabo/protocol/openid-connect/token`
  with `client_id`, `client_secret`, `username` (API user) and `password` (API password).
- The access token lives **300 seconds**. The refresh token lives **1800 seconds**.
  The webhook runs the password grant again instead of using the refresh
  token, so a quiet period longer than 30 minutes never breaks it.
- Wrong user or password: HTTP 400 `invalid_grant`. Wrong client ID or secret: HTTP 401 `unauthorized_client`.

## Every request

- Needs `Authorization: Bearer <token>` and an `x-request-id` header with a
  UUID v4. Without `x-request-id` you get HTTP 400.
- A `DELETE` **without a body** must not send `Content-Type: application/json`,
  or you get HTTP 400 `Body cannot be empty when content-type is set to 'application/json'`.

## Rate limit

Response headers show the limit:

```text
x-ratelimit-limit-second: 15
x-ratelimit-limit-hour: 15000
ratelimit-remaining: 14
ratelimit-reset: 1
```

Too many requests get HTTP 429. The webhook sends at most 10 per second by
default and waits for `ratelimit-reset` when it gets a 429.

## Pagination

- `page` starts at 1. `size` is capped at **1000**; bigger values are cut down.
- `_pagination.totalPages` says how many pages there are. The `_links` values
  are sometimes odd (`page=0`, repeated `size`), so the webhook ignores them.

## Zones

- A new zone already holds 3 `NS` records (`ns1/ns2/ns3.contabo.net`) and one `SOA`.
- Their `prio`, `port`, `weight`, `flag` and `tag` fields are `null`.
- A missing zone is HTTP 404: `Entry DNS Zone not found by zone name = ...`.

## Record names

- The name is stored **exactly as sent**:
  - `www` (relative), `www.example.com` (full) and `www.example.com.` (full
    with dot) all work and are served as the same DNS name.
  - The API also returns them exactly as sent, so a reader must accept all three.
- The bare domain (apex) can be sent as `""`, as the zone name, or by leaving
  `name` out. Contabo's own NS records use the zone name. `@` is refused
  (`Invalid record name: @.example.com`).
- Names are case-insensitive in DNS, but the API keeps the case you sent.
- `PATCH` rewrites the stored name to the full name (`www` becomes `www.example.com`).
- `*` is only allowed as the first label (`*.apps` works, `x.*.apps` does not).

## Record data

- **A / AAAA** must be a public address. These are refused with HTTP 400
  `The data parameter must be valid external IPV4 address ...`:
  private ranges, `100.64.0.0/10`, `198.18.0.0/15`, `240.0.0.0/4`,
  `192.0.2.0/24` and other documentation ranges, `fd00::/8`, `fe80::/10`,
  `2001:db8::/32`. `3fff::/20` (newer IPv6 documentation range) is accepted.
- **TXT** is stored exactly as sent. `"abc"` and `abc` are both served as
  `"abc"` by the name servers. Values longer than 255 characters are split
  into several strings automatically.
- **CNAME** with or without a trailing dot both work.
- **CNAME conflicts:** a CNAME next to any other record of the same name is
  HTTP 400 `Conflicts with pre-existing RRset`. That includes the zone apex,
  which always has NS records.
- **TTL** may be as low as `1` (even `0` is accepted).
- **NS** and **PTR** cannot be created: `type must be one of the following values: A, AAAA, CAA, CNAME, MX, SRV, TXT`.

## Writing

- **Create** an exact duplicate (same name, type and data): HTTP **409**
  `Entry Record is not unique by name = ...`. The webhook treats this as success.
- **Update** (`PATCH`) needs `type`, `ttl`, `prio` and `data` every time.
  It does not accept `name`.
- **Update** of a record ID that does not exist: HTTP **500**, not 404.
  The webhook always reads fresh IDs before it updates, so this does not happen.
- **Delete** of a record ID that does not exist: HTTP 204, same as success.
- **Delete can fail for a while.** On 2026-11-03 single and bulk deletes
  answered HTTP 500 (or listed IDs in `failedIds`) for about 90 seconds, then
  worked again. The webhook retries, keeps ownership records of records it
  could not delete, and lets external-dns try again on the next loop.
- **Bulk delete** takes `{"recordIds": [...]}` and answers with `failedIds`.
  Unknown IDs are not reported as failed.

## Errors

The body has one of two shapes:

```json
{"statusCode": 400, "message": "Bad Request ..."}
{"statusCode": 400, "message": ["ttl must be ...", "type must be ..."]}
```
