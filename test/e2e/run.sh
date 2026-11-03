#!/usr/bin/env bash
# End-to-end test: real external-dns + this webhook in a real cluster.
#
# Two modes:
#
#   E2E_MODE=fake (default, used by CI)
#     Runs the fake Contabo API (test/e2e/fakecontabo) inside the cluster.
#     Needs no Contabo account. Also simulates a Contabo outage.
#
#   E2E_MODE=real
#     Talks to the real Contabo API and a real zone. Use it locally before a
#     release. Every name lives under e2e-<id>.<zone>, so it never touches
#     your real records.
#
# What it checks:
#   1. creating Services creates A, CNAME and TXT ownership records
#   2. changing the Services changes the records
#   3. external-dns stops changing things once it has caught up (no permanent diff)
#   4. deleting the Services deletes every record, even through an outage (fake mode)
#
# Environment:
#   E2E_MODE            fake (default) or real
#   E2E_IMAGE           webhook image already in the cluster (default external-dns-contabo-webhook:e2e)
#   E2E_FAKE_IMAGE      fake API image already in the cluster (default fake-contabo:e2e)
#   E2E_ZONE            zone to use. Fake mode: default example.com. Real mode: required,
#                       must already exist in Contabo.
#   CONTABO_CLIENT_ID, CONTABO_CLIENT_SECRET, CONTABO_API_USER, CONTABO_API_PASSWORD
#                       real mode only
#   E2E_TARGET_IPS      two public IPs, comma separated (default 1.1.1.1,1.0.0.1)
#   E2E_TEST_APEX       "true" to also test the bare domain. Default: true in fake
#                       mode, false in real mode (only use it on a zone nothing else manages).
#   E2E_ID              unique run ID (default: random)
#   E2E_KEEP            "true" to keep the namespace for debugging
#   E2E_CHART_VERSION   external-dns chart version (default 1.23.0)
#   E2E_FAKE_PORT       local port for the fake API (default 18080)
#   KUBECONFIG          cluster to use (the current context is used)
#
# Tools: kubectl, helm, curl, jq. In real mode dig is used when installed.

set -euo pipefail

MODE=${E2E_MODE:-fake}
case "$MODE" in
  fake)
    # Fixed credentials of the fake API (internal/contabo/contabotest). Not secret.
    CONTABO_CLIENT_ID=INT-test
    CONTABO_CLIENT_SECRET=test-secret
    CONTABO_API_USER=user@example.com
    CONTABO_API_PASSWORD=test-password
    E2E_ZONE=${E2E_ZONE:-example.com}
    APEX=${E2E_TEST_APEX:-true}
    ;;
  real)
    : "${CONTABO_CLIENT_ID:?}" "${CONTABO_CLIENT_SECRET:?}" "${CONTABO_API_USER:?}" "${CONTABO_API_PASSWORD:?}"
    : "${E2E_ZONE:?set E2E_ZONE to a zone that exists in Contabo}"
    APEX=${E2E_TEST_APEX:-false}
    ;;
  *) echo "E2E_MODE must be fake or real" >&2; exit 2 ;;
esac

ZONE=$(echo "$E2E_ZONE" | tr '[:upper:]' '[:lower:]')
ID=${E2E_ID:-$(od -An -N4 -tx4 /dev/urandom | tr -dc '0-9a-f')}
BASE="e2e-${ID}.${ZONE}"
NS="extdns-e2e-${ID}"
RELEASE="external-dns-e2e-${ID}"
OWNER="e2e-${ID}"
IMAGE=${E2E_IMAGE:-external-dns-contabo-webhook:e2e}
FAKE_IMAGE=${E2E_FAKE_IMAGE:-fake-contabo:e2e}
IFS=, read -r IP1 IP2 <<<"${E2E_TARGET_IPS:-1.1.1.1,1.0.0.1}"
CHART_VERSION=${E2E_CHART_VERSION:-1.23.0}
HERE=$(cd "$(dirname "$0")" && pwd)
TIMEOUT=300
PF_PID=""

if [ "$MODE" = "fake" ]; then
  # The script reaches the fake through a port-forward; the webhook through the Service.
  API_URL="http://127.0.0.1:${E2E_FAKE_PORT:-18080}"
  AUTH_URL="$API_URL/token"
  WEBHOOK_API_URL="http://contabo-fake.${NS}.svc.cluster.local:8080"
  WEBHOOK_AUTH_URL="$WEBHOOK_API_URL/token"
else
  API_URL=${CONTABO_API_URL:-https://api.contabo.com}
  AUTH_URL=${CONTABO_AUTH_URL:-https://auth.contabo.com/auth/realms/contabo/protocol/openid-connect/token}
  WEBHOOK_API_URL=$API_URL
  WEBHOOK_AUTH_URL=$AUTH_URL
fi

log()  { printf '\n\033[1;34m==> %s\033[0m\n' "$*"; }
pass() { printf '\033[1;32m    ok:\033[0m %s\n' "$*"; }
fail() { printf '\033[1;31m    FAIL:\033[0m %s\n' "$*" >&2; dump; exit 1; }

for tool in kubectl helm curl jq; do
  command -v "$tool" >/dev/null || { echo "missing tool: $tool" >&2; exit 2; }
done

# ---------------------------------------------------------------- Contabo API

TOKEN=""
TOKEN_AT=0
token() {
  local now; now=$(date +%s)
  if [ -z "$TOKEN" ] || [ $((now - TOKEN_AT)) -gt 200 ]; then
    TOKEN=$(curl -sSf --retry 4 --retry-delay 3 --retry-all-errors "$AUTH_URL" \
      --data-urlencode grant_type=password \
      --data-urlencode "client_id=$CONTABO_CLIENT_ID" \
      --data-urlencode "client_secret=$CONTABO_CLIENT_SECRET" \
      --data-urlencode "username=$CONTABO_API_USER" \
      --data-urlencode "password=$CONTABO_API_PASSWORD" | jq -r .access_token)
    TOKEN_AT=$now
  fi
  printf '%s' "$TOKEN"
}

api() { # METHOD PATH [BODY]
  # Retry on errors: the Contabo API has short outages, and a failed read
  # must never look like "no records".
  local args=(-sSf --retry 4 --retry-delay 3 --retry-all-errors -X "$1"
              -H "Authorization: Bearer $(token)" -H "x-request-id: $(cat /proc/sys/kernel/random/uuid)")
  [ $# -ge 3 ] && args+=(-H 'Content-Type: application/json' --data "$3")
  curl "${args[@]}" "$API_URL$2"
}

# Prints "name type data ttl" for every record that belongs to this run.
records() {
  api GET "/v1/dns/zones/${ZONE}/records?size=1000" | jq -r --arg z "$ZONE" --arg b "$BASE" --arg apex "$APEX" '
    def fqdn: (.name // "") | ascii_downcase | rtrimstr(".")
      | if . == "" or . == $z then $z elif endswith("." + $z) then . else . + "." + $z end;
    .data[] | select(.type != "NS" and .type != "SOA")
    | (fqdn) as $n
    | select($n == $b or ($n | endswith("." + $b))
             or ($apex == "true" and ($n == $z or ($n | test("^extdns-[a-z]+\\." + ($z | gsub("\\."; "\\.")) + "$")))))
    | "\($n) \(.type) \(.data) \(.ttl)"' | sort
}

# IDs of records this run created: everything under BASE, plus the apex
# records of the apex test (matched by target IP and owner).
record_ids() {
  api GET "/v1/dns/zones/${ZONE}/records?size=1000" | jq -r --arg z "$ZONE" --arg b "$BASE" \
      --arg apex "$APEX" --arg ip "$IP1" --arg owner "$OWNER" '
    def fqdn: (.name // "") | ascii_downcase | rtrimstr(".")
      | if . == "" or . == $z then $z elif endswith("." + $z) then . else . + "." + $z end;
    .data[] | select(.type != "NS" and .type != "SOA") | (fqdn) as $n
    | select($n == $b or ($n | endswith("." + $b))
             or ($apex == "true" and $n == $z and .type == "A" and .data == $ip)
             or ($apex == "true" and .type == "TXT" and (.data | contains("owner=" + $owner))))
    | .recordId'
}

# wait_until DESCRIPTION COMMAND...: retry COMMAND until it succeeds.
wait_until() {
  local what=$1; shift
  local deadline=$(( $(date +%s) + TIMEOUT ))
  until "$@" >/dev/null 2>&1; do
    [ "$(date +%s)" -lt "$deadline" ] || fail "timed out waiting for: $what"
    sleep 5
  done
  pass "$what"
}

# These fail when the API call fails, so an outage never counts as a pass.
has()      { local r; r=$(records) || return 1; grep -qxF -- "$1" <<<"$r"; }
has_like() { local r; r=$(records) || return 1; grep -qE -- "$1" <<<"$r"; }
missing()  { local r; r=$(records) || return 1; ! grep -qE -- "$1" <<<"$r"; }
empty()    { local r; r=$(records) || return 1; [ -z "$r" ]; }

# dig_has NAME TYPE VALUE: ask Contabo's name servers directly. Uses IPv4
# (many CI runners have no IPv6) and falls back to TCP when UDP is blocked.
# Succeeds if any server returns VALUE.
DNS_REACHABLE=unknown
dig_has() {
  local ns out proto reached=false
  for ns in ns1.contabo.net ns2.contabo.net ns3.contabo.net; do
    for proto in +notcp +tcp; do
      if out=$(dig -4 "$proto" +short +norec +time=3 +tries=1 "@$ns" "$1" "$2" 2>/dev/null); then
        reached=true
        grep -qiF -- "$3" <<<"$out" && { DNS_REACHABLE=true; return 0; }
        break
      fi
    done
  done
  $reached && DNS_REACHABLE=true || DNS_REACHABLE=false
  return 1
}

# check_dns NAME TYPE VALUE: real mode only. Waits for the answer, but only
# warns when this machine cannot reach any Contabo name server at all.
check_dns() {
  if [ "$MODE" != "real" ]; then
    return
  fi
  if ! command -v dig >/dev/null; then
    echo "    skip: dig is not installed, DNS check skipped"
    return
  fi
  dig_has "$@" || true
  if [ "$DNS_REACHABLE" = "false" ]; then
    echo "    skip: cannot reach any Contabo name server from here, DNS check skipped"
    return
  fi
  wait_until "Contabo name servers answer $1 $2 $3" dig_has "$@"
}

# ------------------------------------------------------------------- cluster

webhook_logs() { kubectl -n "$NS" logs "deploy/${RELEASE}" -c webhook --tail=-1 2>/dev/null || true; }
mutations()    { webhook_logs | grep -cE 'msg="(created record|updated record|deleted records)"' || true; }

dump() {
  echo "---- records ----"; records || true
  echo "---- pods ----"; kubectl -n "$NS" get pods -o wide || true
  echo "---- external-dns (last 60 lines) ----"; kubectl -n "$NS" logs "deploy/${RELEASE}" -c external-dns --tail=60 || true
  echo "---- webhook (last 60 lines) ----"; kubectl -n "$NS" logs "deploy/${RELEASE}" -c webhook --tail=60 || true
  if [ "$MODE" = "fake" ]; then
    echo "---- fake Contabo API (last 30 lines) ----"; kubectl -n "$NS" logs deploy/contabo-fake --tail=30 || true
  fi
}

cleanup() {
  local code=$?
  if [ "${E2E_KEEP:-false}" = "true" ]; then
    log "E2E_KEEP=true, leaving namespace $NS and records in place"
    exit "$code"
  fi
  log "cleaning up"
  helm uninstall "$RELEASE" --namespace "$NS" --wait >/dev/null 2>&1 || true
  if [ "$MODE" = "real" ]; then
    # Remove anything the test left behind under its own name.
    local ids; ids=$(record_ids 2>/dev/null | paste -sd, - || true)
    if [ -n "$ids" ]; then
      echo "    deleting leftover record IDs: $ids"
      api DELETE "/v1/dns/zones/${ZONE}/records/bulk" "{\"recordIds\":[${ids}]}" >/dev/null || true
    fi
  fi
  [ -n "$PF_PID" ] && kill "$PF_PID" 2>/dev/null || true
  kubectl delete namespace "$NS" --wait=false --ignore-not-found >/dev/null 2>&1 || true
  exit "$code"
}
trap cleanup EXIT

# expect_stable: external-dns must not change anything for a few loops.
expect_stable() {
  local before after
  sleep 15 # let in-flight changes finish
  before=$(mutations)
  sleep 40 # four 10s loops
  after=$(mutations)
  [ "$before" = "$after" ] || fail "external-dns kept changing records ($before -> $after changes): permanent diff"
  pass "no changes over four loops (no permanent diff)"
}

service() { # NAME HOSTNAME TARGETS [TTL]
  kubectl -n "$NS" apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Service
metadata:
  name: $1
  annotations:
    external-dns.kubernetes.io/hostname: "$2"
    external-dns.kubernetes.io/target: "$3"
    external-dns.kubernetes.io/ttl: "${4:-300}"
spec:
  type: ClusterIP
  ports: [{ port: 80, targetPort: 80 }]
EOF
}

external_name() { # NAME HOSTNAME TARGET
  kubectl -n "$NS" apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Service
metadata:
  name: $1
  annotations:
    external-dns.kubernetes.io/hostname: "$2"
spec:
  type: ExternalName
  externalName: "$3"
EOF
}

start_fake() {
  kubectl -n "$NS" apply -f - >/dev/null <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: contabo-fake
spec:
  replicas: 1
  selector: { matchLabels: { app: contabo-fake } }
  template:
    metadata:
      labels: { app: contabo-fake }
    spec:
      containers:
        - name: fake
          image: "${FAKE_IMAGE}"
          imagePullPolicy: IfNotPresent
          args: ["-zones", "${ZONE}"]
          ports: [{ containerPort: 8080 }]
          readinessProbe:
            httpGet: { path: /healthz, port: 8080 }
          securityContext:
            runAsNonRoot: true
            readOnlyRootFilesystem: true
            allowPrivilegeEscalation: false
            capabilities: { drop: ["ALL"] }
---
apiVersion: v1
kind: Service
metadata:
  name: contabo-fake
spec:
  selector: { app: contabo-fake }
  ports: [{ port: 8080, targetPort: 8080 }]
EOF
  kubectl -n "$NS" rollout status deploy/contabo-fake --timeout=2m >/dev/null
  kubectl -n "$NS" port-forward svc/contabo-fake "${E2E_FAKE_PORT:-18080}:8080" >/dev/null 2>&1 &
  PF_PID=$!
  local i
  for i in $(seq 1 30); do
    curl -sf "$API_URL/healthz" >/dev/null && { pass "fake Contabo API is running"; return; }
    sleep 1
  done
  fail "fake Contabo API did not answer on $API_URL"
}

# fake_outage COUNT: the next COUNT record deletes fail with HTTP 500, like
# the real outage seen on 2026-11-03.
fake_outage() {
  curl -sSf -X POST "$API_URL/_fake/faults" -H 'Content-Type: application/json' \
    --data "{\"method\":\"DELETE\",\"path\":\"/records/\",\"status\":500,\"count\":$1}" >/dev/null
}

# ---------------------------------------------------------------------- run

log "run ${ID} (${MODE} mode): zone ${ZONE}, names under ${BASE}, image ${IMAGE}"
kubectl create namespace "$NS" >/dev/null
if [ "$MODE" = "fake" ]; then
  start_fake
fi
api GET "/v1/dns/zones/${ZONE}" >/dev/null || { echo "zone ${ZONE} not found" >&2; exit 2; }
[ -z "$(records)" ] || { echo "records already exist under ${BASE}" >&2; exit 2; }

log "installing external-dns ${CHART_VERSION} with the webhook"
jq -n --arg ns "$NS" --arg id "$CONTABO_CLIENT_ID" --arg secret "$CONTABO_CLIENT_SECRET" \
      --arg user "$CONTABO_API_USER" --arg pw "$CONTABO_API_PASSWORD" '{
  apiVersion: "v1", kind: "Secret", type: "Opaque",
  metadata: { name: "contabo-credentials", namespace: $ns },
  stringData: { "client-id": $id, "client-secret": $secret, "api-user": $user, "api-password": $pw }
}' | kubectl apply -f - >/dev/null

helm repo add external-dns https://kubernetes-sigs.github.io/external-dns/ --force-update >/dev/null
filter=$BASE
[ "$APEX" = "true" ] && filter=$ZONE
overrides=$(mktemp)
cat >"$overrides" <<EOF
provider:
  webhook:
    image:
      repository: "${IMAGE%:*}"
      tag: "${IMAGE##*:}"
    env:
      - name: CONTABO_CLIENT_ID
        valueFrom: { secretKeyRef: { name: contabo-credentials, key: client-id } }
      - name: CONTABO_CLIENT_SECRET
        valueFrom: { secretKeyRef: { name: contabo-credentials, key: client-secret } }
      - name: CONTABO_API_USER
        valueFrom: { secretKeyRef: { name: contabo-credentials, key: api-user } }
      - name: CONTABO_API_PASSWORD
        valueFrom: { secretKeyRef: { name: contabo-credentials, key: api-password } }
      - { name: CONTABO_API_URL, value: "${WEBHOOK_API_URL}" }
      - { name: CONTABO_AUTH_URL, value: "${WEBHOOK_AUTH_URL}" }
      - { name: CONTABO_DEFAULT_TTL, value: "300" }
      - { name: LOG_LEVEL, value: debug }
txtOwnerId: "${OWNER}"
domainFilters: ["${filter}"]
extraArgs:
  - --txt-wildcard-replacement=wildcard
  - --webhook-provider-read-timeout=30s
  - --webhook-provider-write-timeout=60s
  - --namespace=${NS}
EOF
helm upgrade --install "$RELEASE" external-dns/external-dns \
  --namespace "$NS" --version "$CHART_VERSION" --wait --timeout 5m \
  -f "$HERE/values.yaml" -f "$overrides" >/dev/null
rm -f "$overrides"
pass "external-dns is running"

log "step 1: create records"
service web "www.${BASE}" "$IP1"
service wild "*.wild.${BASE}" "$IP1"
external_name alias "alias.${BASE}" "www.${BASE}"
[ "$APEX" = "true" ] && service apex "${ZONE}" "$IP1"

wait_until "A www.${BASE} -> ${IP1}"            has "www.${BASE} A ${IP1} 300"
wait_until "A *.wild.${BASE} -> ${IP1}"         has "*.wild.${BASE} A ${IP1} 300"
wait_until "CNAME alias.${BASE} -> www.${BASE}" has_like "^alias\.${BASE//./\\.} CNAME www\.${BASE//./\\.}\.? "
wait_until "ownership TXT for www"              has_like "^extdns-a\.www\.${BASE//./\\.} TXT .*external-dns/owner=${OWNER}"
wait_until "ownership TXT for the wildcard"     has_like "^extdns-a\.wildcard\.wild\.${BASE//./\\.} TXT .*owner=${OWNER}"
if [ "$APEX" = "true" ]; then
  wait_until "A on the zone apex ${ZONE}"       has "${ZONE} A ${IP1} 300"
  wait_until "ownership TXT for the apex"       has_like "^extdns-a\.${ZONE//./\\.} TXT .*owner=${OWNER}"
fi
check_dns "www.${BASE}" A "$IP1"
expect_stable

log "step 2: update records"
service web "www.${BASE}" "${IP1},${IP2}" 120
external_name alias "alias.${BASE}" "wild.${BASE}"
wait_until "A www.${BASE} has ${IP1} with TTL 120" has "www.${BASE} A ${IP1} 120"
wait_until "A www.${BASE} has ${IP2} with TTL 120" has "www.${BASE} A ${IP2} 120"
wait_until "CNAME alias.${BASE} -> wild.${BASE}"   has_like "^alias\.${BASE//./\\.} CNAME wild\.${BASE//./\\.}\.? "
wait_until "old CNAME target is gone"              missing "^alias\.${BASE//./\\.} CNAME www\."
expect_stable

log "step 3: delete records"
if [ "$MODE" = "fake" ]; then
  # Enough failures to outlast the webhook's own retries, so external-dns
  # has to try again on a later loop.
  fake_outage 15
  pass "simulating a Contabo outage: the next 15 deletes fail with HTTP 500"
fi
kubectl -n "$NS" delete service web wild alias --wait >/dev/null
[ "$APEX" = "true" ] && kubectl -n "$NS" delete service apex --wait >/dev/null
wait_until "every record of this run is gone" empty

# Errors caused by Contabo itself (HTTP 5xx, 429) are expected now and then
# and are retried. Any other error means a bug.
transient='HTTP (5[0-9][0-9]|429)|could not delete record IDs|ownership record .* could not be created'
all_errors=$(webhook_logs | grep 'level=ERROR' || true)
real_errors=$(grep -vE "$transient" <<<"$all_errors" | sed '/^$/d' || true)
if [ -n "$real_errors" ]; then
  echo "$real_errors" | tail -20
  fail "webhook logged $(wc -l <<<"$real_errors") unexpected errors"
fi
n_transient=$(sed '/^$/d' <<<"$all_errors" | wc -l)
if [ "$n_transient" -gt 0 ]; then
  pass "webhook logged no unexpected errors ($n_transient retried Contabo API errors)"
else
  pass "webhook logged no errors"
fi

log "end-to-end test passed"
