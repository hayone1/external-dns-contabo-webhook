---
name: add-record-type
description: Add support for a DNS record type, or change how an existing type is converted between external-dns targets and Contabo records. Use for MX, SRV or CAA format work, or when Contabo adds a new record type.
---

# Add or change a record type

Contabo stores one value per record. external-dns stores a list of target
strings per name and type. The conversion lives in
`internal/provider/mapping.go`.

## Steps

1. Check that Contabo accepts the type (the `type` enum of
   `CreateDnsZoneRecordRequest` in the OpenAPI spec) and probe it with the
   `probe-contabo-api` skill.
2. Add the type to `contabo.WritableTypes` in `internal/contabo/types.go` if needed.
3. Update all three functions so they agree:
   - `targetRequest`: target string to Contabo request (validate the input)
   - `recordTarget`: Contabo record to target string
   - `canonicalTarget`: the form used to compare two targets
4. Add cases to `TestTargetRoundTrip`, `TestTargetRequestErrors` and
   `TestCanonicalTarget` in `mapping_test.go`.
5. Add the type to the fake server's `writable` map if needed.
6. Add a scenario to `test/integration/loop_test.go` that creates, updates and
   deletes the type and checks that the second run plans zero changes.
7. Document the target format in `docs/how-it-works.md`.
8. Run `make test lint`.

## The golden rule

`recordTarget(recordFromRequest(targetRequest(t)))` must equal the target
that `AdjustEndpoints` hands back to external-dns. If it does not,
external-dns will "fix" the record on every loop, forever.
