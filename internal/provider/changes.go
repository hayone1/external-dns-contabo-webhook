package provider

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"sigs.k8s.io/external-dns/endpoint"
	"sigs.k8s.io/external-dns/plan"

	"github.com/hayone1/external-dns-contabo-webhook/internal/contabo"
)

// zoneChanges is the part of a plan.Changes that belongs to one zone.
type zoneChanges struct {
	create []*endpoint.Endpoint
	update []updatePair
	delete []*endpoint.Endpoint
}

type updatePair struct {
	old, new *endpoint.Endpoint
}

// ApplyChanges creates, updates and deletes records so that Contabo matches
// the plan.
//
// It re-reads every affected zone first, so it always works with the real
// record IDs, even if records were changed outside external-dns since the
// last Records call.
//
// The order of the work matters:
//
//  1. delete records
//  2. delete ownership TXT records, but keep the ones whose record could not
//     be deleted in step 1
//  3. update records
//  4. create ownership TXT records
//  5. create records, but skip the ones whose ownership record could not be
//     created in step 4
//
// Deleting first frees a name before a record of another type (for example
// a CNAME) takes it over. Steps 2 and 5 make sure a record never exists
// without the TXT record that tells external-dns it owns it. Without that, a
// failed delete would leave a record behind that external-dns could never
// clean up.
//
// One failing record does not stop the others. All errors are returned
// together at the end, so external-dns retries only what is still missing.
func (p *Provider) ApplyChanges(ctx context.Context, changes *plan.Changes) error {
	if changes == nil || !changes.HasChanges() {
		return nil
	}
	p.applyMu.Lock()
	defer p.applyMu.Unlock()

	_, all, err := p.managedZones(ctx)
	if err != nil {
		return err
	}

	byZone := map[string]*zoneChanges{}
	get := func(ep *endpoint.Endpoint) *zoneChanges {
		zone, ok := zoneFor(normalizeName(ep.DNSName), all)
		if !ok {
			args := []any{"name", ep.DNSName, "type", ep.RecordType}
			if hint := apexOwnershipHint(ep, all); hint != "" {
				args = append(args, "hint", hint)
			}
			p.log.Warn("skipping change: no Contabo zone found for this name", args...)
			p.metrics.ObserveSkipped("no_zone")
			return nil
		}
		zc := byZone[zone]
		if zc == nil {
			zc = &zoneChanges{}
			byZone[zone] = zc
		}
		return zc
	}

	for _, ep := range changes.Delete {
		if zc := get(ep); zc != nil {
			zc.delete = append(zc.delete, ep)
		}
	}
	for _, pair := range pairUpdates(changes.UpdateOld, changes.UpdateNew) {
		if zc := get(pair.new); zc != nil {
			zc.update = append(zc.update, pair)
		}
	}
	for _, ep := range changes.Create {
		if zc := get(ep); zc != nil {
			zc.create = append(zc.create, ep)
		}
	}
	if len(byZone) == 0 {
		return nil
	}

	zones := make([]string, 0, len(byZone))
	for z := range byZone {
		zones = append(zones, z)
	}
	slices.Sort(zones)
	states, err := p.readZones(ctx, zones)
	if err != nil {
		return err
	}

	type work struct {
		zone string
		zc   *zoneChanges
		idx  *recordIndex
	}
	works := make([]work, 0, len(states))
	for _, zs := range states {
		works = append(works, work{zone: zs.name, zc: byZone[zs.name], idx: newRecordIndex(zs.name, zs.records)})
	}

	var errs []error
	var failedDeletes, failedOwners []*endpoint.Endpoint

	// 1. Delete records.
	for _, w := range works {
		for _, ep := range w.zc.delete {
			if isOwnershipRecord(ep) {
				continue
			}
			if e := p.applyDelete(ctx, w.zone, w.idx, ep); len(e) > 0 {
				errs = append(errs, e...)
				failedDeletes = append(failedDeletes, ep)
			}
		}
	}
	// 2. Delete ownership records, unless their record is still there.
	for _, w := range works {
		for _, ep := range w.zc.delete {
			if !isOwnershipRecord(ep) {
				continue
			}
			if owned := ownedRecord(ep, failedDeletes); owned != nil {
				p.log.Warn("keeping ownership record because the record it owns could not be deleted",
					"ownership", ep.DNSName, "record", describe(owned))
				p.metrics.ObserveSkipped("ownership_kept")
				continue
			}
			errs = append(errs, p.applyDelete(ctx, w.zone, w.idx, ep)...)
		}
	}
	// 3. Update records.
	for _, w := range works {
		for _, pair := range w.zc.update {
			errs = append(errs, p.applyUpdate(ctx, w.zone, w.idx, pair)...)
		}
	}
	// 4. Create ownership records.
	for _, w := range works {
		for _, ep := range w.zc.create {
			if !isOwnershipRecord(ep) {
				continue
			}
			if e := p.applyCreate(ctx, w.zone, w.idx, ep); len(e) > 0 {
				errs = append(errs, e...)
				failedOwners = append(failedOwners, ep)
			}
		}
	}
	// 5. Create records, unless their ownership record is missing.
	for _, w := range works {
		for _, ep := range w.zc.create {
			if isOwnershipRecord(ep) {
				continue
			}
			if owner := ownershipOf(ep, failedOwners); owner != nil {
				p.metrics.ObserveSkipped("owner_missing")
				errs = append(errs, fmt.Errorf("not creating %s: its ownership record %s could not be created", describe(ep), owner.DNSName))
				continue
			}
			errs = append(errs, p.applyCreate(ctx, w.zone, w.idx, ep)...)
		}
	}

	if err := errors.Join(errs...); err != nil {
		return err
	}
	p.metrics.MarkSuccess()
	return nil
}

// pairUpdates matches UpdateOld and UpdateNew entries by name and type.
// external-dns keeps them in the same order, but matching by key is safer.
func pairUpdates(oldEps, newEps []*endpoint.Endpoint) []updatePair {
	type k struct {
		key endpointKey
		set string
	}
	olds := map[k]*endpoint.Endpoint{}
	for _, ep := range oldEps {
		olds[k{keyOf(ep), ep.SetIdentifier}] = ep
	}
	pairs := make([]updatePair, 0, len(newEps))
	for _, ep := range newEps {
		old := olds[k{keyOf(ep), ep.SetIdentifier}]
		if old == nil {
			old = &endpoint.Endpoint{DNSName: ep.DNSName, RecordType: ep.RecordType}
		}
		pairs = append(pairs, updatePair{old: old, new: ep})
	}
	return pairs
}

// recordIndex finds existing records by name, type and canonical target.
type recordIndex struct {
	zone  string
	byKey map[endpointKey][]contabo.Record
}

func newRecordIndex(zone string, records []contabo.Record) *recordIndex {
	idx := &recordIndex{zone: zone, byKey: map[endpointKey][]contabo.Record{}}
	for _, r := range records {
		if !supportedType(r.Type) {
			continue
		}
		k := endpointKey{name: recordFQDN(r.Name, zone), recordType: r.Type}
		idx.byKey[k] = append(idx.byKey[k], r)
	}
	return idx
}

// find returns every record of the set whose target matches.
func (idx *recordIndex) find(k endpointKey, target string) []contabo.Record {
	want := canonicalTarget(k.recordType, target)
	var out []contabo.Record
	for _, r := range idx.byKey[k] {
		if t, ok := recordTarget(r); ok && canonicalTarget(r.Type, t) == want {
			out = append(out, r)
		}
	}
	return out
}

func (idx *recordIndex) add(k endpointKey, r contabo.Record) {
	idx.byKey[k] = append(idx.byKey[k], r)
}

func (idx *recordIndex) remove(k endpointKey, id contabo.Number) {
	idx.byKey[k] = slices.DeleteFunc(idx.byKey[k], func(r contabo.Record) bool { return r.ID == id })
}

func (p *Provider) applyDelete(ctx context.Context, zone string, idx *recordIndex, ep *endpoint.Endpoint) []error {
	k := keyOf(ep)
	var ids []int64
	for _, t := range ep.Targets {
		for _, r := range idx.find(k, t) {
			if !slices.Contains(ids, int64(r.ID)) {
				ids = append(ids, int64(r.ID))
			}
		}
	}
	if len(ids) == 0 {
		p.log.Debug("nothing to delete, records are already gone", "record", describe(ep))
		return nil
	}
	return p.deleteIDs(ctx, zone, idx, k, ids, describe(ep))
}

func (p *Provider) deleteIDs(ctx context.Context, zone string, idx *recordIndex, k endpointKey, ids []int64, what string) []error {
	log := p.log.With("zone", zone, "record", what)
	if p.dryRun {
		log.Info("[dry run] would delete records", "ids", ids)
		return nil
	}

	var err error
	if len(ids) == 1 {
		err = p.api.DeleteRecord(ctx, zone, ids[0])
	} else {
		var failed []int64
		failed, err = p.api.BulkDeleteRecords(ctx, zone, ids)
		if err == nil && len(failed) > 0 {
			err = fmt.Errorf("contabo could not delete record IDs %v", failed)
		}
	}
	p.metrics.ObserveChange("delete", err)
	if err != nil {
		log.Error("failed to delete records", "ids", ids, "error", err)
		return []error{fmt.Errorf("delete %s in zone %s: %w", what, zone, err)}
	}
	for _, id := range ids {
		idx.remove(k, contabo.Number(id))
	}
	log.Info("deleted records", "ids", ids)
	return nil
}

func (p *Provider) applyCreate(ctx context.Context, zone string, idx *recordIndex, ep *endpoint.Endpoint) []error {
	k := keyOf(ep)
	ttl := p.ttlOf(ep)
	var errs []error
	for _, t := range ep.Targets {
		if len(idx.find(k, t)) > 0 {
			p.log.Debug("record already exists, skipping create", "zone", zone, "name", k.name, "type", k.recordType, "target", t)
			continue
		}
		if err := p.createOne(ctx, zone, idx, k, t, ttl); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

func (p *Provider) createOne(ctx context.Context, zone string, idx *recordIndex, k endpointKey, target string, ttl int64) error {
	log := p.log.With("zone", zone, "name", k.name, "type", k.recordType, "target", target, "ttl", ttl)
	req, err := targetRequest(k.name, k.recordType, target, ttl)
	if err != nil {
		p.metrics.ObserveChange("create", err)
		log.Error("cannot create record", "error", err)
		return err
	}
	if p.dryRun {
		log.Info("[dry run] would create record")
		return nil
	}
	rec, err := p.api.CreateRecord(ctx, zone, req)
	if contabo.IsConflict(err) {
		// The same record already exists, which is what we wanted.
		log.Info("record already exists")
		return nil
	}
	p.metrics.ObserveChange("create", err)
	if err != nil {
		log.Error("failed to create record", "error", err)
		return fmt.Errorf("create %s %s %q in zone %s: %w", k.name, k.recordType, target, zone, err)
	}
	if rec.ID == 0 {
		rec = recordFromRequest(req)
	}
	idx.add(k, rec)
	log.Info("created record", "id", int64(rec.ID))
	return nil
}

// applyUpdate makes the record set match the new endpoint:
//
//   - targets in both old and new keep their record; it is patched if the TTL
//     changed
//   - each removed target is paired with an added target and the record is
//     patched in place, so the name never goes without an answer
//   - added targets left over are created
//   - removed targets left over are deleted
//
// Only records whose targets external-dns knew about (the old endpoint) are
// removed. Anything added by hand since the last read is left alone.
func (p *Provider) applyUpdate(ctx context.Context, zone string, idx *recordIndex, pair updatePair) []error {
	k := keyOf(pair.new)
	ttl := p.ttlOf(pair.new)

	desired := map[string]string{} // canonical -> target as sent by external-dns
	var desiredOrder []string
	for _, t := range pair.new.Targets {
		c := canonicalTarget(k.recordType, t)
		if _, dup := desired[c]; !dup {
			desired[c] = t
			desiredOrder = append(desiredOrder, c)
		}
	}
	known := map[string]bool{}
	for _, t := range pair.old.Targets {
		known[canonicalTarget(k.recordType, t)] = true
	}

	var keep, removed []contabo.Record
	present := map[string]bool{}
	for _, r := range idx.byKey[k] {
		t, ok := recordTarget(r)
		if !ok {
			continue
		}
		c := canonicalTarget(k.recordType, t)
		_, wanted := desired[c]
		switch {
		case wanted && !present[c]:
			present[c] = true
			keep = append(keep, r)
		case wanted:
			removed = append(removed, r) // a second record with the same value
		case known[c]:
			removed = append(removed, r)
		}
	}
	var added []string
	for _, c := range desiredOrder {
		if !present[c] {
			added = append(added, desired[c])
		}
	}

	var errs []error
	for _, r := range keep {
		if int64(r.TTL) == ttl {
			continue
		}
		t, _ := recordTarget(r)
		if err := p.patchOne(ctx, zone, idx, k, r, t, ttl); err != nil {
			errs = append(errs, err)
		}
	}
	for len(removed) > 0 && len(added) > 0 {
		r, t := removed[0], added[0]
		removed, added = removed[1:], added[1:]
		if err := p.patchOne(ctx, zone, idx, k, r, t, ttl); err != nil {
			errs = append(errs, err)
		}
	}
	for _, t := range added {
		if err := p.createOne(ctx, zone, idx, k, t, ttl); err != nil {
			errs = append(errs, err)
		}
	}
	if len(removed) > 0 {
		ids := make([]int64, 0, len(removed))
		for _, r := range removed {
			ids = append(ids, int64(r.ID))
		}
		errs = append(errs, p.deleteIDs(ctx, zone, idx, k, ids, describe(pair.new))...)
	}
	return errs
}

func (p *Provider) patchOne(ctx context.Context, zone string, idx *recordIndex, k endpointKey, r contabo.Record, target string, ttl int64) error {
	log := p.log.With("zone", zone, "name", k.name, "type", k.recordType, "id", int64(r.ID), "target", target, "ttl", ttl)
	req, err := targetRequest(k.name, k.recordType, target, ttl)
	if err != nil {
		p.metrics.ObserveChange("update", err)
		log.Error("cannot update record", "error", err)
		return err
	}
	if p.dryRun {
		log.Info("[dry run] would update record")
		return nil
	}
	rec, err := p.api.UpdateRecord(ctx, zone, int64(r.ID), req)
	p.metrics.ObserveChange("update", err)
	if err != nil {
		log.Error("failed to update record", "error", err)
		return fmt.Errorf("update %s %s (id %d) in zone %s: %w", k.name, k.recordType, int64(r.ID), zone, err)
	}
	idx.remove(k, r.ID)
	if rec.ID == 0 {
		rec = recordFromRequest(req)
		rec.ID = r.ID
	}
	idx.add(k, rec)
	log.Info("updated record")
	return nil
}

// apexOwnershipHint explains the most common reason a TXT name has no zone:
// the TXT registry turned the apex "example.com" into "a-example.com".
func apexOwnershipHint(ep *endpoint.Endpoint, zones []string) string {
	if ep.RecordType != contabo.TypeTXT {
		return ""
	}
	name := normalizeName(ep.DNSName)
	for _, z := range zones {
		if strings.HasSuffix(name, "-"+z) {
			return "this looks like the ownership record of the zone apex " + z +
				". Start external-dns with --txt-prefix=extdns-%{record_type}. (ending in a dot) so it lands inside the zone"
		}
	}
	return ""
}

// isOwnershipRecord reports whether ep is a TXT record that the external-dns
// TXT registry uses to remember who owns a record.
func isOwnershipRecord(ep *endpoint.Endpoint) bool {
	if ep.RecordType != contabo.TypeTXT {
		return false
	}
	for _, t := range ep.Targets {
		if strings.Contains(t, "heritage=external-dns") {
			return true
		}
	}
	return false
}

// ownedRecord returns the record in records that the ownership record txt
// belongs to, or nil.
func ownedRecord(txt *endpoint.Endpoint, records []*endpoint.Endpoint) *endpoint.Endpoint {
	for _, r := range records {
		if ownershipMatches(txt, r) {
			return r
		}
	}
	return nil
}

// ownershipOf returns the ownership record in owners that belongs to rec, or nil.
func ownershipOf(rec *endpoint.Endpoint, owners []*endpoint.Endpoint) *endpoint.Endpoint {
	for _, o := range owners {
		if ownershipMatches(o, rec) {
			return o
		}
	}
	return nil
}

// ownershipMatches guesses whether the ownership TXT record txt belongs to
// rec. The TXT registry names ownership records like
// extdns-a.www.example.com (a prefix that ends in a dot) or
// a-www.example.com (the default), so the TXT name must end in the record
// name. When both carry the Kubernetes resource that created them, that must
// match too. Guessing a little too often is safe: it only keeps a spare TXT
// record, or delays a create by one loop.
func ownershipMatches(txt, rec *endpoint.Endpoint) bool {
	tn := normalizeName(txt.DNSName)
	rn := strings.TrimPrefix(normalizeName(rec.DNSName), "*.")
	if !strings.HasSuffix(tn, "."+rn) && !strings.HasSuffix(tn, "-"+rn) {
		return false
	}
	tr, rr := ownershipResource(txt), rec.Labels[endpoint.ResourceLabelKey]
	if tr != "" && rr != "" {
		return tr == rr
	}
	return true
}

// ownershipResource reads the external-dns/resource label from the value of
// an ownership TXT record. It returns "" if the value is encrypted or has no
// such label.
func ownershipResource(txt *endpoint.Endpoint) string {
	key := "external-dns/" + endpoint.ResourceLabelKey + "="
	for _, t := range txt.Targets {
		for part := range strings.SplitSeq(unquote(strings.TrimSpace(t)), ",") {
			if v, ok := strings.CutPrefix(part, key); ok {
				return v
			}
		}
	}
	return ""
}
