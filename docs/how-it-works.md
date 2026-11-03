# How it works

## The big picture

external-dns runs a loop, once a minute by default:

1. It asks the webhook: **"Which records exist?"** (`GET /records`)
2. It looks at your Services and Ingresses and works out which records
   **should** exist.
3. It asks the webhook to tidy that list up (`POST /adjustendpoints`).
4. It compares the two lists and makes a plan: what to create, update and delete.
5. It sends the plan to the webhook (`POST /records`), and the webhook makes
   the changes in Contabo.

The webhook only talks to external-dns on `localhost:8888`, inside the pod.
Nothing outside the pod can reach that port.

## One record versus many targets

external-dns thinks in **record sets**: one name and type with a list of values.
For example `www.example.com A [1.1.1.1, 1.0.0.1]`.

Contabo thinks in **single records**: one name, type and value each.

So the webhook translates:

- **Reading:** it groups Contabo's records with the same name and type into one
  record set.
- **Creating:** it creates one Contabo record per value.
- **Deleting:** it finds the Contabo record of each value and deletes it (with
  one bulk call when there are several).
- **Updating:** it compares old and new values.
  - A value that stays keeps its record. If only the TTL changed, the record is updated in place.
  - A value that is replaced is updated in place, so the name never goes
    without an answer. This also matters for CNAMEs, which Contabo allows only
    once per name.
  - Extra new values are created. Extra old values are deleted.

## The order of changes

Inside each zone the webhook does **deletes first, then updates, then
creates**. That way a name can switch from an A record to a CNAME: the A
record is gone before the CNAME arrives, and Contabo does not complain.

## Fresh data every time

Before it changes anything, the webhook reads the affected zones again. It
never trusts an old list of record IDs. If someone changed a record by hand
in the meantime, the webhook still finds the right one.

## When something goes wrong

- If one record fails (for example, Contabo refuses it), the webhook keeps
  going with the others and reports all errors at the end. external-dns
  tries the failed ones again on its next loop.
- If Contabo is busy (HTTP 429) or has an error (HTTP 5xx), the webhook waits
  and tries again, a few times, with growing pauses.
- Creating a record that already exists counts as success. So does deleting a
  record that is already gone. This makes retries safe.

## Names

Contabo accepts a record name in three spellings and treats them the same:
`www`, `www.example.com` and `www.example.com.`. The bare domain can be an
empty name or the zone name. The webhook understands all of them when it
reads, and always writes full names.

If you have two zones where one is inside the other (for example
`example.com` and `dev.example.com`), each name goes to the zone with the
longest match. `api.dev.example.com` goes to `dev.example.com`.

## Record types and target formats

| Type | Target format external-dns uses | Stored in Contabo as |
|---|---|---|
| A | `1.2.3.4` | data = IP |
| AAAA | `2606:4700::1111` | data = IP |
| CNAME | `target.example.net` | data = target |
| TXT | `"any text"` | data = text, exactly as given |
| MX | `10 mail.example.com` | prio = 10, data = host |
| SRV | `10 5 5060 sip.example.com` | prio, weight, port, data = target |
| CAA | `0 issue "letsencrypt.org"` | flag, tag, data = value |

NS, SOA and PTR are not supported. Contabo manages NS and SOA itself and
the webhook never touches them.

## What AdjustEndpoints does

Before external-dns compares lists, the webhook cleans up the "should exist"
list so it looks exactly like what Contabo will store. This is how the
webhook avoids a **permanent diff**, where external-dns would "fix" the same
record on every loop.

It:

- lower-cases names and removes trailing dots
- writes IP addresses in their shortest form (`2606:4700:0:0::1` becomes `2606:4700::1`)
- sets the default TTL when none is given
- removes duplicate values
- **drops** things Contabo would refuse, with a warning in the log:
  - A or AAAA values that are not public IPs
  - record types Contabo does not support (NS, PTR, ...)
  - endpoints with a `set-identifier` (Contabo has no routing policies)
  - a second CNAME value for the same name

## Ownership records

external-dns uses the **TXT registry**: next to each record it creates a TXT
record that says "external-dns owns this, cluster X". It only changes or
deletes records it owns. Records you made by hand are safe.

With `txtPrefix: "extdns-%{record_type}."` the ownership record of
`www.example.com` (type A) is `extdns-a.www.example.com`, and the one for
`example.com` itself is `extdns-a.example.com`. Both are inside your zone.
