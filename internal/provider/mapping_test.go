package provider

import (
	"net/netip"
	"testing"

	"github.com/hayone1/external-dns-contabo-webhook/internal/contabo"
)

func TestRecordFQDN(t *testing.T) {
	const zone = "example.com"
	tests := map[string]string{
		"":                       "example.com",
		"@":                      "example.com",
		"example.com":            "example.com",
		"example.com.":           "example.com",
		"EXAMPLE.com":            "example.com",
		"www":                    "www.example.com",
		"WWW":                    "www.example.com",
		"www.example.com":        "www.example.com",
		"www.example.com.":       "www.example.com",
		"a.b.c":                  "a.b.c.example.com",
		"*.apps":                 "*.apps.example.com",
		"*.apps.example.com":     "*.apps.example.com",
		"_acme-challenge":        "_acme-challenge.example.com",
		"notexample.com":         "notexample.com.example.com",
		"www.example.com.other":  "www.example.com.other.example.com",
		"a-www.example.com":      "a-www.example.com",
		"cname-www.example.com.": "cname-www.example.com",
	}
	for in, want := range tests {
		if got := recordFQDN(in, zone); got != want {
			t.Errorf("recordFQDN(%q) = %q, want %q", in, got, want)
		}
	}
	if got := recordFQDN("  www.example.com.	", zone); got != "www.example.com" {
		t.Errorf("surrounding spaces not trimmed: %q", got)
	}
}

func TestZoneFor(t *testing.T) {
	zones := []string{"example.com", "sub.example.com", "example.org", "ample.com"}
	tests := map[string]string{
		"example.com":           "example.com",
		"www.example.com":       "example.com",
		"sub.example.com":       "sub.example.com",
		"a.sub.example.com":     "sub.example.com",
		"asub.example.com":      "example.com",
		"www.example.org":       "example.org",
		"ample.com":             "ample.com",
		"x.ample.com":           "ample.com",
		"notexample.com":        "",
		"example.com.evil.test": "",
	}
	for name, want := range tests {
		got, ok := zoneFor(name, zones)
		if got != want || ok != (want != "") {
			t.Errorf("zoneFor(%q) = %q,%v want %q", name, got, ok, want)
		}
	}
}

func TestTargetRoundTrip(t *testing.T) {
	tests := []struct {
		typ, in, want string
	}{
		{"A", "1.2.3.4", "1.2.3.4"},
		{"A", " 1.2.3.4 ", "1.2.3.4"},
		{"A", "::ffff:1.2.3.4", "1.2.3.4"},
		{"AAAA", "2606:4700:4700:0:0:0:0:1111", "2606:4700:4700::1111"},
		{"AAAA", "2606:4700:4700::1111", "2606:4700:4700::1111"},
		{"CNAME", "target.example.net.", "target.example.net"},
		{"CNAME", "target.example.net", "target.example.net"},
		{"TXT", `"heritage=external-dns,external-dns/owner=default"`, `"heritage=external-dns,external-dns/owner=default"`},
		{"TXT", "v=spf1 -all", "v=spf1 -all"},
		{"MX", "10 mail.example.com.", "10 mail.example.com"},
		{"MX", "0 mail.example.com", "0 mail.example.com"},
		{"SRV", "10 5 5060 sip.example.com", "10 5 5060 sip.example.com"},
		{"SRV", "0 0 443 target.example.com.", "0 0 443 target.example.com."},
		{"CAA", `0 issue "letsencrypt.org"`, `0 issue "letsencrypt.org"`},
		{"CAA", `0 issue letsencrypt.org`, `0 issue "letsencrypt.org"`},
		{"CAA", `128 ISSUEWILD ";"`, `128 issuewild ";"`},
		{"CAA", `0 iodef "mailto:security@example.com"`, `0 iodef "mailto:security@example.com"`},
	}
	for _, tt := range tests {
		req, err := targetRequest("x.example.com", tt.typ, tt.in, 300)
		if err != nil {
			t.Errorf("%s %q: %v", tt.typ, tt.in, err)
			continue
		}
		got, ok := recordTarget(recordFromRequest(req))
		if !ok || got != tt.want {
			t.Errorf("%s %q: round trip gave %q, want %q", tt.typ, tt.in, got, tt.want)
		}
		if req.Name != "x.example.com" || req.TTL != 300 {
			t.Errorf("%s %q: name/ttl not set: %+v", tt.typ, tt.in, req)
		}
	}
}

func TestTargetRequestFields(t *testing.T) {
	req, err := targetRequest("_sip._tcp.example.com", "SRV", "10 20 5060 sip.example.com", 60)
	if err != nil {
		t.Fatal(err)
	}
	if req.Prio != 10 || *req.Weight != 20 || *req.Port != 5060 || req.Data != "sip.example.com" {
		t.Fatalf("bad SRV request: %+v", req)
	}
	req, err = targetRequest("example.com", "CAA", `0 issue "letsencrypt.org"`, 60)
	if err != nil {
		t.Fatal(err)
	}
	if *req.Flag != 0 || req.Tag != "issue" || req.Data != "letsencrypt.org" {
		t.Fatalf("bad CAA request: %+v", req)
	}
	req, err = targetRequest("example.com", "MX", "5 mx.example.com", 60)
	if err != nil {
		t.Fatal(err)
	}
	if req.Prio != 5 || req.Data != "mx.example.com" {
		t.Fatalf("bad MX request: %+v", req)
	}
}

func TestTargetRequestErrors(t *testing.T) {
	tests := []struct{ typ, target string }{
		{"A", "not-an-ip"},
		{"A", "2001:4860::1"},
		{"AAAA", "1.2.3.4"},
		{"A", ""},
		{"MX", "mail.example.com"},
		{"MX", "x mail.example.com"},
		{"MX", "70000 mail.example.com"},
		{"SRV", "1 2 host"},
		{"SRV", "1 2 99999 host"},
		{"CAA", "issue letsencrypt.org"},
		{"CAA", "300 issue x"},
		{"NS", "ns1.example.com"},
		{"PTR", "host.example.com"},
	}
	for _, tt := range tests {
		if _, err := targetRequest("x.example.com", tt.typ, tt.target, 300); err == nil {
			t.Errorf("%s %q: expected an error", tt.typ, tt.target)
		}
	}
}

func TestCanonicalTarget(t *testing.T) {
	same := []struct{ typ, a, b string }{
		{"A", "1.2.3.4", " 1.2.3.4"},
		{"AAAA", "2606:4700::1", "2606:4700:0::1"},
		{"CNAME", "Target.Example.net.", "target.example.net"},
		{"TXT", `"hello world"`, `hello world`},
		{"MX", "10 Mail.Example.com.", "10 mail.example.com"},
		{"SRV", "1 2 3 Host.example.com.", "1 2 3 host.example.com"},
		{"CAA", `0 ISSUE letsencrypt.org`, `0 issue "letsencrypt.org"`},
	}
	for _, tt := range same {
		if canonicalTarget(tt.typ, tt.a) != canonicalTarget(tt.typ, tt.b) {
			t.Errorf("%s: %q and %q should be equal", tt.typ, tt.a, tt.b)
		}
	}
	if canonicalTarget("TXT", "abc") == canonicalTarget("TXT", "ABC") {
		t.Error("TXT values are case sensitive")
	}
}

func TestRecordTargetSkipsEmpty(t *testing.T) {
	if _, ok := recordTarget(contabo.Record{Type: "A", Data: " "}); ok {
		t.Error("empty data should be skipped")
	}
	if _, ok := recordTarget(contabo.Record{Type: "NS", Data: "ns1.contabo.net"}); ok {
		t.Error("NS records should be skipped")
	}
}

func TestPublicIP(t *testing.T) {
	tests := map[string]bool{
		"169.58.246.3":         true,
		"8.8.8.8":              true,
		"2606:4700:4700::1111": true,
		"10.0.0.1":             false,
		"172.16.5.4":           false,
		"192.168.1.1":          false,
		"127.0.0.1":            false,
		"169.254.1.1":          false,
		"100.64.0.1":           false,
		"192.0.2.1":            false,
		"198.51.100.7":         false,
		"203.0.113.9":          false,
		"198.18.0.1":           false,
		"0.0.0.0":              false,
		"255.255.255.255":      false,
		"224.0.0.1":            false,
		"::1":                  false,
		"fe80::1":              false,
		"fd00::1":              false,
		"2001:db8::1":          false,
		"::ffff:10.0.0.1":      false,
		"::ffff:8.8.8.8":       true,
	}
	for in, want := range tests {
		if got := publicIP(netip.MustParseAddr(in)); got != want {
			t.Errorf("publicIP(%s) = %v, want %v", in, got, want)
		}
	}
}
