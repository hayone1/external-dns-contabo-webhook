package provider

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/external-dns/endpoint"
	"sigs.k8s.io/external-dns/plan"

	"github.com/hayone1/external-dns-contabo-webhook/internal/contabo"
	"github.com/hayone1/external-dns-contabo-webhook/internal/contabo/contabotest"
	"github.com/hayone1/external-dns-contabo-webhook/internal/metrics"
)

type fixture struct {
	srv *contabotest.Server
	p   *Provider
}

func newFixture(t *testing.T, opts Options, zones ...string) *fixture {
	t.Helper()
	srv := contabotest.New()
	t.Cleanup(srv.Close)
	for _, z := range zones {
		srv.AddZone(z)
	}
	c, err := contabo.New(contabo.Options{
		APIURL:  srv.URL,
		AuthURL: srv.TokenURL(),
		Credentials: contabo.Credentials{
			ClientID: contabotest.ClientID, ClientSecret: contabotest.ClientSecret,
			APIUser: contabotest.APIUser, APIPassword: contabotest.APIPassword,
		},
		RateLimit:   1000,
		MaxRetries:  2,
		BaseBackoff: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if opts.Metrics == nil {
		opts.Metrics = metrics.New("test")
	}
	return &fixture{srv: srv, p: New(c, opts)}
}

func strp(s string) *string    { return &s }
func f64(v float64) *float64   { return &v }
func ctx() context.Context     { return context.Background() }
func ttl(v int64) endpoint.TTL { return endpoint.TTL(v) }

func (f *fixture) seed(zone, name, typ, data string, ttl float64) int64 {
	return f.srv.Seed(zone, contabotest.Record{Name: strp(name), Type: typ, TTL: f64(ttl), Prio: f64(0), Data: data})
}

func (f *fixture) records(t *testing.T) map[string]*endpoint.Endpoint {
	t.Helper()
	eps, err := f.p.Records(ctx())
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*endpoint.Endpoint{}
	for _, ep := range eps {
		out[ep.DNSName+"/"+ep.RecordType] = ep
	}
	return out
}

func TestRecordsGroupsAndNormalizes(t *testing.T) {
	f := newFixture(t, Options{}, "example.com")
	f.seed("example.com", "www", "A", "1.1.1.1", 300)
	f.seed("example.com", "www.example.com", "A", "1.0.0.1", 300)
	f.seed("example.com", "WWW.example.com.", "A", "1.1.1.1", 300) // duplicate value
	f.seed("example.com", "", "TXT", "apex", 300)
	f.seed("example.com", "example.com", "TXT", "apex2", 300)
	f.seed("example.com", "alias", "CNAME", "target.example.net.", 60)
	f.seed("example.com", "*.apps", "A", "8.8.8.8", 120)
	f.srv.Seed("example.com", contabotest.Record{Type: "TXT", TTL: f64(300), Prio: f64(0), Data: "no-name"})

	got := f.records(t)
	if ep := got["www.example.com/A"]; ep == nil || strings.Join(ep.Targets, ",") != "1.0.0.1,1.1.1.1" || ep.RecordTTL != 300 {
		t.Errorf("www A = %v", ep)
	}
	if ep := got["example.com/TXT"]; ep == nil || len(ep.Targets) != 3 {
		t.Errorf("apex TXT = %v", ep)
	}
	if ep := got["alias.example.com/CNAME"]; ep == nil || ep.Targets[0] != "target.example.net" {
		t.Errorf("CNAME = %v", ep)
	}
	if got["*.apps.example.com/A"] == nil {
		t.Error("wildcard missing")
	}
	if got["example.com/NS"] != nil || got["example.com/SOA"] != nil {
		t.Error("NS and SOA must never be returned")
	}
}

func TestRecordsMixedTTLReportsZero(t *testing.T) {
	f := newFixture(t, Options{}, "example.com")
	f.seed("example.com", "www", "A", "1.1.1.1", 300)
	f.seed("example.com", "www", "A", "1.0.0.1", 600)
	if ep := f.records(t)["www.example.com/A"]; ep.RecordTTL != 0 {
		t.Fatalf("mixed TTLs should be reported as 0, got %d", ep.RecordTTL)
	}
}

func TestRecordsDomainFilter(t *testing.T) {
	f := newFixture(t, Options{
		DomainFilter: endpoint.NewDomainFilterWithExclusions([]string{"app.example.com", "example.org"}, []string{"secret.app.example.com"}),
	}, "example.com", "example.org", "example.net")
	f.seed("example.com", "www", "A", "1.1.1.1", 300)
	f.seed("example.com", "x.app", "A", "1.1.1.1", 300)
	f.seed("example.com", "secret.app", "A", "1.1.1.1", 300)
	f.seed("example.org", "www", "A", "1.1.1.1", 300)
	f.seed("example.net", "www", "A", "1.1.1.1", 300)

	got := f.records(t)
	if got["x.app.example.com/A"] == nil || got["www.example.org/A"] == nil {
		t.Errorf("filtered records missing: %v", got)
	}
	for _, k := range []string{"www.example.com/A", "secret.app.example.com/A", "www.example.net/A"} {
		if got[k] != nil {
			t.Errorf("%s should be filtered out", k)
		}
	}
	if f.srv.Calls("listRecords") != 2 {
		t.Errorf("example.net should not be read, listRecords called %d times", f.srv.Calls("listRecords"))
	}
}

func TestRecordsChildZoneWins(t *testing.T) {
	f := newFixture(t, Options{}, "example.com", "sub.example.com")
	f.seed("example.com", "www.sub", "A", "1.1.1.1", 300) // shadowed by the child zone
	f.seed("sub.example.com", "www", "A", "8.8.8.8", 300)
	ep := f.records(t)["www.sub.example.com/A"]
	if ep == nil || len(ep.Targets) != 1 || ep.Targets[0] != "8.8.8.8" {
		t.Fatalf("child zone record should win, got %v", ep)
	}
}

func TestRecordsError(t *testing.T) {
	f := newFixture(t, Options{}, "example.com")
	f.srv.Inject(contabotest.Fault{Path: "/records", Status: 500, Count: 100})
	if _, err := f.p.Records(ctx()); err == nil {
		t.Fatal("expected an error")
	}
}

func TestAdjustEndpoints(t *testing.T) {
	f := newFixture(t, Options{DefaultTTL: 900}, "example.com")
	in := []*endpoint.Endpoint{
		endpoint.NewEndpoint("WWW.Example.com.", "A", "8.8.8.8", "10.0.0.1", "8.8.8.8", "1.1.1.1"),
		endpoint.NewEndpoint("private.example.com", "A", "10.0.0.1", "192.168.0.1"),
		endpoint.NewEndpoint("v6.example.com", "AAAA", "2606:4700:4700:0::1111", "fd00::1"),
		endpoint.NewEndpoint("ns.example.com", "NS", "ns1.example.net"),
		endpoint.NewEndpoint("ptr.example.com", "PTR", "x"),
		endpoint.NewEndpoint("weighted.example.com", "A", "8.8.8.8").WithSetIdentifier("eu"),
		endpoint.NewEndpointWithTTL("ttl.example.com", "A", ttl(60), "8.8.8.8"),
		endpoint.NewEndpoint("c.example.com", "CNAME", "b.example.net", "a.example.net"),
		endpoint.NewEndpoint("mx.example.com", "MX", "10 mail.example.com", "bad"),
		endpoint.NewEndpoint("caa.example.com", "CAA", "0 issue letsencrypt.org"),
		endpoint.NewEndpoint("txt.example.com", "TXT", `"heritage=external-dns"`),
		nil,
	}
	in[0].ProviderSpecific = endpoint.ProviderSpecific{{Name: "x", Value: "y"}}

	out, err := f.p.AdjustEndpoints(in)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]*endpoint.Endpoint{}
	for _, ep := range out {
		got[ep.DNSName+"/"+ep.RecordType] = ep
	}
	check := func(key, targets string, wantTTL int64) {
		t.Helper()
		ep := got[key]
		if ep == nil {
			t.Errorf("%s missing", key)
			return
		}
		if strings.Join(ep.Targets, ",") != targets {
			t.Errorf("%s targets = %v, want %s", key, ep.Targets, targets)
		}
		if int64(ep.RecordTTL) != wantTTL {
			t.Errorf("%s ttl = %d, want %d", key, ep.RecordTTL, wantTTL)
		}
	}
	check("www.example.com/A", "1.1.1.1,8.8.8.8", 900)
	check("v6.example.com/AAAA", "2606:4700:4700::1111", 900)
	check("ttl.example.com/A", "8.8.8.8", 60)
	check("c.example.com/CNAME", "a.example.net", 900)
	check("mx.example.com/MX", "10 mail.example.com", 900)
	check("caa.example.com/CAA", `0 issue "letsencrypt.org"`, 900)
	check("txt.example.com/TXT", `"heritage=external-dns"`, 900)
	for _, k := range []string{"private.example.com/A", "ns.example.com/NS", "ptr.example.com/PTR", "weighted.example.com/A"} {
		if got[k] != nil {
			t.Errorf("%s should be dropped", k)
		}
	}
	if got["www.example.com/A"].ProviderSpecific != nil {
		t.Error("provider specific settings should be cleared")
	}
}

func TestApplyChangesCreateUpdateDelete(t *testing.T) {
	f := newFixture(t, Options{DefaultTTL: 300}, "example.com")

	create := &plan.Changes{Create: []*endpoint.Endpoint{
		endpoint.NewEndpointWithTTL("www.example.com", "A", ttl(300), "1.1.1.1", "1.0.0.1"),
		endpoint.NewEndpointWithTTL("example.com", "TXT", ttl(300), `"hello"`),
		endpoint.NewEndpointWithTTL("alias.example.com", "CNAME", ttl(300), "www.example.com"),
	}}
	if err := f.p.ApplyChanges(ctx(), create); err != nil {
		t.Fatal(err)
	}
	got := f.records(t)
	if ep := got["www.example.com/A"]; ep == nil || len(ep.Targets) != 2 {
		t.Fatalf("A not created: %v", ep)
	}
	if got["example.com/TXT"] == nil || got["alias.example.com/CNAME"] == nil {
		t.Fatalf("TXT or CNAME not created: %v", got)
	}

	// Change one target and the TTL.
	update := &plan.Changes{
		UpdateOld: []*endpoint.Endpoint{endpoint.NewEndpointWithTTL("www.example.com", "A", ttl(300), "1.0.0.1", "1.1.1.1")},
		UpdateNew: []*endpoint.Endpoint{endpoint.NewEndpointWithTTL("www.example.com", "A", ttl(600), "1.1.1.1", "8.8.8.8")},
	}
	before := f.srv.Calls("create")
	if err := f.p.ApplyChanges(ctx(), update); err != nil {
		t.Fatal(err)
	}
	if f.srv.Calls("create") != before {
		t.Error("a changed target should be patched in place, not created")
	}
	ep := f.records(t)["www.example.com/A"]
	if strings.Join(ep.Targets, ",") != "1.1.1.1,8.8.8.8" || ep.RecordTTL != 600 {
		t.Fatalf("update not applied: %v ttl=%d", ep, ep.RecordTTL)
	}

	// Change a CNAME target: must patch, a second CNAME would conflict.
	cname := &plan.Changes{
		UpdateOld: []*endpoint.Endpoint{endpoint.NewEndpointWithTTL("alias.example.com", "CNAME", ttl(300), "www.example.com")},
		UpdateNew: []*endpoint.Endpoint{endpoint.NewEndpointWithTTL("alias.example.com", "CNAME", ttl(300), "other.example.net")},
	}
	if err := f.p.ApplyChanges(ctx(), cname); err != nil {
		t.Fatal(err)
	}
	if ep := f.records(t)["alias.example.com/CNAME"]; ep.Targets[0] != "other.example.net" {
		t.Fatalf("CNAME not updated: %v", ep)
	}

	del := &plan.Changes{Delete: []*endpoint.Endpoint{
		endpoint.NewEndpoint("www.example.com", "A", "1.1.1.1", "8.8.8.8"),
		endpoint.NewEndpoint("example.com", "TXT", `"hello"`),
		endpoint.NewEndpoint("alias.example.com", "CNAME", "other.example.net"),
	}}
	if err := f.p.ApplyChanges(ctx(), del); err != nil {
		t.Fatal(err)
	}
	if got := f.records(t); len(got) != 0 {
		t.Fatalf("records left after delete: %v", got)
	}
	if f.srv.Calls("bulkDelete") != 1 {
		t.Errorf("two targets should use one bulk delete, got %d", f.srv.Calls("bulkDelete"))
	}
	if n := len(f.srv.Records("example.com")); n != 4 {
		t.Errorf("NS and SOA must survive, zone has %d records", n)
	}
}

func TestApplyChangesIsIdempotent(t *testing.T) {
	f := newFixture(t, Options{}, "example.com")
	f.seed("example.com", "www", "A", "1.1.1.1", 3600)

	// Create something that is already there: no API call.
	if err := f.p.ApplyChanges(ctx(), &plan.Changes{Create: []*endpoint.Endpoint{
		endpoint.NewEndpointWithTTL("www.example.com", "A", ttl(3600), "1.1.1.1"),
	}}); err != nil {
		t.Fatal(err)
	}
	if f.srv.Calls("create") != 0 {
		t.Error("existing record should not be created again")
	}
	// Delete something that is already gone: no error, no API call.
	if err := f.p.ApplyChanges(ctx(), &plan.Changes{Delete: []*endpoint.Endpoint{
		endpoint.NewEndpoint("gone.example.com", "A", "1.1.1.1"),
	}}); err != nil {
		t.Fatal(err)
	}
	if f.srv.MutatingCalls() != 0 {
		t.Errorf("no mutating calls expected, got %d", f.srv.MutatingCalls())
	}
}

func TestApplyChangesConflictIsSuccess(t *testing.T) {
	f := newFixture(t, Options{}, "example.com")
	f.srv.Inject(contabotest.Fault{Method: "POST", Path: "/records", Status: 409, Count: 1})
	err := f.p.ApplyChanges(ctx(), &plan.Changes{Create: []*endpoint.Endpoint{
		endpoint.NewEndpointWithTTL("www.example.com", "A", ttl(300), "1.1.1.1"),
	}})
	if err != nil {
		t.Fatalf("409 on create should count as success: %v", err)
	}
}

func TestApplyChangesTypeSwap(t *testing.T) {
	f := newFixture(t, Options{}, "example.com")
	f.seed("example.com", "www", "A", "1.1.1.1", 300)
	// external-dns sends a delete of the A and a create of the CNAME. The
	// delete must run first or Contabo rejects the CNAME.
	err := f.p.ApplyChanges(ctx(), &plan.Changes{
		Create: []*endpoint.Endpoint{endpoint.NewEndpointWithTTL("www.example.com", "CNAME", ttl(300), "lb.example.net")},
		Delete: []*endpoint.Endpoint{endpoint.NewEndpoint("www.example.com", "A", "1.1.1.1")},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := f.records(t)
	if got["www.example.com/A"] != nil || got["www.example.com/CNAME"] == nil {
		t.Fatalf("type swap failed: %v", got)
	}
}

func TestApplyChangesPartialFailure(t *testing.T) {
	f := newFixture(t, Options{}, "example.com")
	err := f.p.ApplyChanges(ctx(), &plan.Changes{Create: []*endpoint.Endpoint{
		endpoint.NewEndpointWithTTL("bad.example.com", "A", ttl(300), "10.1.1.1"),
		endpoint.NewEndpointWithTTL("good.example.com", "A", ttl(300), "1.1.1.1"),
	}})
	if err == nil || !strings.Contains(err.Error(), "bad.example.com") {
		t.Fatalf("expected an error naming the bad record, got %v", err)
	}
	if f.records(t)["good.example.com/A"] == nil {
		t.Fatal("the good record should still be created")
	}
}

func TestApplyChangesUnknownZoneIsSkipped(t *testing.T) {
	f := newFixture(t, Options{}, "example.com")
	err := f.p.ApplyChanges(ctx(), &plan.Changes{Create: []*endpoint.Endpoint{
		endpoint.NewEndpointWithTTL("www.other.org", "A", ttl(300), "1.1.1.1"),
	}})
	if err != nil {
		t.Fatalf("names outside every zone should be skipped: %v", err)
	}
	if f.srv.MutatingCalls() != 0 {
		t.Error("nothing should be sent")
	}
}

func TestApplyChangesDryRun(t *testing.T) {
	f := newFixture(t, Options{DryRun: true}, "example.com")
	f.seed("example.com", "old", "A", "1.1.1.1", 300)
	err := f.p.ApplyChanges(ctx(), &plan.Changes{
		Create:    []*endpoint.Endpoint{endpoint.NewEndpointWithTTL("new.example.com", "A", ttl(300), "1.1.1.1")},
		UpdateOld: []*endpoint.Endpoint{endpoint.NewEndpointWithTTL("old.example.com", "A", ttl(300), "1.1.1.1")},
		UpdateNew: []*endpoint.Endpoint{endpoint.NewEndpointWithTTL("old.example.com", "A", ttl(600), "1.1.1.1")},
		Delete:    []*endpoint.Endpoint{endpoint.NewEndpoint("old.example.com", "A", "1.1.1.1")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if f.srv.MutatingCalls() != 0 {
		t.Fatalf("dry run sent %d mutating calls", f.srv.MutatingCalls())
	}
}

func TestApplyChangesUsesFreshIDs(t *testing.T) {
	f := newFixture(t, Options{}, "example.com")
	f.seed("example.com", "www", "A", "1.1.1.1", 300)
	_ = f.records(t)
	// Someone recreates the record by hand, so its ID changes.
	for _, r := range f.srv.Records("example.com") {
		if r.Type == "A" {
			if err := f.p.api.DeleteRecord(ctx(), "example.com", r.RecordID); err != nil {
				t.Fatal(err)
			}
		}
	}
	f.seed("example.com", "www.example.com", "A", "1.1.1.1", 300)

	err := f.p.ApplyChanges(ctx(), &plan.Changes{Delete: []*endpoint.Endpoint{endpoint.NewEndpoint("www.example.com", "A", "1.1.1.1")}})
	if err != nil {
		t.Fatal(err)
	}
	if f.records(t)["www.example.com/A"] != nil {
		t.Fatal("record with the new ID should be deleted")
	}
}

func TestApplyChangesFixesMixedTTL(t *testing.T) {
	f := newFixture(t, Options{}, "example.com")
	f.seed("example.com", "www", "A", "1.1.1.1", 300)
	f.seed("example.com", "www", "A", "1.0.0.1", 600)
	err := f.p.ApplyChanges(ctx(), &plan.Changes{
		UpdateOld: []*endpoint.Endpoint{endpoint.NewEndpoint("www.example.com", "A", "1.0.0.1", "1.1.1.1")},
		UpdateNew: []*endpoint.Endpoint{endpoint.NewEndpointWithTTL("www.example.com", "A", ttl(300), "1.0.0.1", "1.1.1.1")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if ep := f.records(t)["www.example.com/A"]; ep.RecordTTL != 300 {
		t.Fatalf("TTL should be 300 everywhere, got %d", ep.RecordTTL)
	}
	if f.srv.Calls("update") != 1 {
		t.Fatalf("only the 600 record needs a patch, got %d updates", f.srv.Calls("update"))
	}
}

func TestApplyChangesLeavesUnknownTargets(t *testing.T) {
	f := newFixture(t, Options{}, "example.com")
	f.seed("example.com", "www", "A", "1.1.1.1", 300)
	f.seed("example.com", "www", "A", "9.9.9.9", 300) // added by hand after the last read
	err := f.p.ApplyChanges(ctx(), &plan.Changes{
		UpdateOld: []*endpoint.Endpoint{endpoint.NewEndpointWithTTL("www.example.com", "A", ttl(300), "1.1.1.1")},
		UpdateNew: []*endpoint.Endpoint{endpoint.NewEndpointWithTTL("www.example.com", "A", ttl(300), "8.8.8.8")},
	})
	if err != nil {
		t.Fatal(err)
	}
	ep := f.records(t)["www.example.com/A"]
	if strings.Join(ep.Targets, ",") != "8.8.8.8,9.9.9.9" {
		t.Fatalf("got %v, want the unknown 9.9.9.9 kept", ep.Targets)
	}
}

func TestApplyChangesNil(t *testing.T) {
	f := newFixture(t, Options{}, "example.com")
	if err := f.p.ApplyChanges(ctx(), nil); err != nil {
		t.Fatal(err)
	}
	if err := f.p.ApplyChanges(ctx(), &plan.Changes{}); err != nil {
		t.Fatal(err)
	}
	if f.srv.Calls("listZones") != 0 {
		t.Fatal("empty changes should not call the API")
	}
}

func TestGetDomainFilter(t *testing.T) {
	f := newFixture(t, Options{DomainFilter: endpoint.NewDomainFilter([]string{"example.com"})})
	df := f.p.GetDomainFilter()
	if !df.Match("www.example.com") || df.Match("www.example.org") {
		t.Fatal("domain filter not passed through")
	}
}

func ownershipTXT(name, resource string) *endpoint.Endpoint {
	return endpoint.NewEndpointWithTTL(name, "TXT", ttl(300),
		`"heritage=external-dns,external-dns/owner=test,external-dns/resource=`+resource+`"`)
}

func withResource(ep *endpoint.Endpoint, resource string) *endpoint.Endpoint {
	ep.Labels[endpoint.ResourceLabelKey] = resource
	return ep
}

// Seen on the real API: DELETE returned HTTP 500 for a while. If the record
// delete fails but its ownership record is deleted, the record is left
// behind and external-dns can never clean it up.
func TestApplyChangesKeepsOwnershipWhenDeleteFails(t *testing.T) {
	f := newFixture(t, Options{}, "example.com")
	webID := f.seed("example.com", "www", "A", "1.1.1.1", 300)
	f.seed("example.com", "extdns-a.www", "TXT", `"heritage=external-dns,external-dns/owner=test,external-dns/resource=service/ns/web"`, 300)
	f.seed("example.com", "api", "A", "1.1.1.1", 300)
	f.seed("example.com", "extdns-a.api", "TXT", `"heritage=external-dns,external-dns/owner=test,external-dns/resource=service/ns/api"`, 300)
	f.srv.Inject(contabotest.Fault{Method: "DELETE", Path: fmt.Sprintf("/records/%d", webID), Status: 500, Count: 100})

	err := f.p.ApplyChanges(ctx(), &plan.Changes{Delete: []*endpoint.Endpoint{
		// The registry sends the ownership record first; order must not matter.
		ownershipTXT("extdns-a.www.example.com", "service/ns/web"),
		withResource(endpoint.NewEndpoint("www.example.com", "A", "1.1.1.1"), "service/ns/web"),
		ownershipTXT("extdns-a.api.example.com", "service/ns/api"),
		withResource(endpoint.NewEndpoint("api.example.com", "A", "1.1.1.1"), "service/ns/api"),
	}})
	if err == nil {
		t.Fatal("expected an error for the failed delete")
	}
	got := f.records(t)
	if got["www.example.com/A"] == nil || got["extdns-a.www.example.com/TXT"] == nil {
		t.Fatalf("www and its ownership record must both stay: %v", got)
	}
	if got["api.example.com/A"] != nil || got["extdns-a.api.example.com/TXT"] != nil {
		t.Fatalf("api and its ownership record should be gone: %v", got)
	}

	// The outage ends; the next loop cleans up the rest.
	f.srv.ClearFaults()
	err = f.p.ApplyChanges(ctx(), &plan.Changes{Delete: []*endpoint.Endpoint{
		ownershipTXT("extdns-a.www.example.com", "service/ns/web"),
		withResource(endpoint.NewEndpoint("www.example.com", "A", "1.1.1.1"), "service/ns/web"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got := f.records(t); len(got) != 0 {
		t.Fatalf("records left: %v", got)
	}
}

func TestApplyChangesSkipsRecordWhenOwnershipCreateFails(t *testing.T) {
	f := newFixture(t, Options{}, "example.com")
	f.srv.Inject(contabotest.Fault{Method: "POST", BodyContains: "service/ns/web", Status: 500, Count: 100})

	err := f.p.ApplyChanges(ctx(), &plan.Changes{Create: []*endpoint.Endpoint{
		// Records first in the list: the provider must still create the
		// ownership record first.
		withResource(endpoint.NewEndpointWithTTL("www.example.com", "A", ttl(300), "1.1.1.1"), "service/ns/web"),
		withResource(endpoint.NewEndpointWithTTL("api.example.com", "A", ttl(300), "1.1.1.1"), "service/ns/api"),
		ownershipTXT("extdns-a.www.example.com", "service/ns/web"),
		ownershipTXT("extdns-a.api.example.com", "service/ns/api"),
	}})
	if err == nil || !strings.Contains(err.Error(), "ownership record") {
		t.Fatalf("expected an ownership error, got %v", err)
	}
	got := f.records(t)
	if got["www.example.com/A"] != nil {
		t.Fatal("www must not be created without its ownership record")
	}
	if got["api.example.com/A"] == nil || got["extdns-a.api.example.com/TXT"] == nil {
		t.Fatalf("api should be created normally: %v", got)
	}
}

func TestPlainTXTIsNotOwnership(t *testing.T) {
	if isOwnershipRecord(endpoint.NewEndpoint("x.example.com", "TXT", `"v=spf1 -all"`)) {
		t.Fatal("a normal TXT record is not an ownership record")
	}
	if !isOwnershipRecord(ownershipTXT("a-x.example.com", "service/ns/x")) {
		t.Fatal("ownership record not detected")
	}
}

func TestOwnershipMatches(t *testing.T) {
	rec := func(name, res string) *endpoint.Endpoint {
		ep := endpoint.NewEndpoint(name, "A", "1.1.1.1")
		if res != "" {
			ep.Labels[endpoint.ResourceLabelKey] = res
		}
		return ep
	}
	plain := func(name string) *endpoint.Endpoint {
		return endpoint.NewEndpoint(name, "TXT", `"heritage=external-dns,external-dns/owner=test"`)
	}
	tests := []struct {
		txt  *endpoint.Endpoint
		rec  *endpoint.Endpoint
		want bool
	}{
		{ownershipTXT("extdns-a.www.example.com", "service/ns/web"), rec("www.example.com", "service/ns/web"), true},
		{ownershipTXT("extdns-a.www.example.com", "service/ns/web"), rec("www.example.com", "service/ns/other"), false},
		{ownershipTXT("extdns-a.www.example.com", "service/ns/web"), rec("api.example.com", "service/ns/web"), false},
		{ownershipTXT("a-www.example.com", "service/ns/web"), rec("www.example.com", "service/ns/web"), true},
		{ownershipTXT("extdns-a.wildcard.apps.example.com", "service/ns/w"), rec("*.apps.example.com", "service/ns/w"), true},
		{ownershipTXT("extdns-a.example.com", "service/ns/apex"), rec("example.com", "service/ns/apex"), true},
		{plain("extdns-a.www.example.com"), rec("www.example.com", ""), true},
		{plain("extdns-a.www.example.com"), rec("www.example.com", "service/ns/web"), true},
		{plain("extdns-a.notwww.example.com"), rec("www.example.com", ""), false},
	}
	for i, tt := range tests {
		if got := ownershipMatches(tt.txt, tt.rec); got != tt.want {
			t.Errorf("case %d: ownershipMatches(%s, %s) = %v, want %v", i, tt.txt.DNSName, tt.rec.DNSName, got, tt.want)
		}
	}
}
