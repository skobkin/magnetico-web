// Package crawler is the crawler-side delivery pipeline
// (docs/specs/crawler-ingest-delivery-v1.md §5.4, §5.6, §8):
//
//	DHT → precheck → bounded metadata retrieval → durable SQLite queue →
//	authenticated HTTP sender → PostgreSQL ingest
//
// It has no PostgreSQL dependency. Tokens are supplied by the caller (from the
// environment) and are never logged.
package crawler

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client timeouts (spec §5.4).
const (
	ExistsTimeout = 10 * time.Second
	SubmitTimeout = 60 * time.Second

	// maxResponseRead bounds how much of any response body is read.
	maxResponseRead = 64 << 10
)

// ErrInvalidIngestURL reports an unusable ingest URL.
var ErrInvalidIngestURL = errors.New("the ingest URL must be an https:// URL (plain http:// is allowed only for a loopback host)")

// ValidateIngestURL enforces https, except plain http to a loopback host
// (tests and local smoke runs).
func ValidateIngestURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil {
		return nil, ErrInvalidIngestURL
	}
	switch u.Scheme {
	case "https":
	case "http":
		host := u.Hostname()
		ip := net.ParseIP(host)
		if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return nil, ErrInvalidIngestURL
		}
	default:
		return nil, ErrInvalidIngestURL
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawQuery, u.Fragment = "", ""
	return u, nil
}

// IdleConnTimeout is how long the default transport keeps an idle connection.
// It must stay below the ingest server's idle timeout (120 s) so that the
// client, not the server, closes an idle connection: no reconnect storms.
const IdleConnTimeout = 90 * time.Second

// defaultTransport is the transport of a client built without one. It sets no
// TLSClientConfig and no custom dialer, so Go negotiates HTTP/2 over TLS
// (ALPN) by itself; a plain http:// URL (loopback only) uses HTTP/1.1 keep-alive.
func defaultTransport() *http.Transport {
	return &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		MaxIdleConnsPerHost: 2,
		IdleConnTimeout:     IdleConnTimeout,
		TLSHandshakeTimeout: 10 * time.Second,
	}
}

// Client talks to the ingest HTTP API.
type Client struct {
	base  *url.URL
	token string
	http  *http.Client
}

// NewClient validates the URL and builds a client. httpClient may be nil.
func NewClient(rawURL, token string, httpClient *http.Client) (*Client, error) {
	u, err := ValidateIngestURL(rawURL)
	if err != nil {
		return nil, err
	}
	if httpClient == nil {
		httpClient = &http.Client{Transport: defaultTransport(),
			// A redirect is never followed: the bearer token must only go to
			// the configured URL. A 3xx surfaces as an ambiguous status.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	return &Client{base: u, token: token, http: httpClient}, nil
}

func (c *Client) endpoint(path string) string {
	u := *c.base
	u.Path += "/api/ingest/v1" + path
	return u.String()
}

// ExistsResult classifies the requested hashes.
type ExistsResult struct {
	Exists  [][20]byte
	Missing [][20]byte
	Unknown [][20]byte
}

// Exists asks which hashes are already stored. Any error means the whole
// answer is unusable (callers treat every hash as unknown).
func (c *Client) Exists(ctx context.Context, hashes [][20]byte) (ExistsResult, error) {
	ctx, cancel := context.WithTimeout(ctx, ExistsTimeout)
	defer cancel()

	hx := make([]string, len(hashes))
	for i, h := range hashes {
		hx[i] = hex.EncodeToString(h[:])
	}
	body, _ := json.Marshal(map[string][]string{"info_hashes": hx})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint("/torrents/exists"), bytes.NewReader(body))
	if err != nil {
		return ExistsResult{}, err
	}
	c.decorate(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return ExistsResult{}, errors.New("exists request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseRead))
		return ExistsResult{}, fmt.Errorf("exists: HTTP %d", resp.StatusCode)
	}
	var out struct {
		Exists  []string `json:"exists"`
		Missing []string `json:"missing"`
		Unknown []string `json:"unknown"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseRead)).Decode(&out); err != nil {
		return ExistsResult{}, errors.New("exists: malformed response")
	}
	var res ExistsResult
	seen := make(map[[20]byte]bool, len(hashes))
	parse := func(list []string, dst *[][20]byte) {
		for _, s := range list {
			var h [20]byte
			if len(s) != 40 {
				continue
			}
			if _, err := hex.Decode(h[:], []byte(s)); err != nil {
				continue
			}
			seen[h] = true
			*dst = append(*dst, h)
		}
	}
	parse(out.Exists, &res.Exists)
	parse(out.Missing, &res.Missing)
	parse(out.Unknown, &res.Unknown)
	// Anything the server did not classify is unknown, never missing.
	for _, h := range hashes {
		if !seen[h] {
			res.Unknown = append(res.Unknown, h)
		}
	}
	return res, nil
}

func (c *Client) decorate(r *http.Request) {
	r.Header.Set("Authorization", "Bearer "+c.token)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json")
	r.Header.Set("User-Agent", "magnetico-crawler")
}

// SubmitKind classifies a submit outcome for the sender (spec §5.4).
type SubmitKind int

const (
	// SubmitAccepted: status 202 — every record is durably queued.
	SubmitAccepted SubmitKind = iota
	// SubmitRecordRejected: a recognised record-specific rejection of a
	// single-record request (400 invalid_record / unsupported_version, 413
	// record_too_large, as a well-formed ingest JSON error body).
	SubmitRecordRejected
	// SubmitRejected: any other 400/413 — says nothing certain about a
	// record's content. More than one record → bisect; one record → keep.
	SubmitRejected
	// SubmitUnauthorized: 401/403.
	SubmitUnauthorized
	// SubmitRetryable: 5xx, 429, timeouts, connection failures.
	SubmitRetryable
	// SubmitAmbiguous: any other status (including 2xx other than 202).
	SubmitAmbiguous
)

// SubmitResult is the classified outcome of one submit request.
type SubmitResult struct {
	Kind        SubmitKind
	Status      int    // HTTP status, 0 if none
	Code        string // ingest error code when the body was a well-formed envelope
	EnvelopeOK  bool
	ContentType string
	BodySample  string // ≤ 200 sanitised printable bytes
	Records     int
}

// Submit posts the payloads (each a record v1 JSON document) as one request.
// It never returns an error: every outcome is classified.
func (c *Client) Submit(ctx context.Context, payloads [][]byte) SubmitResult {
	res := SubmitResult{Records: len(payloads)}
	ctx, cancel := context.WithTimeout(ctx, SubmitTimeout)
	defer cancel()

	var buf bytes.Buffer
	buf.WriteString(`{"records":[`)
	for i, p := range payloads {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(p)
	}
	buf.WriteString(`]}`)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint("/torrents"), &buf)
	if err != nil {
		res.Kind = SubmitRetryable
		return res
	}
	c.decorate(req)
	resp, err := c.http.Do(req)
	if err != nil {
		res.Kind = SubmitRetryable // timeout, reset, DNS, TLS: nothing is known
		return res
	}
	defer resp.Body.Close()
	res.Status = resp.StatusCode
	res.ContentType = resp.Header.Get("Content-Type")
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseRead))

	switch {
	case resp.StatusCode == http.StatusAccepted:
		res.Kind = SubmitAccepted // the body is informational and ignored
		return res
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		res.Kind = SubmitUnauthorized
		return res
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		res.Kind = SubmitRetryable
		return res
	case resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusRequestEntityTooLarge:
		res.Code, res.EnvelopeOK = parseEnvelope(res.ContentType, raw)
		res.BodySample = sanitise(raw, 200)
		res.Kind = SubmitRejected
		if len(payloads) == 1 && res.EnvelopeOK && recordSpecific(resp.StatusCode, res.Code) {
			res.Kind = SubmitRecordRejected
		}
		return res
	default:
		res.Kind = SubmitAmbiguous
		return res
	}
}

// recordSpecific reports the three codes that blame one record.
func recordSpecific(status int, code string) bool {
	switch {
	case status == http.StatusBadRequest && (code == "invalid_record" || code == "unsupported_version"):
		return true
	case status == http.StatusRequestEntityTooLarge && code == "record_too_large":
		return true
	}
	return false
}

// parseEnvelope extracts the error code of a well-formed ingest error body
// (application/json with {"error":{"code":…}}).
func parseEnvelope(contentType string, body []byte) (string, bool) {
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil || mt != "application/json" {
		return "", false
	}
	var eb struct {
		Error *struct {
			Code *string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &eb); err != nil || eb.Error == nil || eb.Error.Code == nil {
		return "", false
	}
	return *eb.Error.Code, true
}

// sanitise returns at most n printable ASCII bytes of b, replacing control
// and non-ASCII bytes with '.'.
func sanitise(b []byte, n int) string {
	if len(b) > n {
		b = b[:n]
	}
	out := make([]byte, len(b))
	for i, c := range b {
		if c < 0x20 || c > 0x7e {
			c = '.'
		}
		out[i] = c
	}
	return string(out)
}
