package contabo

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
)

// Record types that the Contabo zone API can create, update and delete.
// NS and SOA records also show up when listing a zone, but the API refuses
// to change them.
const (
	TypeA     = "A"
	TypeAAAA  = "AAAA"
	TypeCAA   = "CAA"
	TypeCNAME = "CNAME"
	TypeMX    = "MX"
	TypeSRV   = "SRV"
	TypeTXT   = "TXT"
)

// WritableTypes lists the record types the API accepts on create and update.
var WritableTypes = map[string]bool{
	TypeA: true, TypeAAAA: true, TypeCAA: true, TypeCNAME: true,
	TypeMX: true, TypeSRV: true, TypeTXT: true,
}

// Number is a JSON number that the API may send as an integer, a float or
// null. It is always stored as a whole number; null becomes 0.
type Number int64

// UnmarshalJSON implements json.Unmarshaler.
func (n *Number) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		*n = 0
		return nil
	}
	// Some APIs quote numbers. Accept that too.
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		if s == "" {
			*n = 0
			return nil
		}
		b = []byte(s)
	}
	f, err := strconv.ParseFloat(string(b), 64)
	if err != nil {
		return fmt.Errorf("contabo: invalid number %q: %w", string(b), err)
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return fmt.Errorf("contabo: invalid number %q", string(b))
	}
	*n = Number(math.Round(f))
	return nil
}

// Zone is one DNS zone in the Contabo account.
type Zone struct {
	TenantID   string `json:"tenantId"`
	CustomerID string `json:"customerId"`
	Name       string `json:"zoneName"`
}

// Record is one DNS record as returned by the API. Each record holds exactly
// one value in Data.
type Record struct {
	ID     Number `json:"recordId"`
	Name   string `json:"name"`
	Type   string `json:"type"`
	TTL    Number `json:"ttl"`
	Prio   Number `json:"prio"`
	Data   string `json:"data"`
	Port   Number `json:"port"`
	Weight Number `json:"weight"`
	Flag   Number `json:"flag"`
	Tag    string `json:"tag"`
}

// RecordRequest is the body for creating or updating a record. Name is only
// used when creating; the update endpoint does not accept it.
type RecordRequest struct {
	Name   string `json:"name,omitempty"`
	Type   string `json:"type"`
	TTL    int64  `json:"ttl"`
	Prio   int64  `json:"prio"`
	Data   string `json:"data"`
	Port   *int64 `json:"port,omitempty"`
	Weight *int64 `json:"weight,omitempty"`
	Flag   *int64 `json:"flag,omitempty"`
	Tag    string `json:"tag,omitempty"`
}

type pagination struct {
	Size          Number `json:"size"`
	TotalElements Number `json:"totalElements"`
	TotalPages    Number `json:"totalPages"`
	Page          Number `json:"page"`
}

type listZonesResponse struct {
	Pagination pagination `json:"_pagination"`
	Data       []Zone     `json:"data"`
}

type listRecordsResponse struct {
	Pagination pagination `json:"_pagination"`
	Data       []Record   `json:"data"`
}

type recordResponse struct {
	Data []Record `json:"data"`
}

type bulkDeleteRequest struct {
	RecordIDs []int64 `json:"recordIds"`
}

type bulkDeleteResponse struct {
	Data []struct {
		FailedIDs []Number `json:"failedIds"`
	} `json:"data"`
}

// apiErrorBody covers both error shapes the API uses: a single message string
// and a list of validation messages.
type apiErrorBody struct {
	StatusCode Number          `json:"statusCode"`
	Message    json.RawMessage `json:"message"`
}
