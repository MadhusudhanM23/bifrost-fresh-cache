package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"time"
)

// Bifrost header names (see transports/bifrost-http/lib/responseheaders.go
// and docs/features/semantic-caching.mdx in the Bifrost repository).
const (
	HeaderCacheHit        = "X-Bifrost-Cache-Hit"
	HeaderCacheID         = "X-Bifrost-Cache-Id"
	HeaderCacheHitType    = "X-Bifrost-Cache-Hit-Type"
	HeaderCacheSimilarity = "X-Bifrost-Cache-Similarity"
	HeaderCacheThreshold  = "X-Bifrost-Cache-Threshold"
	HeaderCacheNoStore    = "X-Bf-Cache-No-Store"
	// HeaderCacheType narrows a lookup: "direct" reads only the exact-request
	// entry and, on a miss, writes one with no embedding (direct-only).
	HeaderCacheType = "X-Bf-Cache-Type"
)

// Request is a buffered client request, kept so it can be re-sent on refresh.
type Request struct {
	Method   string
	Path     string // escaped path, e.g. /v1/chat/completions
	RawQuery string
	Header   http.Header
	Body     []byte
}

// Clone copies the request; the body bytes are shared and never modified.
func (r *Request) Clone() *Request {
	c := *r
	c.Header = r.Header.Clone()
	if c.Header == nil {
		c.Header = http.Header{}
	}
	return &c
}

// Response is a buffered upstream response.
type Response struct {
	StatusCode int
	Header     http.Header
	Body       []byte
}

// CacheHit reports whether Bifrost served this response from its cache.
func (r *Response) CacheHit() bool { return isCacheHit(r.Header) }

// IsSemanticHit reports whether Bifrost answered from a similarity match
// rather than from an exact (direct) one.
func (r *Response) IsSemanticHit() bool {
	return r.CacheHit() && strings.EqualFold(strings.TrimSpace(r.Header.Get(HeaderCacheHitType)), "semantic")
}

// CacheID returns x-bifrost-cache-id.
func (r *Response) CacheID() string { return strings.TrimSpace(r.Header.Get(HeaderCacheID)) }

func isCacheHit(h http.Header) bool {
	return strings.EqualFold(strings.TrimSpace(h.Get(HeaderCacheHit)), "true")
}

func isCacheMiss(h http.Header) bool {
	return strings.EqualFold(strings.TrimSpace(h.Get(HeaderCacheHit)), "false")
}

// Upstream sends requests to Bifrost.
type Upstream struct {
	base             string
	client           *http.Client
	maxResponseBytes int64
}

// NewTransport returns the transport used for Bifrost. Compression is left
// to the client and Bifrost (bytes pass through untouched); headers must
// arrive within responseHeaderTimeout.
func NewTransport(responseHeaderTimeout time.Duration) *http.Transport {
	return &http.Transport{
		Proxy:                 nil, // Bifrost is a direct neighbour; ignore HTTP(S)_PROXY
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		ResponseHeaderTimeout: responseHeaderTimeout,
		DisableCompression:    true,
	}
}

// NewUpstream returns an Upstream for the Bifrost base URL.
func NewUpstream(baseURL string, transport http.RoundTripper, maxResponseBytes int64) (*Upstream, error) {
	base, err := parseBaseURL(baseURL)
	if err != nil {
		return nil, err
	}
	if transport == nil {
		transport = NewTransport(2 * time.Minute)
	}
	if maxResponseBytes <= 0 {
		maxResponseBytes = 8 << 20
	}
	return &Upstream{
		base: base,
		client: &http.Client{
			Transport: transport,
			// A proxy hands redirects to its client instead of following them.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		maxResponseBytes: maxResponseBytes,
	}, nil
}

func parseBaseURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("invalid Bifrost URL %q: want http(s)://host[:port]", raw)
	}
	if u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", fmt.Errorf("invalid Bifrost URL %q: no query, fragment or credentials allowed", raw)
	}
	return strings.TrimRight(u.String(), "/"), nil
}

func (u *Upstream) target(escapedPath, rawQuery string) string {
	s := u.base + escapedPath
	if rawQuery != "" {
		s += "?" + rawQuery
	}
	return s
}

// Open sends a request and returns the live response; the caller closes it.
func (u *Upstream) Open(ctx context.Context, method, escapedPath, rawQuery string, header http.Header, body io.Reader, contentLength int64) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, u.target(escapedPath, rawQuery), body)
	if err != nil {
		return nil, err
	}
	req.Header = forwardHeaders(header)
	if body != nil && body != http.NoBody {
		req.ContentLength = contentLength
	}
	return u.client.Do(req)
}

// Send sends a buffered request and buffers the response.
func (u *Upstream) Send(ctx context.Context, req *Request) (*Response, error) {
	resp, err := u.Open(ctx, req.Method, req.Path, req.RawQuery, req.Header, bytes.NewReader(req.Body), int64(len(req.Body)))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, complete, err := readBounded(resp.Body, u.maxResponseBytes)
	if err != nil {
		return nil, fmt.Errorf("reading bifrost response: %w", err)
	}
	if !complete {
		return nil, fmt.Errorf("bifrost response larger than %d bytes", u.maxResponseBytes)
	}
	return &Response{StatusCode: resp.StatusCode, Header: resp.Header, Body: body}, nil
}

// readBounded reads up to limit bytes. complete is false when the body is
// longer; the bytes read so far (limit+1) are returned so the caller can
// still forward them followed by the rest of r.
func readBounded(r io.Reader, limit int64) (data []byte, complete bool, err error) {
	data, err = io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return data, false, err
	}
	return data, int64(len(data)) <= limit, nil
}

// hopHeaders are connection-scoped and never forwarded (RFC 9110 section 7.6.1).
var hopHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

func removeHopByHop(h http.Header) {
	for _, field := range h.Values("Connection") {
		for _, name := range strings.Split(field, ",") {
			if name = textproto.TrimString(name); name != "" {
				h.Del(name)
			}
		}
	}
	for _, name := range hopHeaders {
		h.Del(name)
	}
}

// forwardHeaders copies the client's headers for Bifrost (including its
// Authorization and x-bf-* headers), minus hop-by-hop ones.
func forwardHeaders(in http.Header) http.Header {
	h := in.Clone()
	if h == nil {
		h = http.Header{}
	}
	removeHopByHop(h)
	return h
}

func copyResponseHeaders(dst, src http.Header) {
	for name, values := range src {
		dst[name] = append([]string(nil), values...)
	}
	removeHopByHop(dst)
}

// inspectable returns the body decoded for reading (gzip is undone on a
// copy; the bytes forwarded to the client are never changed). nil means the
// body cannot be read, which makes any check unable_to_verify.
func inspectable(resp *Response, limit int64) []byte {
	switch strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding"))) {
	case "", "identity":
		return resp.Body
	case "gzip", "x-gzip":
		zr, err := gzip.NewReader(bytes.NewReader(resp.Body))
		if err != nil {
			return nil
		}
		defer zr.Close()
		out, complete, err := readBounded(zr, limit)
		if err != nil || !complete {
			return nil
		}
		return out
	default:
		return nil
	}
}

func isEventStream(h http.Header) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(h.Get("Content-Type"))), "text/event-stream")
}
