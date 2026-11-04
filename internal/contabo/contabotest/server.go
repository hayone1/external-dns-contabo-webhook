// Package contabotest is an in-memory fake of the Contabo DNS API for tests.
//
// It copies the quirks seen on the real API (checked against
// api.contabo.com in November 2026):
//
//   - names are stored exactly as sent; "www", "www.zone" and "www.zone."
//     all mean the same record
//   - a new zone already holds NS and SOA records with null numbers
//   - A and AAAA data must be a public IP address
//   - creating an exact duplicate returns 409
//   - a CNAME next to another record of the same name returns 400
//   - PATCH on an unknown record ID returns 500
//   - PATCH rewrites the stored name to the full name
//   - DELETE returns 204 even if the record does not exist
//   - DELETE with a JSON content type and no body returns 400
//   - every request needs an x-request-id header
//   - page size is capped at 1000
package contabotest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Credentials accepted by the fake token endpoint.
const (
	ClientID     = "INT-test"
	ClientSecret = "test-secret"
	APIUser      = "user@example.com"
	APIPassword  = "test-password"
)

// Record is a stored record. Numeric fields are pointers so they can be null,
// like on the real API.
type Record struct {
	TenantID   string   `json:"tenantId"`
	CustomerID string   `json:"customerId"`
	RecordID   int64    `json:"recordId"`
	Name       *string  `json:"name,omitempty"`
	Type       string   `json:"type"`
	TTL        *float64 `json:"ttl"`
	Prio       *float64 `json:"prio"`
	Data       string   `json:"data"`
	Port       *float64 `json:"port,omitempty"`
	Weight     *float64 `json:"weight,omitempty"`
	Flag       *float64 `json:"flag,omitempty"`
	Tag        *string  `json:"tag,omitempty"`
}

// Fault makes the next matching requests fail.
type Fault struct {
	Method       string      `json:"method,omitempty"`       // empty matches any method
	Path         string      `json:"path,omitempty"`         // substring of the URL path; empty matches any path
	BodyContains string      `json:"bodyContains,omitempty"` // substring of the request body; empty matches any body
	Status       int         `json:"status"`
	Count        int         `json:"count"`
	Header       http.Header `json:"header,omitempty"`
}

// Server is the fake API.
type Server struct {
	// Server is only set by New. NewHandler leaves it nil.
	*httptest.Server

	handler  http.Handler
	mu       sync.Mutex
	zones    map[string][]*Record
	order    []string
	nextID   int64
	faults   []*Fault
	calls    map[string]int
	tokens   map[string]bool
	tokenSeq atomic.Int64
	// TokenTTL is sent as expires_in. Defaults to 300 seconds.
	TokenTTL int
}

// New starts a fake server on a random local port. Close it when done.
func New() *Server {
	s := NewHandler()
	s.Server = httptest.NewServer(s.handler)
	return s
}

// NewHandler creates the fake without starting a server. Serve
// Handler() yourself, for example in a container for end-to-end tests.
//
// Besides the Contabo API it serves two test-only endpoints:
//
//	POST   /_fake/faults   add a Fault (JSON body)
//	DELETE /_fake/faults   remove all faults
func NewHandler() *Server {
	s := &Server{
		zones:    map[string][]*Record{},
		nextID:   1000,
		calls:    map[string]int{},
		tokens:   map[string]bool{},
		TokenTTL: 300,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /_fake/faults", s.addFault)
	mux.HandleFunc("DELETE /_fake/faults", func(w http.ResponseWriter, _ *http.Request) {
		s.ClearFaults()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.HandleFunc("POST /token", s.token)
	mux.HandleFunc("GET /v1/dns/zones", s.auth(s.listZones))
	mux.HandleFunc("GET /v1/dns/zones/{zone}", s.auth(s.getZone))
	mux.HandleFunc("GET /v1/dns/zones/{zone}/records", s.auth(s.listRecords))
	mux.HandleFunc("POST /v1/dns/zones/{zone}/records", s.auth(s.createRecord))
	mux.HandleFunc("DELETE /v1/dns/zones/{zone}/records/bulk", s.auth(s.bulkDelete))
	mux.HandleFunc("PATCH /v1/dns/zones/{zone}/records/{id}", s.auth(s.updateRecord))
	mux.HandleFunc("DELETE /v1/dns/zones/{zone}/records/{id}", s.auth(s.deleteRecord))
	s.handler = mux
	return s
}

// Handler returns the HTTP handler of the fake API.
func (s *Server) Handler() http.Handler { return s.handler }

func (s *Server) addFault(w http.ResponseWriter, r *http.Request) {
	var f Fault
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&f); err != nil || f.Status < 100 || f.Status > 599 || f.Count < 1 {
		writeError(w, http.StatusBadRequest, "body must be a fault with status 100-599 and count >= 1")
		return
	}
	s.Inject(f)
	w.WriteHeader(http.StatusNoContent)
}

// TokenURL is the URL of the fake token endpoint.
func (s *Server) TokenURL() string { return s.URL + "/token" }

// AddZone creates a zone with the default NS and SOA records.
func (s *Server) AddZone(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.zones[name]; ok {
		return
	}
	s.order = append(s.order, name)
	ttl := 3600.0
	var recs []*Record
	for _, ns := range []string{"ns1.contabo.net", "ns2.contabo.net", "ns3.contabo.net"} {
		recs = append(recs, s.newRecord(name, "NS", ns, &ttl))
	}
	recs = append(recs, s.newRecord(name, "SOA", "ns1.contabo.net. hostmaster.contabo.de. 2026110301 10800 3600 604800 3600", &ttl))
	s.zones[name] = recs
}

func (s *Server) newRecord(name, typ, data string, ttl *float64) *Record {
	s.nextID++
	n := name
	return &Record{TenantID: "INT", CustomerID: "1", RecordID: s.nextID, Name: &n, Type: typ, TTL: ttl, Data: data}
}

// Seed stores a record as-is, without validation, and returns its ID.
func (s *Server) Seed(zone string, r Record) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	r.RecordID = s.nextID
	r.TenantID, r.CustomerID = "INT", "1"
	s.zones[zone] = append(s.zones[zone], &r)
	return r.RecordID
}

// Records returns a copy of the records in a zone.
func (s *Server) Records(zone string) []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Record, 0, len(s.zones[zone]))
	for _, r := range s.zones[zone] {
		out = append(out, *r)
	}
	return out
}

// Inject adds a fault.
func (s *Server) Inject(f Fault) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faults = append(s.faults, &f)
}

// ClearFaults removes every fault, as if the outage ended.
func (s *Server) ClearFaults() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faults = nil
}

// Calls returns how often "METHOD /path-pattern" was hit. Patterns use the
// operation names: listZones, listRecords, create, update, delete, bulkDelete, token.
func (s *Server) Calls(op string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[op]
}

// MutatingCalls returns the number of create, update and delete calls.
func (s *Server) MutatingCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls["create"] + s.calls["update"] + s.calls["delete"] + s.calls["bulkDelete"]
}

// RevokeTokens makes every issued token invalid.
func (s *Server) RevokeTokens() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens = map[string]bool{}
}

func (s *Server) count(op string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls[op]++
}

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	s.count("token")
	if s.fault(w, r) {
		return
	}
	_ = r.ParseForm()
	f := r.PostForm
	if f.Get("grant_type") != "password" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported_grant_type"})
		return
	}
	if f.Get("client_id") != ClientID || f.Get("client_secret") != ClientSecret {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized_client", "error_description": "Invalid client or Invalid client credentials"})
		return
	}
	if f.Get("username") != APIUser || f.Get("password") != APIPassword {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant", "error_description": "Invalid user credentials"})
		return
	}
	tok := "tok-" + strconv.FormatInt(s.tokenSeq.Add(1), 10)
	s.mu.Lock()
	s.tokens[tok] = true
	ttl := s.TokenTTL
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": tok, "token_type": "Bearer", "expires_in": ttl,
		"refresh_token": "refresh", "refresh_expires_in": 1800,
	})
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		s.mu.Lock()
		ok := s.tokens[tok]
		s.mu.Unlock()
		if !ok {
			writeError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}
		if r.Header.Get("x-request-id") == "" {
			writeError(w, http.StatusBadRequest, "Bad Request x-request-id header is missing, please include x-request-id header to request")
			return
		}
		if s.fault(w, r) {
			return
		}
		w.Header().Set("ratelimit-limit", "15")
		w.Header().Set("ratelimit-remaining", "14")
		w.Header().Set("ratelimit-reset", "1")
		next(w, r)
	}
}

func (s *Server) fault(w http.ResponseWriter, r *http.Request) bool {
	// Read the body once so faults can match on it, then put it back.
	var body []byte
	if r.Body != nil {
		body, _ = io.ReadAll(io.LimitReader(r.Body, 1<<20))
		r.Body = io.NopCloser(bytes.NewReader(body))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range s.faults {
		if f.Count <= 0 {
			continue
		}
		if (f.Method == "" || f.Method == r.Method) && strings.Contains(r.URL.Path, f.Path) &&
			(f.BodyContains == "" || bytes.Contains(body, []byte(f.BodyContains))) {
			f.Count--
			for k, v := range f.Header {
				w.Header()[k] = v
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(f.Status)
			_, _ = fmt.Fprintf(w, `{"statusCode":%d,"message":"injected fault"}`, f.Status)
			return true
		}
	}
	return false
}

func paging(r *http.Request) (page, size int) {
	page, _ = strconv.Atoi(r.URL.Query().Get("page"))
	size, _ = strconv.Atoi(r.URL.Query().Get("size"))
	if page < 1 {
		page = 1
	}
	if size < 1 {
		size = 10
	}
	if size > 1000 {
		size = 1000
	}
	return page, size
}

func pageOf[T any](items []T, page, size int) ([]T, map[string]any) {
	total := len(items)
	pages := (total + size - 1) / size
	start := min((page-1)*size, total)
	end := min(start+size, total)
	return items[start:end], map[string]any{"size": size, "totalElements": total, "totalPages": pages, "page": page}
}

func (s *Server) listZones(w http.ResponseWriter, r *http.Request) {
	s.count("listZones")
	s.mu.Lock()
	names := append([]string(nil), s.order...)
	s.mu.Unlock()
	sort.Strings(names)
	type zone struct {
		TenantID   string `json:"tenantId"`
		CustomerID string `json:"customerId"`
		ZoneName   string `json:"zoneName"`
	}
	all := make([]zone, 0, len(names))
	for _, n := range names {
		all = append(all, zone{"INT", "1", n})
	}
	page, size := paging(r)
	data, meta := pageOf(all, page, size)
	writeJSON(w, http.StatusOK, map[string]any{"data": data, "_pagination": meta, "_links": map[string]string{"self": r.URL.Path}})
}

func (s *Server) zoneOr404(w http.ResponseWriter, r *http.Request) (string, bool) {
	zone := r.PathValue("zone")
	s.mu.Lock()
	_, ok := s.zones[zone]
	s.mu.Unlock()
	if !ok {
		writeError(w, http.StatusNotFound, "Entry DNS Zone not found by zone name = "+zone)
	}
	return zone, ok
}

func (s *Server) getZone(w http.ResponseWriter, r *http.Request) {
	s.count("getZone")
	zone, ok := s.zoneOr404(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"data":   []map[string]string{{"tenantId": "INT", "customerId": "1", "zoneName": zone}},
		"_links": map[string]string{"self": r.URL.Path},
	})
}

func (s *Server) listRecords(w http.ResponseWriter, r *http.Request) {
	s.count("listRecords")
	zone, ok := s.zoneOr404(w, r)
	if !ok {
		return
	}
	recs := s.Records(zone)
	page, size := paging(r)
	data, meta := pageOf(recs, page, size)
	writeJSON(w, http.StatusOK, map[string]any{"data": data, "_pagination": meta, "_links": map[string]string{"self": r.URL.Path}})
}

type recordBody struct {
	Name   *string  `json:"name"`
	Type   string   `json:"type"`
	TTL    *float64 `json:"ttl"`
	Prio   *float64 `json:"prio"`
	Data   *string  `json:"data"`
	Port   *float64 `json:"port"`
	Weight *float64 `json:"weight"`
	Flag   *float64 `json:"flag"`
	Tag    *string  `json:"tag"`
}

var writable = map[string]bool{"A": true, "AAAA": true, "CAA": true, "CNAME": true, "MX": true, "SRV": true, "TXT": true}

func (b recordBody) validate() []string {
	var msgs []string
	if b.TTL == nil {
		msgs = append(msgs, "ttl must be a number conforming to the specified constraints")
	}
	if b.Prio == nil {
		msgs = append(msgs, "prio must be a number conforming to the specified constraints")
	}
	if !writable[b.Type] {
		msgs = append(msgs, "type must be one of the following values: A, AAAA, CAA, CNAME, MX, SRV, TXT")
	}
	if b.Data == nil {
		msgs = append(msgs, "data must be a string")
	}
	return msgs
}

// fqdn mirrors how the real API resolves a stored name.
func fqdn(name *string, zone string) string {
	if name == nil {
		return zone
	}
	n := strings.ToLower(strings.TrimSuffix(*name, "."))
	switch {
	case n == "" || n == zone:
		return zone
	case strings.HasSuffix(n, "."+zone):
		return n
	default:
		return n + "." + zone
	}
}

// sameData reports whether two record values count as duplicates. For TXT
// the real API ignores one pair of surrounding quotes: creating "abc" next
// to abc is a 409 (checked 2026-11-04).
func sameData(typ, a, b string) bool {
	if typ == "TXT" {
		return unquoteTXT(a) == unquoteTXT(b)
	}
	return a == b
}

func unquoteTXT(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

func externalIP(typ, data string) bool {
	ip, err := netip.ParseAddr(data)
	if err != nil || (typ == "A") != ip.Is4() {
		return false
	}
	if !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return false
	}
	for _, p := range []string{"192.0.2.0/24", "198.51.100.0/24", "203.0.113.0/24", "100.64.0.0/10", "2001:db8::/32"} {
		if netip.MustParsePrefix(p).Contains(ip) {
			return false
		}
	}
	return true
}

func (s *Server) checkData(zone, name string, b recordBody, skipID int64) (int, string) {
	if b.Type == "A" || b.Type == "AAAA" {
		if !externalIP(b.Type, *b.Data) {
			return http.StatusBadRequest, "Bad Request The data parameter must be valid external IPV4 address if record type is A or a valid IPV6 address if record type is AAAA"
		}
	}
	if name == "@."+zone || strings.HasPrefix(name, "@.") {
		return http.StatusBadRequest, "Bad Request Invalid record name: " + name
	}
	for _, rec := range s.zones[zone] {
		if rec.RecordID == skipID || fqdn(rec.Name, zone) != name {
			continue
		}
		if rec.Type == b.Type && sameData(b.Type, rec.Data, *b.Data) {
			return http.StatusConflict, "Entry Record is not unique by name = " + name
		}
		if (b.Type == "CNAME") != (rec.Type == "CNAME") && rec.Type != "NS" && rec.Type != "SOA" {
			return http.StatusBadRequest, fmt.Sprintf("Bad Request Bad Request RRset %s. IN CNAME: Conflicts with pre-existing RRset", name)
		}
		if b.Type == "CNAME" && rec.Type == "CNAME" {
			return http.StatusBadRequest, fmt.Sprintf("Bad Request RRset %s. IN CNAME: has more than one record", name)
		}
	}
	return 0, ""
}

func (s *Server) createRecord(w http.ResponseWriter, r *http.Request) {
	s.count("create")
	zone, ok := s.zoneOr404(w, r)
	if !ok {
		return
	}
	var b recordBody
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&b); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if msgs := b.validate(); len(msgs) > 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"message": msgs, "statusCode": 400})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	name := fqdn(b.Name, zone)
	if code, msg := s.checkData(zone, name, b, 0); code != 0 {
		writeError(w, code, msg)
		return
	}
	s.nextID++
	rec := &Record{TenantID: "INT", CustomerID: "1", RecordID: s.nextID, Name: b.Name, Type: b.Type,
		TTL: b.TTL, Prio: b.Prio, Data: *b.Data, Port: b.Port, Weight: b.Weight, Flag: b.Flag, Tag: b.Tag}
	s.zones[zone] = append(s.zones[zone], rec)
	writeJSON(w, http.StatusCreated, map[string]any{"data": []*Record{rec}, "_links": map[string]string{"self": r.URL.Path}})
}

func (s *Server) updateRecord(w http.ResponseWriter, r *http.Request) {
	s.count("update")
	zone, ok := s.zoneOr404(w, r)
	if !ok {
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var b recordBody
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&b); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if b.Name != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"message": []string{"property name should not exist"}, "statusCode": 400})
		return
	}
	if msgs := b.validate(); len(msgs) > 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"message": msgs, "statusCode": 400})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var rec *Record
	for _, x := range s.zones[zone] {
		if x.RecordID == id {
			rec = x
		}
	}
	if rec == nil {
		writeError(w, http.StatusInternalServerError, "Internal Server Error, retry or contact support")
		return
	}
	name := fqdn(rec.Name, zone)
	if code, msg := s.checkData(zone, name, b, id); code != 0 {
		writeError(w, code, msg)
		return
	}
	rec.Name = &name
	rec.Type, rec.TTL, rec.Prio, rec.Data = b.Type, b.TTL, b.Prio, *b.Data
	rec.Port, rec.Weight, rec.Flag, rec.Tag = b.Port, b.Weight, b.Flag, b.Tag
	writeJSON(w, http.StatusOK, map[string]any{"data": []*Record{rec}, "_links": map[string]string{"self": r.URL.Path}})
}

func (s *Server) deleteRecord(w http.ResponseWriter, r *http.Request) {
	s.count("delete")
	if r.Header.Get("Content-Type") == "application/json" && r.ContentLength <= 0 {
		writeError(w, http.StatusBadRequest, "Body cannot be empty when content-type is set to 'application/json'")
		return
	}
	zone, ok := s.zoneOr404(w, r)
	if !ok {
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	s.remove(zone, id)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) bulkDelete(w http.ResponseWriter, r *http.Request) {
	s.count("bulkDelete")
	zone, ok := s.zoneOr404(w, r)
	if !ok {
		return
	}
	var b struct {
		RecordIDs []int64 `json:"recordIds"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&b); err != nil || b.RecordIDs == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"message": []string{"recordIds must be an array"}, "statusCode": 400})
		return
	}
	for _, id := range b.RecordIDs {
		s.remove(zone, id)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"data":   []map[string]any{{"tenantId": "INT", "customerId": "1", "failedIds": []int64{}}},
		"_links": map[string]string{"self": r.URL.Path},
	})
}

func (s *Server) remove(zone string, id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	recs := s.zones[zone]
	for i, x := range recs {
		if x.RecordID == id {
			s.zones[zone] = append(recs[:i:i], recs[i+1:]...)
			return
		}
	}
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"statusCode": code, "message": msg})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
