// Package provider implements the external-dns provider.Provider interface on
// top of the Contabo DNS API.
package provider

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"slices"
	"sort"
	"strings"
	"sync"

	"golang.org/x/sync/errgroup"
	"sigs.k8s.io/external-dns/endpoint"
	"sigs.k8s.io/external-dns/provider"

	"github.com/hayone1/external-dns-contabo-webhook/internal/contabo"
	"github.com/hayone1/external-dns-contabo-webhook/internal/metrics"
)

// zoneReadConcurrency limits how many zones are read at the same time. The
// client rate limiter still applies on top of this.
const zoneReadConcurrency = 4

// API is the part of the Contabo client the provider uses. Tests replace it
// with an in-memory fake.
type API interface {
	ListZones(ctx context.Context) ([]contabo.Zone, error)
	ListRecords(ctx context.Context, zone string) ([]contabo.Record, error)
	CreateRecord(ctx context.Context, zone string, req contabo.RecordRequest) (contabo.Record, error)
	UpdateRecord(ctx context.Context, zone string, id int64, req contabo.RecordRequest) (contabo.Record, error)
	DeleteRecord(ctx context.Context, zone string, id int64) error
	BulkDeleteRecords(ctx context.Context, zone string, ids []int64) ([]int64, error)
}

// Options configures a Provider.
type Options struct {
	DomainFilter *endpoint.DomainFilter
	DefaultTTL   int64
	DryRun       bool
	Logger       *slog.Logger
	Metrics      *metrics.Metrics
}

// Provider is the Contabo implementation of provider.Provider.
type Provider struct {
	provider.BaseProvider

	api          API
	domainFilter *endpoint.DomainFilter
	defaultTTL   int64
	dryRun       bool
	log          *slog.Logger
	metrics      *metrics.Metrics

	// applyMu makes sure two ApplyChanges calls never interleave. external-dns
	// does not send them in parallel, but a retry after a timeout could.
	applyMu sync.Mutex
}

var _ provider.Provider = (*Provider)(nil)

// New creates a Provider.
func New(api API, o Options) *Provider {
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.DomainFilter == nil {
		o.DomainFilter = endpoint.NewDomainFilter(nil)
	}
	if o.DefaultTTL <= 0 {
		o.DefaultTTL = 3600
	}
	return &Provider{
		api:          api,
		domainFilter: o.DomainFilter,
		defaultTTL:   o.DefaultTTL,
		dryRun:       o.DryRun,
		log:          o.Logger,
		metrics:      o.Metrics,
	}
}

// GetDomainFilter returns the filter external-dns should apply.
func (p *Provider) GetDomainFilter() endpoint.DomainFilterInterface {
	return p.domainFilter
}

// zoneState is one zone with its current records.
type zoneState struct {
	name    string
	records []contabo.Record
}

// managedZones lists the zones whose names or sub-names match the domain
// filter. It returns the normalized names of all zones too, so that records
// can be matched to the most specific zone.
func (p *Provider) managedZones(ctx context.Context) (managed, all []string, err error) {
	zones, err := p.api.ListZones(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("list zones: %w", err)
	}
	seen := map[string]bool{}
	for _, z := range zones {
		name := normalizeName(z.Name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		all = append(all, name)
		// A zone is managed if the filter matches it, or if the filter
		// names a sub-domain of it (DOMAIN_FILTER=app.example.com still
		// needs the example.com zone).
		if p.domainFilter.Match(name) || p.domainFilter.MatchParent(name) {
			managed = append(managed, name)
		}
	}
	sort.Strings(managed)
	sort.Strings(all)
	return managed, all, nil
}

// readZones fetches the records of several zones at once.
func (p *Provider) readZones(ctx context.Context, names []string) ([]zoneState, error) {
	out := make([]zoneState, len(names))
	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(zoneReadConcurrency)
	for i, name := range names {
		g.Go(func() error {
			recs, err := p.api.ListRecords(ctx, name)
			if err != nil {
				return fmt.Errorf("list records of zone %s: %w", name, err)
			}
			out[i] = zoneState{name: name, records: recs}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return out, nil
}

// Records returns every record set in the managed zones.
func (p *Provider) Records(ctx context.Context) ([]*endpoint.Endpoint, error) {
	managed, all, err := p.managedZones(ctx)
	if err != nil {
		return nil, err
	}
	states, err := p.readZones(ctx, managed)
	if err != nil {
		return nil, err
	}

	type group struct {
		targets []string
		ttls    map[int64]bool
	}
	groups := map[endpointKey]*group{}
	for _, zs := range states {
		for _, r := range zs.records {
			if !supportedType(r.Type) {
				continue // NS, SOA and anything else Contabo manages itself
			}
			fqdn := recordFQDN(r.Name, zs.name)
			// Skip records that sit in a parent zone but belong to a more
			// specific child zone; DNS never serves them.
			if owner, _ := zoneFor(fqdn, all); owner != zs.name {
				continue
			}
			if !p.domainFilter.Match(fqdn) {
				continue
			}
			target, ok := recordTarget(r)
			if !ok {
				continue
			}
			k := endpointKey{name: fqdn, recordType: r.Type}
			g := groups[k]
			if g == nil {
				g = &group{ttls: map[int64]bool{}}
				groups[k] = g
			}
			if !slices.Contains(g.targets, target) {
				g.targets = append(g.targets, target)
			}
			g.ttls[int64(r.TTL)] = true
		}
	}

	endpoints := make([]*endpoint.Endpoint, 0, len(groups))
	for k, g := range groups {
		// When the records of one set have different TTLs, report TTL 0.
		// external-dns then sees a TTL change and sends an update, and the
		// update brings every record to the same TTL.
		var ttl int64
		if len(g.ttls) == 1 {
			for t := range g.ttls {
				ttl = t
			}
		}
		sort.Strings(g.targets)
		ep := endpoint.NewEndpointWithTTL(k.name, k.recordType, endpoint.TTL(ttl), g.targets...)
		if ep == nil {
			p.log.Warn("skipping record with a label longer than 63 characters", "name", k.name, "type", k.recordType)
			continue
		}
		endpoints = append(endpoints, ep)
	}
	sort.Slice(endpoints, func(i, j int) bool {
		if endpoints[i].DNSName != endpoints[j].DNSName {
			return endpoints[i].DNSName < endpoints[j].DNSName
		}
		return endpoints[i].RecordType < endpoints[j].RecordType
	})

	p.metrics.SetInventory(len(managed), len(endpoints))
	p.metrics.MarkSuccess()
	p.log.Debug("read records from Contabo", "zones", len(managed), "endpoints", len(endpoints))
	return endpoints, nil
}

// AdjustEndpoints rewrites the desired endpoints so they look exactly like
// what Records would return after they are created. That stops external-dns
// from planning the same change on every loop. Endpoints Contabo cannot store
// are dropped with a warning instead of failing the whole batch.
func (p *Provider) AdjustEndpoints(endpoints []*endpoint.Endpoint) ([]*endpoint.Endpoint, error) {
	out := make([]*endpoint.Endpoint, 0, len(endpoints))
	for _, ep := range endpoints {
		if ep == nil {
			continue
		}
		ep.DNSName = normalizeName(ep.DNSName)
		log := p.log.With("name", ep.DNSName, "type", ep.RecordType)

		if !supportedType(ep.RecordType) {
			log.Warn("dropping endpoint: Contabo cannot manage this record type")
			p.metrics.ObserveSkipped("unsupported_type")
			continue
		}
		if ep.SetIdentifier != "" {
			log.Warn("dropping endpoint: Contabo has no routing policies, so set-identifier is not supported", "setIdentifier", ep.SetIdentifier)
			p.metrics.ObserveSkipped("set_identifier")
			continue
		}

		targets := make([]string, 0, len(ep.Targets))
		for _, t := range ep.Targets {
			req, err := targetRequest(ep.DNSName, ep.RecordType, t, p.defaultTTL)
			if err != nil {
				log.Warn("dropping target", "target", t, "error", err)
				p.metrics.ObserveSkipped("invalid_target")
				continue
			}
			if ep.RecordType == contabo.TypeA || ep.RecordType == contabo.TypeAAAA {
				if ip, _ := netip.ParseAddr(req.Data); !publicIP(ip) {
					log.Warn("dropping target: Contabo only accepts public IP addresses", "target", t)
					p.metrics.ObserveSkipped("non_public_ip")
					continue
				}
			}
			canon, _ := recordTarget(recordFromRequest(req))
			if !slices.Contains(targets, canon) {
				targets = append(targets, canon)
			}
		}
		if len(targets) == 0 {
			log.Warn("dropping endpoint: no usable targets left")
			continue
		}
		sort.Strings(targets)
		if ep.RecordType == contabo.TypeCNAME && len(targets) > 1 {
			log.Warn("a CNAME can only have one target, keeping the first", "targets", targets)
			p.metrics.ObserveSkipped("extra_cname_target")
			targets = targets[:1]
		}
		ep.Targets = targets

		if !ep.RecordTTL.IsConfigured() {
			ep.RecordTTL = endpoint.TTL(p.defaultTTL)
		}
		// Contabo has no provider specific settings. Clearing them keeps
		// desired and current endpoints equal.
		ep.ProviderSpecific = nil
		out = append(out, ep)
	}
	return out, nil
}

// recordFromRequest builds the record Contabo would store for req.
func recordFromRequest(req contabo.RecordRequest) contabo.Record {
	r := contabo.Record{Name: req.Name, Type: req.Type, TTL: contabo.Number(req.TTL), Prio: contabo.Number(req.Prio), Data: req.Data, Tag: req.Tag}
	if req.Port != nil {
		r.Port = contabo.Number(*req.Port)
	}
	if req.Weight != nil {
		r.Weight = contabo.Number(*req.Weight)
	}
	if req.Flag != nil {
		r.Flag = contabo.Number(*req.Flag)
	}
	return r
}

// ttlOf returns the TTL to send for an endpoint.
func (p *Provider) ttlOf(ep *endpoint.Endpoint) int64 {
	if ep.RecordTTL.IsConfigured() && ep.RecordTTL > 0 {
		return int64(ep.RecordTTL)
	}
	return p.defaultTTL
}

// describe is used in log lines.
func describe(ep *endpoint.Endpoint) string {
	return fmt.Sprintf("%s %s [%s]", ep.DNSName, ep.RecordType, strings.Join(ep.Targets, ", "))
}
