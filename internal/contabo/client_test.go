package contabo_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hayone1/external-dns-contabo-webhook/internal/contabo"
	"github.com/hayone1/external-dns-contabo-webhook/internal/contabo/contabotest"
)

func newClient(t *testing.T, srv *contabotest.Server, mod ...func(*contabo.Options)) *contabo.Client {
	t.Helper()
	o := contabo.Options{
		APIURL:  srv.URL,
		AuthURL: srv.TokenURL(),
		Credentials: contabo.Credentials{
			ClientID: contabotest.ClientID, ClientSecret: contabotest.ClientSecret,
			APIUser: contabotest.APIUser, APIPassword: contabotest.APIPassword,
		},
		RequestTimeout: 5 * time.Second,
		RateLimit:      1000,
		MaxRetries:     3,
		BaseBackoff:    time.Millisecond,
	}
	for _, m := range mod {
		m(&o)
	}
	c, err := contabo.New(o)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestNewRejectsBadURL(t *testing.T) {
	for _, u := range []string{"", "not a url", "https://api.contabo.com?x=1"} {
		if _, err := contabo.New(contabo.Options{APIURL: u}); err == nil {
			t.Errorf("expected error for %q", u)
		}
	}
}

func TestCredentials(t *testing.T) {
	srv := contabotest.New()
	defer srv.Close()

	if err := newClient(t, srv).CheckCredentials(context.Background()); err != nil {
		t.Fatalf("good credentials failed: %v", err)
	}

	bad := newClient(t, srv, func(o *contabo.Options) { o.Credentials.APIPassword = "wrong" })
	err := bad.CheckCredentials(context.Background())
	if !errors.Is(err, contabo.ErrInvalidCredentials) {
		t.Fatalf("want ErrInvalidCredentials, got %v", err)
	}
	if strings.Contains(err.Error(), "wrong") {
		t.Fatalf("error leaks the password: %v", err)
	}
	if contabo.IsTemporary(err) {
		t.Fatal("bad credentials must not be temporary")
	}

	badClient := newClient(t, srv, func(o *contabo.Options) { o.Credentials.ClientSecret = "nope" })
	if err := badClient.CheckCredentials(context.Background()); !errors.Is(err, contabo.ErrInvalidCredentials) {
		t.Fatalf("want ErrInvalidCredentials for bad client secret, got %v", err)
	}
}

func TestTokenIsReused(t *testing.T) {
	srv := contabotest.New()
	defer srv.Close()
	srv.AddZone("example.com")
	c := newClient(t, srv)
	for range 5 {
		if _, err := c.ListZones(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if n := srv.Calls("token"); n != 1 {
		t.Fatalf("token requested %d times, want 1", n)
	}
}

func TestShortLivedTokenIsRenewed(t *testing.T) {
	srv := contabotest.New()
	defer srv.Close()
	srv.TokenTTL = 10 // shorter than the 30s early-expiry window
	srv.AddZone("example.com")
	c := newClient(t, srv)
	for range 3 {
		if _, err := c.ListZones(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if n := srv.Calls("token"); n != 3 {
		t.Fatalf("token requested %d times, want 3", n)
	}
}

func TestRevokedTokenRecovers(t *testing.T) {
	srv := contabotest.New()
	defer srv.Close()
	srv.AddZone("example.com")
	c := newClient(t, srv, func(o *contabo.Options) { o.MaxRetries = 0 })
	if _, err := c.ListZones(context.Background()); err != nil {
		t.Fatal(err)
	}
	srv.RevokeTokens()
	if _, err := c.ListZones(context.Background()); err != nil {
		t.Fatalf("client did not recover from a revoked token: %v", err)
	}
	if n := srv.Calls("token"); n != 2 {
		t.Fatalf("token requested %d times, want 2", n)
	}
}

func TestPagination(t *testing.T) {
	srv := contabotest.New()
	defer srv.Close()
	srv.AddZone("example.com")
	for i := range 2500 {
		name := fmt.Sprintf("host%d", i)
		ttl, prio := 300.0, 0.0
		srv.Seed("example.com", contabotest.Record{Name: &name, Type: "A", TTL: &ttl, Prio: &prio, Data: "1.2.3.4"})
	}
	recs, err := newClient(t, srv).ListRecords(context.Background(), "example.com")
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2504 { // 2500 + 3 NS + 1 SOA
		t.Fatalf("got %d records, want 2504", len(recs))
	}
	if n := srv.Calls("listRecords"); n != 3 {
		t.Fatalf("listRecords called %d times, want 3 pages", n)
	}
}

func TestNullNumbersDecode(t *testing.T) {
	srv := contabotest.New()
	defer srv.Close()
	srv.AddZone("example.com")
	recs, err := newClient(t, srv).ListRecords(context.Background(), "example.com")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if r.Prio != 0 || r.Port != 0 {
			t.Fatalf("null numbers should decode to 0: %+v", r)
		}
	}
}

func TestRetriesTemporaryErrors(t *testing.T) {
	for _, code := range []int{http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			srv := contabotest.New()
			defer srv.Close()
			srv.AddZone("example.com")
			srv.Inject(contabotest.Fault{Path: "/v1/dns/zones", Status: code, Count: 2, Header: http.Header{"Retry-After": {"0"}}})
			zones, err := newClient(t, srv).ListZones(context.Background())
			if err != nil {
				t.Fatalf("expected success after retries: %v", err)
			}
			if len(zones) != 1 || srv.Calls("listZones") != 1 {
				t.Fatalf("zones=%v calls=%d", zones, srv.Calls("listZones"))
			}
		})
	}
}

func TestGivesUpAfterMaxRetries(t *testing.T) {
	srv := contabotest.New()
	defer srv.Close()
	srv.Inject(contabotest.Fault{Path: "/v1/dns/zones", Status: 503, Count: 100})
	_, err := newClient(t, srv, func(o *contabo.Options) { o.MaxRetries = 2 }).ListZones(context.Background())
	var apiErr *contabo.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("want 503 APIError, got %v", err)
	}
	if !contabo.IsTemporary(err) {
		t.Fatal("503 should be temporary")
	}
	if apiErr.RequestID == "" {
		t.Fatal("error should carry the x-request-id for support cases")
	}
}

func TestDoesNotRetryClientErrors(t *testing.T) {
	srv := contabotest.New()
	defer srv.Close()
	srv.AddZone("example.com")
	c := newClient(t, srv)
	_, err := c.CreateRecord(context.Background(), "example.com", contabo.RecordRequest{Name: "x.example.com", Type: "A", TTL: 300, Data: "10.0.0.1"})
	var apiErr *contabo.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %v", err)
	}
	if !strings.Contains(apiErr.Message, "external IPV4") {
		t.Fatalf("message not parsed: %q", apiErr.Message)
	}
	if srv.Calls("create") != 1 {
		t.Fatalf("a 400 must not be retried, got %d calls", srv.Calls("create"))
	}
	if contabo.IsTemporary(err) {
		t.Fatal("400 must not be temporary")
	}
}

func TestValidationMessageList(t *testing.T) {
	srv := contabotest.New()
	defer srv.Close()
	srv.AddZone("example.com")
	_, err := newClient(t, srv).CreateRecord(context.Background(), "example.com", contabo.RecordRequest{Type: "NS", TTL: 1, Data: "x"})
	if err == nil || !strings.Contains(err.Error(), "type must be one of") {
		t.Fatalf("validation list not parsed: %v", err)
	}
}

func TestRecordLifecycle(t *testing.T) {
	srv := contabotest.New()
	defer srv.Close()
	srv.AddZone("example.com")
	c := newClient(t, srv)
	ctx := context.Background()

	rec, err := c.CreateRecord(ctx, "example.com", contabo.RecordRequest{Name: "www.example.com", Type: "A", TTL: 300, Data: "1.2.3.4"})
	if err != nil {
		t.Fatal(err)
	}
	if rec.ID == 0 || rec.Data != "1.2.3.4" {
		t.Fatalf("bad create response: %+v", rec)
	}

	_, err = c.CreateRecord(ctx, "example.com", contabo.RecordRequest{Name: "www", Type: "A", TTL: 300, Data: "1.2.3.4"})
	if !contabo.IsConflict(err) {
		t.Fatalf("duplicate should be a conflict, got %v", err)
	}

	upd, err := c.UpdateRecord(ctx, "example.com", int64(rec.ID), contabo.RecordRequest{Name: "ignored", Type: "A", TTL: 600, Data: "1.2.3.5"})
	if err != nil {
		t.Fatal(err)
	}
	if upd.TTL != 600 || upd.Data != "1.2.3.5" {
		t.Fatalf("update not applied: %+v", upd)
	}

	if err := c.DeleteRecord(ctx, "example.com", int64(rec.ID)); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteRecord(ctx, "example.com", int64(rec.ID)); err != nil {
		t.Fatalf("deleting twice should succeed: %v", err)
	}

	a, _ := c.CreateRecord(ctx, "example.com", contabo.RecordRequest{Name: "a.example.com", Type: "TXT", TTL: 300, Data: "x"})
	b, _ := c.CreateRecord(ctx, "example.com", contabo.RecordRequest{Name: "b.example.com", Type: "TXT", TTL: 300, Data: "y"})
	failed, err := c.BulkDeleteRecords(ctx, "example.com", []int64{int64(a.ID), int64(b.ID), 424242})
	if err != nil || len(failed) != 0 {
		t.Fatalf("bulk delete: failed=%v err=%v", failed, err)
	}
	if n := len(srv.Records("example.com")); n != 4 {
		t.Fatalf("zone should be back to NS+SOA, has %d records", n)
	}
}

func TestZoneNotFound(t *testing.T) {
	srv := contabotest.New()
	defer srv.Close()
	_, err := newClient(t, srv).ListRecords(context.Background(), "missing.example")
	if !contabo.IsNotFound(err) {
		t.Fatalf("want 404, got %v", err)
	}
}

func TestContextCancelStopsRetries(t *testing.T) {
	srv := contabotest.New()
	defer srv.Close()
	srv.Inject(contabotest.Fault{Status: 503, Count: 1000, Header: http.Header{"Retry-After": {"20"}}})
	c := newClient(t, srv, func(o *contabo.Options) { o.MaxRetries = 10 })
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := c.ListZones(ctx); err == nil {
		t.Fatal("expected an error")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("cancel did not stop the retry wait")
	}
}

func TestNumberDecoding(t *testing.T) {
	tests := map[string]int64{`null`: 0, `3600`: 3600, `3600.0`: 3600, `"300"`: 300, `""`: 0, `1.6`: 2}
	for in, want := range tests {
		var n contabo.Number
		if err := n.UnmarshalJSON([]byte(in)); err != nil {
			t.Errorf("%s: %v", in, err)
			continue
		}
		if int64(n) != want {
			t.Errorf("%s: got %d want %d", in, n, want)
		}
	}
	var n contabo.Number
	if err := n.UnmarshalJSON([]byte(`"abc"`)); err == nil {
		t.Error("expected error for non-numeric string")
	}
}

func TestNetworkErrorIsTemporary(t *testing.T) {
	srv := contabotest.New()
	url := srv.URL
	srv.Close()
	c, _ := contabo.New(contabo.Options{APIURL: url, AuthURL: url + "/token", MaxRetries: 0, RequestTimeout: time.Second})
	_, err := c.ListZones(context.Background())
	if err == nil || !contabo.IsTemporary(err) {
		t.Fatalf("connection refused should be temporary: %v", err)
	}
}
