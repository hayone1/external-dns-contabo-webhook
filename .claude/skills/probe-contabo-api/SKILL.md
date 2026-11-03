---
name: probe-contabo-api
description: Safely check how the real Contabo DNS API behaves (name formats, error codes, quoting, limits) and record the findings in the fake API and docs. Use when a test against the fake passes but the real API behaves differently, or before relying on an undocumented behavior.
---

# Probe the Contabo API

The OpenAPI spec (https://api.contabo.com/api-v1.yaml) does not tell the
whole story. Check the real API before you depend on a behavior.

## Rules

- Use a zone the user named. Never create or delete zones unless the user asks.
- Use record names that start with `probe-` so they are easy to find and remove.
- Never print tokens or credentials. Load them from `.env` into the environment.
- Remove every probe record when you are done, and show the user the final record list.

## Helper

```sh
set -a; . ./.env; set +a
TOKEN=$(curl -sSf https://auth.contabo.com/auth/realms/contabo/protocol/openid-connect/token \
  --data-urlencode grant_type=password \
  --data-urlencode "client_id=$CONTABO_CLIENT_ID" --data-urlencode "client_secret=$CONTABO_CLIENT_SECRET" \
  --data-urlencode "username=$CONTABO_API_USER" --data-urlencode "password=$CONTABO_API_PASSWORD" | jq -r .access_token)

capi() { # METHOD PATH [JSON]
  local a=(-sS -X "$1" -H "Authorization: Bearer $TOKEN"
           -H "x-request-id: $(cat /proc/sys/kernel/random/uuid)" -w '\nHTTP %{http_code}\n')
  [ $# -ge 3 ] && a+=(-H 'Content-Type: application/json' --data "$3")
  curl "${a[@]}" "https://api.contabo.com$2"
}

capi GET "/v1/dns/zones/$ZONE/records?size=1000"
capi POST "/v1/dns/zones/$ZONE/records" '{"name":"probe-a","type":"A","ttl":300,"prio":0,"data":"1.1.1.1"}'
```

Tokens expire after 5 minutes. Fetch a new one when you get HTTP 401.
Use public IPs you are allowed to point at, such as `1.1.1.1`. The API
rejects private and documentation IPs.

## After probing

1. Write the finding in `docs/contabo-api.md` with the date.
2. Copy the behavior into `internal/contabo/contabotest/server.go`.
3. Add or change a test that depends on it.
4. Delete the probe records: collect their `recordId`s and call
   `capi DELETE "/v1/dns/zones/$ZONE/records/bulk" '{"recordIds":[1,2,3]}'`.
