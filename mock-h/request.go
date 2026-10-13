package mockh

import (
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Request is an HTTP request received by the mock server.
//
// Header lookups are case-insensitive (headers are stored in an http.Header,
// which canonicalises names); query parameter lookups are case-sensitive.
//
// A Request is produced by the server and is read-only from a matcher or
// responder's point of view - never mutate one.
type Request struct {
	// Method is the HTTP verb, upper-cased (GET, POST, ...).
	Method string

	// Path is the decoded request path, without the query string.
	Path string

	// RawQuery is the query string exactly as it arrived, without the leading
	// "?". It is empty when the request carried no query string.
	RawQuery string

	// Query holds the parsed query parameters. Repeated parameters keep all of
	// their values, in arrival order, and malformed percent escapes are passed
	// through undecoded rather than failing the request.
	Query url.Values

	// Headers are the request headers. Names are canonicalised, so lookups
	// through the helper methods (or http.Header's own methods) are
	// case-insensitive.
	Headers http.Header

	// Body is the raw request body. It is never nil, only possibly empty.
	Body []byte

	// ReceivedAt is the instant the server read the request.
	ReceivedAt time.Time
}

// BodyText returns the request body decoded as UTF-8 text.
func (r *Request) BodyText() string { return string(r.Body) }

// URL returns the path plus the raw query string, for example "/search?q=go".
func (r *Request) URL() string {
	if r.RawQuery == "" {
		return r.Path
	}
	return r.Path + "?" + r.RawQuery
}

// Header returns the first value of a header, or "" when it is absent.
// Use HasHeader to tell an absent header from an empty one.
func (r *Request) Header(name string) string { return r.Headers.Get(name) }

// HeaderValues returns every value of a repeated header, or nil when absent.
func (r *Request) HeaderValues(name string) []string { return r.Headers.Values(name) }

// HasHeader reports whether the header is present, whatever its value.
func (r *Request) HasHeader(name string) bool {
	return len(r.Headers.Values(name)) > 0
}

// Param returns the first value of a query parameter, or "" when it is absent.
// Use HasParam to tell an absent parameter from an empty one, as in "?debug".
func (r *Request) Param(name string) string {
	if vs := r.Query[name]; len(vs) > 0 {
		return vs[0]
	}
	return ""
}

// ParamValues returns every value of a repeated query parameter, for example
// all three values of "?tag=a&tag=b&tag=c".
func (r *Request) ParamValues(name string) []string { return r.Query[name] }

// HasParam reports whether the query parameter is present, whatever its value.
func (r *Request) HasParam(name string) bool {
	_, ok := r.Query[name]
	return ok
}

// PathVariables extracts path variables against a template such as
// "/users/{id}/orders/{orderId}". It returns an empty map when the path does
// not fit the template.
func (r *Request) PathVariables(template string) map[string]string {
	return NewPathTemplate(template).Extract(r.Path)
}

// String renders the request as "METHOD /path?query", which is what the
// server's 404 and 500 diagnostics quote.
func (r *Request) String() string { return r.Method + " " + r.URL() }

// newRequest snapshots an incoming *http.Request into an immutable Request.
// body has already been drained by the caller.
func newRequest(r *http.Request, body []byte) *Request {
	headers := make(http.Header, len(r.Header)+1)
	for name, values := range r.Header {
		headers[name] = append([]string(nil), values...)
	}
	// net/http strips Host out of the header map; put it back so matchers can
	// see every header the client actually sent.
	if r.Host != "" && len(headers.Values("Host")) == 0 {
		headers.Set("Host", r.Host)
	}
	if body == nil {
		body = []byte{}
	}
	return &Request{
		Method:     strings.ToUpper(r.Method),
		Path:       r.URL.Path,
		RawQuery:   r.URL.RawQuery,
		Query:      parseQuery(r.URL.RawQuery),
		Headers:    headers,
		Body:       body,
		ReceivedAt: time.Now(),
	}
}

// parseQuery splits a raw query string into parameters. Unlike url.ParseQuery
// it never fails: a value with a malformed percent escape is kept verbatim, so
// a bad escape cannot stop a request from being matched.
func parseQuery(raw string) url.Values {
	out := url.Values{}
	if raw == "" {
		return out
	}
	for _, pair := range strings.Split(raw, "&") {
		if pair == "" {
			continue
		}
		name, value := pair, ""
		if i := strings.IndexByte(pair, '='); i >= 0 {
			name, value = pair[:i], pair[i+1:]
		}
		key := decodeQueryComponent(name)
		out[key] = append(out[key], decodeQueryComponent(value))
	}
	return out
}

// decodeQueryComponent percent-decodes one query component, passing malformed
// input through unchanged.
func decodeQueryComponent(s string) string {
	if decoded, err := url.QueryUnescape(s); err == nil {
		return decoded
	}
	return s
}
