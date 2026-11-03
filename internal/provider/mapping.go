package provider

import (
	"fmt"
	"net/netip"
	"strconv"
	"strings"

	"sigs.k8s.io/external-dns/endpoint"

	"github.com/hayone1/external-dns-contabo-webhook/internal/contabo"
)

// normalizeName lower-cases a DNS name and removes the trailing dot.
func normalizeName(name string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), "."))
}

// recordFQDN works out the full name of a record.
//
// The Contabo API stores the name exactly as it was sent, and it accepts
// three spellings that all mean the same record:
//
//	"www"                  relative to the zone
//	"www.example.com"      full name
//	"www.example.com."     full name with a trailing dot
//
// The zone apex can be "", "@" or the zone name. Contabo itself decides
// that a name is already full when it ends with the zone name, so this
// function does the same.
func recordFQDN(recordName, zone string) string {
	name := normalizeName(recordName)
	switch {
	case name == "" || name == "@" || name == zone:
		return zone
	case strings.HasSuffix(name, "."+zone):
		return name
	default:
		return name + "." + zone
	}
}

// zoneFor returns the zone that owns name, picking the longest match so that
// a delegated child zone wins over its parent. zones must be normalized.
func zoneFor(name string, zones []string) (string, bool) {
	best := ""
	for _, z := range zones {
		if (name == z || strings.HasSuffix(name, "."+z)) && len(z) > len(best) {
			best = z
		}
	}
	return best, best != ""
}

// supportedType reports whether the webhook can manage a record type.
func supportedType(t string) bool {
	return contabo.WritableTypes[t]
}

// recordTarget turns a Contabo record into an external-dns target string.
func recordTarget(r contabo.Record) (string, bool) {
	data := strings.TrimSpace(r.Data)
	if data == "" {
		return "", false
	}
	switch r.Type {
	case contabo.TypeA, contabo.TypeAAAA:
		if ip, err := netip.ParseAddr(data); err == nil {
			return ip.Unmap().String(), true
		}
		return data, true
	case contabo.TypeCNAME:
		return strings.TrimSuffix(data, "."), true
	case contabo.TypeTXT:
		// TXT data is passed through untouched so that external-dns
		// ownership records round-trip exactly.
		return r.Data, true
	case contabo.TypeMX:
		return fmt.Sprintf("%d %s", r.Prio, strings.TrimSuffix(data, ".")), true
	case contabo.TypeSRV:
		return fmt.Sprintf("%d %d %d %s", r.Prio, r.Weight, r.Port, data), true
	case contabo.TypeCAA:
		return fmt.Sprintf("%d %s %s", r.Flag, r.Tag, strconv.Quote(unquote(data))), true
	}
	return "", false
}

// targetRequest turns one external-dns target into a Contabo record request.
func targetRequest(fqdn, recordType, target string, ttl int64) (contabo.RecordRequest, error) {
	req := contabo.RecordRequest{Name: fqdn, Type: recordType, TTL: ttl}
	target = strings.TrimSpace(target)
	if target == "" {
		return req, fmt.Errorf("%s %s: empty target", recordType, fqdn)
	}
	switch recordType {
	case contabo.TypeA, contabo.TypeAAAA:
		ip, err := netip.ParseAddr(target)
		if err != nil {
			return req, fmt.Errorf("%s %s: %q is not an IP address", recordType, fqdn, target)
		}
		ip = ip.Unmap()
		if (recordType == contabo.TypeA) != ip.Is4() {
			return req, fmt.Errorf("%s %s: %q is the wrong IP version for this record type", recordType, fqdn, target)
		}
		req.Data = ip.String()
	case contabo.TypeCNAME:
		req.Data = strings.TrimSuffix(target, ".")
	case contabo.TypeTXT:
		req.Data = target
	case contabo.TypeMX:
		f := strings.Fields(target)
		if len(f) != 2 {
			return req, fmt.Errorf("MX %s: target %q must look like \"10 mail.example.com\"", fqdn, target)
		}
		prio, err := parseUint16(f[0])
		if err != nil {
			return req, fmt.Errorf("MX %s: bad priority in %q: %w", fqdn, target, err)
		}
		req.Prio, req.Data = prio, strings.TrimSuffix(f[1], ".")
	case contabo.TypeSRV:
		f := strings.Fields(target)
		if len(f) != 4 {
			return req, fmt.Errorf("SRV %s: target %q must look like \"10 5 443 host.example.com\"", fqdn, target)
		}
		var nums [3]int64
		for i := range nums {
			n, err := parseUint16(f[i])
			if err != nil {
				return req, fmt.Errorf("SRV %s: bad number in %q: %w", fqdn, target, err)
			}
			nums[i] = n
		}
		req.Prio, req.Weight, req.Port, req.Data = nums[0], &nums[1], &nums[2], f[3]
	case contabo.TypeCAA:
		flag, tag, value, err := parseCAA(target)
		if err != nil {
			return req, fmt.Errorf("CAA %s: %w", fqdn, err)
		}
		req.Flag, req.Tag, req.Data = &flag, tag, value
	default:
		return req, fmt.Errorf("record type %s is not supported by Contabo", recordType)
	}
	return req, nil
}

// canonicalTarget gives a form of target that is safe to compare. Two targets
// that Contabo serves the same way get the same canonical form.
func canonicalTarget(recordType, target string) string {
	target = strings.TrimSpace(target)
	switch recordType {
	case contabo.TypeA, contabo.TypeAAAA:
		if ip, err := netip.ParseAddr(target); err == nil {
			return ip.Unmap().String()
		}
		return strings.ToLower(target)
	case contabo.TypeCNAME:
		return normalizeName(target)
	case contabo.TypeTXT:
		// Contabo serves "abc" and abc as the same TXT value.
		return unquote(target)
	case contabo.TypeMX, contabo.TypeSRV:
		f := strings.Fields(target)
		if len(f) > 0 {
			f[len(f)-1] = normalizeName(f[len(f)-1])
		}
		return strings.Join(f, " ")
	case contabo.TypeCAA:
		if flag, tag, value, err := parseCAA(target); err == nil {
			return fmt.Sprintf("%d %s %s", flag, strings.ToLower(tag), strconv.Quote(value))
		}
	}
	return target
}

// parseCAA reads `0 issue "letsencrypt.org"`. The quotes are optional.
func parseCAA(target string) (int64, string, string, error) {
	parts := strings.SplitN(strings.TrimSpace(target), " ", 3)
	if len(parts) != 3 {
		return 0, "", "", fmt.Errorf("target %q must look like `0 issue \"letsencrypt.org\"`", target)
	}
	flag, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || flag < 0 || flag > 255 {
		return 0, "", "", fmt.Errorf("bad flag in %q", target)
	}
	tag := strings.ToLower(strings.TrimSpace(parts[1]))
	if tag == "" {
		return 0, "", "", fmt.Errorf("missing tag in %q", target)
	}
	value := unquote(strings.TrimSpace(parts[2]))
	return flag, tag, value, nil
}

func parseUint16(s string) (int64, error) {
	n, err := strconv.ParseUint(s, 10, 16)
	return int64(n), err
}

// unquote removes one pair of surrounding double quotes, if present.
func unquote(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

// publicIP reports whether Contabo will accept ip as A or AAAA data. The API
// only takes "external" addresses: private, loopback, link-local, shared
// (CGNAT), documentation and other reserved ranges are refused.
func publicIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return false
	}
	for _, p := range nonPublicPrefixes {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}

var nonPublicPrefixes = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{
		"0.0.0.0/8",       // "this network"
		"100.64.0.0/10",   // shared address space (CGNAT)
		"192.0.0.0/24",    // IETF protocol assignments
		"192.0.2.0/24",    // TEST-NET-1
		"198.18.0.0/15",   // benchmarking
		"198.51.100.0/24", // TEST-NET-2
		"203.0.113.0/24",  // TEST-NET-3
		"240.0.0.0/4",     // reserved
		"2001:db8::/32",   // IPv6 documentation
		"64:ff9b:1::/48",  // local-use NAT64
		"100::/64",        // discard-only
	} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

// endpointKey identifies one record set.
type endpointKey struct {
	name       string
	recordType string
}

func keyOf(ep *endpoint.Endpoint) endpointKey {
	return endpointKey{name: normalizeName(ep.DNSName), recordType: ep.RecordType}
}
