// Package integration runs real external-dns code against the webhook.
//
// The chain is the same as in a cluster, minus Kubernetes:
//
//	external-dns planner -> TXT registry -> webhook HTTP client
//	  -> our webhook server -> our provider -> our Contabo client
//	  -> fake Contabo API
//
// Each scenario runs the reconcile loop twice. The second run must plan no
// changes; otherwise external-dns would fight itself forever in production.
package integration

import (
	"context"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/external-dns/endpoint"
	"sigs.k8s.io/external-dns/pkg/apis/externaldns"
	"sigs.k8s.io/external-dns/plan"
	"sigs.k8s.io/external-dns/provider/webhook"
	"sigs.k8s.io/external-dns/registry"
	"sigs.k8s.io/external-dns/registry/txt"

	"github.com/hayone1/external-dns-contabo-webhook/internal/contabo"
	"github.com/hayone1/external-dns-contabo-webhook/internal/contabo/contabotest"
	"github.com/hayone1/external-dns-contabo-webhook/internal/provider"
	"github.com/hayone1/external-dns-contabo-webhook/internal/server"
)

const zone = "example.com"

type harness struct {
	t    *testing.T
	api  *contabotest.Server
	reg  registry.Registry
	pol  plan.Policy
	mgd  []string
	errs int
}

func newHarness(t *testing.T, policy plan.Policy, filter []string) *harness {
	t.Helper()
	api := contabotest.New()
	t.Cleanup(api.Close)
	api.AddZone(zone)
	api.AddZone("example.org")

	client, err := contabo.New(contabo.Options{
		APIURL:  api.URL,
		AuthURL: api.TokenURL(),
		Credentials: contabo.Credentials{
			ClientID: contabotest.ClientID, ClientSecret: contabotest.ClientSecret,
			APIUser: contabotest.APIUser, APIPassword: contabotest.APIPassword,
		},
		RateLimit:   1000,
		BaseBackoff: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	p := provider.New(client, provider.Options{DomainFilter: endpoint.NewDomainFilter(filter), DefaultTTL: 300})
	hook := httptest.NewServer(server.WebhookHandler(p, nil))
	t.Cleanup(hook.Close)

	// externaldns.NewConfig returns empty values, so set what the
	// controller would get from its flag defaults.
	cfg := externaldns.NewConfig()
	cfg.WebhookProviderURL = hook.URL
	cfg.WebhookProviderReadTimeout = 5 * time.Second
	cfg.WebhookProviderWriteTimeout = 10 * time.Second
	cfg.TXTOwnerID = "test-cluster"
	// The settings the README recommends. Without a prefix that ends in a
	// dot, the ownership record of an apex name lands outside the zone.
	cfg.TXTPrefix = "extdns-%{record_type}."
	cfg.TXTWildcardReplacement = "wildcard"
	cfg.ManagedDNSRecordTypes = []string{endpoint.RecordTypeA, endpoint.RecordTypeAAAA, endpoint.RecordTypeCNAME}
	wp, err := webhook.New(context.Background(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := txt.New(cfg, wp)
	if err != nil {
		t.Fatal(err)
	}
	return &harness{t: t, api: api, reg: reg, pol: policy, mgd: cfg.ManagedDNSRecordTypes}
}

// run does one reconcile loop and returns the number of planned changes.
func (h *harness) run(desired func() []*endpoint.Endpoint) int {
	h.t.Helper()
	ctx := context.Background()
	current, err := h.reg.Records(ctx)
	if err != nil {
		h.t.Fatalf("Records: %v", err)
	}
	adjusted, err := h.reg.AdjustEndpoints(desired())
	if err != nil {
		h.t.Fatalf("AdjustEndpoints: %v", err)
	}
	p := (&plan.Plan{
		Policies:       []plan.Policy{h.pol},
		Current:        current,
		Desired:        adjusted,
		DomainFilter:   endpoint.MatchAllDomainFilters{h.reg.GetDomainFilter()},
		ManagedRecords: h.mgd,
		OwnerID:        h.reg.OwnerID(),
	}).Calculate()
	c := p.Changes
	n := len(c.Create) + len(c.UpdateNew) + len(c.Delete)
	if n > 0 {
		if err := h.reg.ApplyChanges(ctx, c); err != nil {
			h.errs++
			h.t.Logf("ApplyChanges: %v", err)
		}
	}
	return n
}

// converge runs the loop until it plans nothing, failing if that takes more
// than two runs.
func (h *harness) converge(desired func() []*endpoint.Endpoint) {
	h.t.Helper()
	if n := h.run(desired); n == 0 {
		h.t.Fatal("first run should have planned changes")
	}
	if n := h.run(desired); n != 0 {
		h.t.Fatalf("second run planned %d changes; the webhook is not stable", n)
	}
	if h.errs != 0 {
		h.t.Fatalf("%d ApplyChanges calls failed", h.errs)
	}
}

// dns returns "name type target" lines for every non NS/SOA record.
func (h *harness) dns(z string) []string {
	var out []string
	for _, r := range h.api.Records(z) {
		if r.Type == "NS" || r.Type == "SOA" {
			continue
		}
		name := z
		if r.Name != nil && *r.Name != "" {
			name = strings.TrimSuffix(strings.ToLower(*r.Name), ".")
			if name != z && !strings.HasSuffix(name, "."+z) {
				name += "." + z
			}
		}
		out = append(out, name+" "+r.Type+" "+r.Data)
	}
	sort.Strings(out)
	return out
}

func (h *harness) has(z, line string) bool {
	for _, l := range h.dns(z) {
		if l == line {
			return true
		}
	}
	return false
}

func eps(list ...*endpoint.Endpoint) func() []*endpoint.Endpoint {
	return func() []*endpoint.Endpoint {
		out := make([]*endpoint.Endpoint, len(list))
		for i, ep := range list {
			out[i] = ep.DeepCopy()
		}
		return out
	}
}

func TestLifecycle(t *testing.T) {
	h := newHarness(t, &plan.SyncPolicy{}, nil)

	// 1. Create a mix of records.
	v1 := eps(
		endpoint.NewEndpoint("www.example.com", "A", "1.1.1.1", "8.8.8.8"),
		endpoint.NewEndpoint("example.com", "A", "1.0.0.1"),
		endpoint.NewEndpoint("v6.example.com", "AAAA", "2606:4700:4700::1111"),
		endpoint.NewEndpoint("alias.example.com", "CNAME", "www.example.com"),
		endpoint.NewEndpoint("*.apps.example.com", "A", "1.1.1.1"),
		endpoint.NewEndpointWithTTL("short.example.com", "A", 60, "8.8.4.4"),
		endpoint.NewEndpoint("www.example.org", "A", "9.9.9.9"),
	)
	h.converge(v1)
	for _, want := range []string{
		"www.example.com A 1.1.1.1", "www.example.com A 8.8.8.8", "example.com A 1.0.0.1",
		"v6.example.com AAAA 2606:4700:4700::1111", "alias.example.com CNAME www.example.com",
		"*.apps.example.com A 1.1.1.1", "short.example.com A 8.8.4.4",
	} {
		if !h.has(zone, want) {
			t.Errorf("missing %q in %v", want, h.dns(zone))
		}
	}
	if !h.has("example.org", "www.example.org A 9.9.9.9") {
		t.Errorf("second zone not written: %v", h.dns("example.org"))
	}
	txtCount := 0
	for _, l := range h.dns(zone) {
		if strings.Contains(l, " TXT ") && strings.Contains(l, "external-dns/owner=test-cluster") {
			txtCount++
		}
	}
	if txtCount == 0 {
		t.Fatalf("no ownership TXT records written: %v", h.dns(zone))
	}

	// 2. Change targets and TTLs, swap A for CNAME.
	v2 := eps(
		endpoint.NewEndpoint("www.example.com", "A", "8.8.8.8", "9.9.9.9"),
		endpoint.NewEndpointWithTTL("example.com", "A", 600, "1.0.0.1"),
		endpoint.NewEndpoint("v6.example.com", "AAAA", "2606:4700:4700::1001"),
		endpoint.NewEndpoint("alias.example.com", "CNAME", "other.example.net"),
		endpoint.NewEndpoint("*.apps.example.com", "CNAME", "lb.example.net"),
		endpoint.NewEndpointWithTTL("short.example.com", "A", 60, "8.8.4.4"),
		endpoint.NewEndpoint("www.example.org", "A", "9.9.9.9"),
	)
	h.converge(v2)
	if h.has(zone, "www.example.com A 1.1.1.1") || !h.has(zone, "www.example.com A 9.9.9.9") {
		t.Errorf("www not updated: %v", h.dns(zone))
	}
	if h.has(zone, "*.apps.example.com A 1.1.1.1") || !h.has(zone, "*.apps.example.com CNAME lb.example.net") {
		t.Errorf("A to CNAME swap failed: %v", h.dns(zone))
	}

	// 3. Remove everything. Only NS and SOA may stay.
	h.converge(eps())
	if left := h.dns(zone); len(left) != 0 {
		t.Fatalf("records left behind: %v", left)
	}
	if left := h.dns("example.org"); len(left) != 0 {
		t.Fatalf("records left behind: %v", left)
	}
	if n := len(h.api.Records(zone)); n != 4 {
		t.Fatalf("NS/SOA records were touched, zone has %d records", n)
	}
}

func TestForeignRecordsAreLeftAlone(t *testing.T) {
	h := newHarness(t, &plan.SyncPolicy{}, nil)
	ttl, prio := 300.0, 0.0
	name := "manual"
	h.api.Seed(zone, contabotest.Record{Name: &name, Type: "A", TTL: &ttl, Prio: &prio, Data: "4.4.4.4"})
	mx := "example.com"
	ten := 10.0
	h.api.Seed(zone, contabotest.Record{Name: &mx, Type: "MX", TTL: &ttl, Prio: &ten, Data: "mail.example.com"})

	h.converge(eps(endpoint.NewEndpoint("www.example.com", "A", "1.1.1.1")))
	h.converge(eps())
	if !h.has(zone, "manual.example.com A 4.4.4.4") || !h.has(zone, "example.com MX mail.example.com") {
		t.Fatalf("records not owned by external-dns were changed: %v", h.dns(zone))
	}
}

func TestUnusableTargetsDoNotLoop(t *testing.T) {
	h := newHarness(t, &plan.SyncPolicy{}, nil)
	desired := eps(
		endpoint.NewEndpoint("private.example.com", "A", "10.0.0.5"),
		endpoint.NewEndpoint("mixed.example.com", "A", "10.0.0.5", "1.1.1.1"),
		endpoint.NewEndpoint("ns.example.com", "NS", "ns1.example.net"),
	)
	h.converge(desired)
	if h.has(zone, "private.example.com A 10.0.0.5") {
		t.Fatal("private IP must never be sent")
	}
	if !h.has(zone, "mixed.example.com A 1.1.1.1") {
		t.Fatalf("public target of a mixed endpoint should be created: %v", h.dns(zone))
	}
	if n := h.run(desired); n != 0 {
		t.Fatalf("third run planned %d changes", n)
	}
}

func TestDomainFilter(t *testing.T) {
	h := newHarness(t, &plan.SyncPolicy{}, []string{"example.org"})
	h.converge(eps(
		endpoint.NewEndpoint("www.example.org", "A", "1.1.1.1"),
		endpoint.NewEndpoint("www.example.com", "A", "1.1.1.1"),
	))
	if len(h.dns(zone)) != 0 {
		t.Fatalf("example.com is outside the filter but was written: %v", h.dns(zone))
	}
	if !h.has("example.org", "www.example.org A 1.1.1.1") {
		t.Fatal("example.org record missing")
	}
}

func TestUpsertOnlyNeverDeletes(t *testing.T) {
	h := newHarness(t, &plan.UpsertOnlyPolicy{}, nil)
	h.converge(eps(endpoint.NewEndpoint("www.example.com", "A", "1.1.1.1")))
	if n := h.run(eps()); n != 0 {
		t.Fatalf("upsert-only planned %d changes for a removal", n)
	}
	if !h.has(zone, "www.example.com A 1.1.1.1") {
		t.Fatal("upsert-only deleted a record")
	}
}

func TestTransientErrorsRecover(t *testing.T) {
	h := newHarness(t, &plan.SyncPolicy{}, nil)
	desired := eps(endpoint.NewEndpoint("www.example.com", "A", "1.1.1.1"))
	// Every create fails with a 503 for a while. The webhook returns 500 and
	// external-dns tries again on the next loop.
	h.api.Inject(contabotest.Fault{Method: "POST", Path: "/records", Status: 503, Count: 3})
	h.run(desired)
	if h.errs == 0 {
		t.Fatal("expected the first loop to fail")
	}
	h.errs = 0
	for range 3 {
		if h.run(desired) == 0 {
			break
		}
	}
	if !h.has(zone, "www.example.com A 1.1.1.1") {
		t.Fatalf("record not created after the outage: %v", h.dns(zone))
	}
	if n := h.run(desired); n != 0 {
		t.Fatalf("not stable after recovery: %d changes", n)
	}
}
