// Package contabo is a small client for the DNS part of the Contabo API.
//
// It only covers what the webhook needs: list zones, list records, and
// create, update and delete records. It handles login, the x-request-id
// header, pagination, rate limits and retries.
package contabo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/time/rate"

	"github.com/hayone1/external-dns-contabo-webhook/internal/metrics"
)

const (
	// pageSize is the largest page the API returns; bigger values are cut to 1000.
	pageSize = 1000
	// maxPages stops a broken pagination response from looping forever.
	maxPages = 10000
	// maxResponseBytes caps how much of a response body is read.
	maxResponseBytes = 32 << 20
	// maxBackoff caps the wait between retries.
	maxBackoff = 30 * time.Second
)

// Options configures a Client.
type Options struct {
	APIURL         string
	AuthURL        string
	Credentials    Credentials
	RequestTimeout time.Duration
	// RateLimit is the most requests per second the client sends.
	RateLimit  float64
	MaxRetries int
	UserAgent  string
	Logger     *slog.Logger
	Metrics    *metrics.Metrics
	// HTTPClient is used for API and token calls. Tests use it to inject a
	// fake server. If nil, a client with RequestTimeout is created.
	HTTPClient *http.Client
	// BaseBackoff is the first retry delay. It defaults to 500ms.
	BaseBackoff time.Duration
}

// Client talks to the Contabo DNS API. It is safe for concurrent use.
type Client struct {
	base        string
	http        *http.Client
	tokens      *tokenCache
	limiter     *rate.Limiter
	maxRetries  int
	userAgent   string
	log         *slog.Logger
	metrics     *metrics.Metrics
	baseBackoff time.Duration
	sleep       func(ctx context.Context, d time.Duration) error
}

// New creates a Client.
func New(o Options) (*Client, error) {
	base := strings.TrimRight(o.APIURL, "/")
	if u, err := url.Parse(base); err != nil || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("contabo: invalid API URL %q", o.APIURL)
	}
	if o.RequestTimeout <= 0 {
		o.RequestTimeout = 30 * time.Second
	}
	if o.RateLimit <= 0 {
		o.RateLimit = 10
	}
	if o.MaxRetries < 0 {
		o.MaxRetries = 0
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.HTTPClient == nil {
		o.HTTPClient = &http.Client{Timeout: o.RequestTimeout}
	}
	if o.BaseBackoff <= 0 {
		o.BaseBackoff = 500 * time.Millisecond
	}
	burst := max(1, int(o.RateLimit))

	m := o.Metrics
	return &Client{
		base:        base,
		http:        o.HTTPClient,
		tokens:      newTokenCache(o.AuthURL, o.Credentials, o.HTTPClient, o.RequestTimeout, m.ObserveAuthFailure),
		limiter:     rate.NewLimiter(rate.Limit(o.RateLimit), burst),
		maxRetries:  o.MaxRetries,
		userAgent:   o.UserAgent,
		log:         o.Logger,
		metrics:     m,
		baseBackoff: o.BaseBackoff,
		sleep:       sleepCtx,
	}, nil
}

// CheckCredentials gets an access token, which proves the credentials work.
func (c *Client) CheckCredentials(ctx context.Context) error {
	_, err := c.tokens.accessToken(ctx)
	return err
}

// ListZones returns every DNS zone in the account.
func (c *Client) ListZones(ctx context.Context) ([]Zone, error) {
	var all []Zone
	err := c.paginate(ctx, "listZones", "/v1/dns/zones", func(body []byte) (pagination, int, error) {
		var r listZonesResponse
		if err := json.Unmarshal(body, &r); err != nil {
			return pagination{}, 0, err
		}
		all = append(all, r.Data...)
		return r.Pagination, len(r.Data), nil
	})
	return all, err
}

// ListRecords returns every record in a zone, including NS and SOA.
func (c *Client) ListRecords(ctx context.Context, zone string) ([]Record, error) {
	var all []Record
	path := "/v1/dns/zones/" + url.PathEscape(zone) + "/records"
	err := c.paginate(ctx, "listRecords", path, func(body []byte) (pagination, int, error) {
		var r listRecordsResponse
		if err := json.Unmarshal(body, &r); err != nil {
			return pagination{}, 0, err
		}
		all = append(all, r.Data...)
		return r.Pagination, len(r.Data), nil
	})
	return all, err
}

// CreateRecord adds one record to a zone.
func (c *Client) CreateRecord(ctx context.Context, zone string, req RecordRequest) (Record, error) {
	path := "/v1/dns/zones/" + url.PathEscape(zone) + "/records"
	body, err := c.do(ctx, "createRecord", http.MethodPost, path, nil, req)
	if err != nil {
		return Record{}, err
	}
	return firstRecord(body)
}

// UpdateRecord changes one record. The name of a record cannot be changed.
func (c *Client) UpdateRecord(ctx context.Context, zone string, id int64, req RecordRequest) (Record, error) {
	req.Name = ""
	path := "/v1/dns/zones/" + url.PathEscape(zone) + "/records/" + strconv.FormatInt(id, 10)
	body, err := c.do(ctx, "updateRecord", http.MethodPatch, path, nil, req)
	if err != nil {
		return Record{}, err
	}
	return firstRecord(body)
}

// DeleteRecord removes one record. Deleting a record that does not exist is
// not an error.
func (c *Client) DeleteRecord(ctx context.Context, zone string, id int64) error {
	path := "/v1/dns/zones/" + url.PathEscape(zone) + "/records/" + strconv.FormatInt(id, 10)
	_, err := c.do(ctx, "deleteRecord", http.MethodDelete, path, nil, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// BulkDeleteRecords removes several records with one call and returns the
// IDs the API could not delete.
func (c *Client) BulkDeleteRecords(ctx context.Context, zone string, ids []int64) ([]int64, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	path := "/v1/dns/zones/" + url.PathEscape(zone) + "/records/bulk"
	body, err := c.do(ctx, "bulkDeleteRecords", http.MethodDelete, path, nil, bulkDeleteRequest{RecordIDs: ids})
	if err != nil {
		return nil, err
	}
	var r bulkDeleteResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("contabo bulkDeleteRecords: decode response: %w", err)
	}
	var failed []int64
	for _, d := range r.Data {
		for _, id := range d.FailedIDs {
			failed = append(failed, int64(id))
		}
	}
	return failed, nil
}

func firstRecord(body []byte) (Record, error) {
	var r recordResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return Record{}, fmt.Errorf("contabo: decode record response: %w", err)
	}
	if len(r.Data) == 0 {
		return Record{}, nil
	}
	return r.Data[0], nil
}

func (c *Client) paginate(ctx context.Context, op, path string, page func([]byte) (pagination, int, error)) error {
	for p := 1; p <= maxPages; p++ {
		q := url.Values{"page": {strconv.Itoa(p)}, "size": {strconv.Itoa(pageSize)}}
		body, err := c.do(ctx, op, http.MethodGet, path, q, nil)
		if err != nil {
			return err
		}
		meta, n, err := page(body)
		if err != nil {
			return fmt.Errorf("contabo %s: decode page %d: %w", op, p, err)
		}
		// Stop on the last page, on an empty page, or when the API reports
		// no paging data at all.
		if n == 0 || int(meta.TotalPages) <= p {
			return nil
		}
	}
	return fmt.Errorf("contabo %s: more than %d pages, giving up", op, maxPages)
}

// do sends one API request with retries and returns the response body.
func (c *Client) do(ctx context.Context, op, method, path string, query url.Values, in any) ([]byte, error) {
	var payload []byte
	if in != nil {
		var err error
		if payload, err = json.Marshal(in); err != nil {
			return nil, fmt.Errorf("contabo %s: encode request: %w", op, err)
		}
	}

	// path is already escaped, so join strings instead of using url.URL.Path.
	rawURL := c.base + path
	if len(query) > 0 {
		rawURL += "?" + query.Encode()
	}

	refreshedToken := false
	for attempt := 0; ; attempt++ {
		body, retryAfter, err := c.once(ctx, op, method, rawURL, payload)
		if err == nil {
			return body, nil
		}

		// A 401 usually means the cached token was revoked early. Get a new
		// token and try once more; this does not count as a retry.
		if apiErr, ok := errors.AsType[*APIError](err); ok && apiErr.StatusCode == http.StatusUnauthorized && !refreshedToken {
			refreshedToken = true
			attempt--
			c.metrics.ObserveRetry(op, "unauthorized")
			continue
		}
		if !IsTemporary(err) || ctx.Err() != nil || attempt >= c.maxRetries {
			return nil, err
		}

		wait := c.backoff(attempt, retryAfter)
		reason := "network"
		if apiErr, ok := errors.AsType[*APIError](err); ok {
			reason = strconv.Itoa(apiErr.StatusCode)
		}
		c.metrics.ObserveRetry(op, reason)
		c.log.Debug("retrying Contabo API request", "operation", op, "attempt", attempt+1, "reason", reason, "wait", wait, "error", err)
		if err := c.sleep(ctx, wait); err != nil {
			return nil, err
		}
	}
}

// once sends a single request. It returns the body on success, or an error
// plus the server's suggested wait time.
func (c *Client) once(ctx context.Context, op, method, rawURL string, payload []byte) ([]byte, time.Duration, error) {
	if err := c.limiter.Wait(ctx); err != nil {
		return nil, 0, err
	}
	token, err := c.tokens.accessToken(ctx)
	if err != nil {
		return nil, 0, err
	}

	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return nil, 0, fmt.Errorf("contabo %s: build request: %w", op, err)
	}
	requestID := uuid.NewString()
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("x-request-id", requestID)
	req.Header.Set("Accept", "application/json")
	if c.userAgent != "" {
		req.Header.Set("User-Agent", c.userAgent)
	}
	// The API rejects a JSON content type on a request without a body.
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	start := time.Now()
	resp, err := c.http.Do(req)
	if err != nil {
		c.metrics.ObserveRequest(op, 0, time.Since(start))
		return nil, 0, fmt.Errorf("contabo %s: %w", op, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	c.metrics.ObserveRequest(op, resp.StatusCode, time.Since(start))
	if v, err := strconv.ParseFloat(resp.Header.Get("ratelimit-remaining"), 64); err == nil {
		c.metrics.SetRateLimitRemaining(v)
	}

	c.log.Debug("Contabo API call", "operation", op, "method", method, "status", resp.StatusCode,
		"duration", time.Since(start), "requestID", requestID)

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if readErr != nil {
			return nil, 0, fmt.Errorf("contabo %s: read response: %w", op, readErr)
		}
		return data, 0, nil
	}
	apiErr := &APIError{Operation: op, StatusCode: resp.StatusCode, Message: parseErrorMessage(data), RequestID: requestID}
	if resp.StatusCode == http.StatusUnauthorized {
		c.tokens.invalidate(token)
	}
	return nil, retryAfter(resp.Header), apiErr
}

// retryAfter reads the server's suggested wait from Retry-After or the
// ratelimit-reset header. Both hold a number of seconds.
func retryAfter(h http.Header) time.Duration {
	for _, key := range []string{"Retry-After", "ratelimit-reset"} {
		v := strings.TrimSpace(h.Get(key))
		if v == "" {
			continue
		}
		if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
			return time.Duration(secs) * time.Second
		}
		if t, err := http.ParseTime(v); err == nil {
			if d := time.Until(t); d > 0 {
				return d
			}
		}
	}
	return 0
}

// backoff returns an exponential delay with full jitter, or the server hint
// if that is longer.
func (c *Client) backoff(attempt int, hint time.Duration) time.Duration {
	d := c.baseBackoff << min(attempt, 10)
	if d > maxBackoff || d <= 0 {
		d = maxBackoff
	}
	d = d/2 + rand.N(d/2+1) //nolint:gosec // jitter for retry timing, not security
	if hint > d {
		d = min(hint, maxBackoff)
	}
	return d
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
