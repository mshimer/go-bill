package mockh

import (
	"net/http"
	"strconv"
	"time"
)

// Response is an HTTP response for a mock to send back.
//
// Values are treated as immutable: every builder method returns a modified
// copy, so a base response can be shared between mocks and specialised by
// chaining.
//
//	mockh.JSON(`{"ok":true}`).WithStatus(201).WithHeader("X-Id", "abc")
type Response struct {
	// Status is the HTTP status code to send.
	Status int

	// Headers are the response headers. Nil means "no headers of our own".
	Headers http.Header

	// Body is the response payload. It is suppressed for 204, 304 and HEAD,
	// whatever is set here.
	Body []byte

	// Delay holds the response open for this long before writing it, which is
	// how client timeout handling gets exercised.
	Delay time.Duration
}

// clone returns a copy of the response, with the header map copied too so the
// original is never aliased.
func (r *Response) clone() *Response {
	out := &Response{Status: r.Status, Body: r.Body, Delay: r.Delay}
	if r.Headers != nil {
		out.Headers = make(http.Header, len(r.Headers))
		for name, values := range r.Headers {
			out.Headers[name] = append([]string(nil), values...)
		}
	}
	return out
}

// WithStatus returns a copy of the response with a different status code.
func (r *Response) WithStatus(code int) *Response {
	out := r.clone()
	out.Status = code
	return out
}

// WithHeader returns a copy of the response with a header set, replacing any
// existing values for that name. Passing several values produces a repeated
// header, as in WithHeader("Set-Cookie", "a=1", "b=2").
func (r *Response) WithHeader(name string, values ...string) *Response {
	out := r.clone()
	if out.Headers == nil {
		out.Headers = http.Header{}
	}
	out.Headers.Del(name)
	for _, v := range values {
		out.Headers.Add(name, v)
	}
	return out
}

// WithContentType returns a copy of the response with the Content-Type header
// set.
func (r *Response) WithContentType(value string) *Response {
	return r.WithHeader("Content-Type", value)
}

// WithBody returns a copy of the response whose body is the UTF-8 encoding of
// text.
func (r *Response) WithBody(text string) *Response {
	out := r.clone()
	out.Body = []byte(text)
	return out
}

// WithBytes returns a copy of the response whose body is the given bytes.
func (r *Response) WithBytes(body []byte) *Response {
	out := r.clone()
	out.Body = body
	return out
}

// WithDelay returns a copy of the response that is written only after the
// given delay.
func (r *Response) WithDelay(d time.Duration) *Response {
	out := r.clone()
	out.Delay = d
	return out
}

// String renders the response as "Response(200, 4 bytes)", which keeps test
// failure output readable.
func (r *Response) String() string {
	return "Response(" + strconv.Itoa(r.Status) + ", " + strconv.Itoa(len(r.Body)) + " bytes)"
}

const textContentType = "text/plain; charset=utf-8"

// OK returns a 200 response carrying text as a plain-text body. Call it with
// no argument for an empty 200.
func OK(text ...string) *Response {
	body := ""
	if len(text) > 0 {
		body = text[0]
	}
	return (&Response{Status: http.StatusOK, Body: []byte(body)}).WithContentType(textContentType)
}

// JSON returns a 200 response with body sent as application/json.
func JSON(body string) *Response {
	return (&Response{Status: http.StatusOK, Body: []byte(body)}).WithContentType("application/json")
}

// Text returns a plain-text response with an explicit status code.
func Text(status int, body string) *Response {
	return (&Response{Status: status, Body: []byte(body)}).WithContentType(textContentType)
}

// Status returns an empty response with the given status code and no headers.
func Status(code int) *Response {
	return &Response{Status: code}
}

// NoContent returns a 204 response. Any body later set on it is dropped when
// the response is written.
func NoContent() *Response {
	return &Response{Status: http.StatusNoContent}
}

// NotFound returns a 404 plain-text response. Call it with no argument for the
// default "Not Found" message.
func NotFound(message ...string) *Response {
	msg := "Not Found"
	if len(message) > 0 {
		msg = message[0]
	}
	return Text(http.StatusNotFound, msg)
}

// Bytes returns a response carrying raw bytes under an explicit content type,
// for binary payloads.
func Bytes(status int, contentType string, body []byte) *Response {
	return (&Response{Status: status, Body: body}).WithContentType(contentType)
}
