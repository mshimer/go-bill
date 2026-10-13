package mockh_test

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	mockh "comfy/go-bill/mock-h"
)

// testClient is a minimal real HTTP client, so every case exercises the server
// over an actual socket instead of calling into the handler directly.
type testClient struct {
	t       *testing.T
	baseURL string
	client  *http.Client
}

// newClient returns a client pointed at the server, with a timeout short
// enough that a hung request fails the test rather than the suite.
func newClient(t *testing.T, s *mockh.MockServer) *testClient {
	t.Helper()
	return &testClient{
		t:       t,
		baseURL: s.BaseURL(),
		// A dedicated transport without keep-alives: no connection is pooled
		// across tests, so a closed server really does refuse the next request.
		client: &http.Client{
			Timeout:   20 * time.Second,
			Transport: &http.Transport{DisableKeepAlives: true},
		},
	}
}

// result is a fully-read HTTP response, which keeps the assertions terse.
type result struct {
	status  int
	body    string
	raw     []byte
	headers http.Header
}

// send performs a request and reads the whole response. body == "" means no
// body at all.
func (c *testClient) send(method, path, body string, headers map[string]string) *result {
	c.t.Helper()
	res, err := c.trySend(method, path, body, headers)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	return res
}

// trySend performs a request and returns the transport error, for cases that
// expect the connection to fail.
func (c *testClient) trySend(method, path, body string, headers map[string]string) (*result, error) {
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, c.baseURL+path, reader)
	if err != nil {
		return nil, err
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	res, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	return &result{status: res.StatusCode, body: string(raw), raw: raw, headers: res.Header}, nil
}

func (c *testClient) get(path string, headers ...map[string]string) *result {
	c.t.Helper()
	return c.send(http.MethodGet, path, "", first(headers))
}

func (c *testClient) post(path, body string, headers ...map[string]string) *result {
	c.t.Helper()
	return c.send(http.MethodPost, path, body, first(headers))
}

func (c *testClient) put(path, body string, headers ...map[string]string) *result {
	c.t.Helper()
	return c.send(http.MethodPut, path, body, first(headers))
}

func (c *testClient) patch(path, body string, headers ...map[string]string) *result {
	c.t.Helper()
	return c.send(http.MethodPatch, path, body, first(headers))
}

func (c *testClient) delete(path string, headers ...map[string]string) *result {
	c.t.Helper()
	return c.send(http.MethodDelete, path, "", first(headers))
}

func (c *testClient) head(path string, headers ...map[string]string) *result {
	c.t.Helper()
	return c.send(http.MethodHead, path, "", first(headers))
}

func (c *testClient) options(path string, headers ...map[string]string) *result {
	c.t.Helper()
	return c.send(http.MethodOptions, path, "", first(headers))
}

func first(headers []map[string]string) map[string]string {
	if len(headers) > 0 {
		return headers[0]
	}
	return nil
}

// startServer boots an ephemeral-port server and registers its shutdown.
func startServer(t *testing.T) (*mockh.MockServer, *testClient) {
	t.Helper()
	s, err := mockh.Start(0)
	if err != nil {
		t.Fatalf("Start(0): %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return s, newClient(t, s)
}

func wantStatus(t *testing.T, got *result, status int) {
	t.Helper()
	if got.status != status {
		t.Errorf("status = %d, want %d (body %q)", got.status, status, got.body)
	}
}

func wantBody(t *testing.T, got *result, body string) {
	t.Helper()
	if got.body != body {
		t.Errorf("body = %q, want %q", got.body, body)
	}
}

func wantContains(t *testing.T, got, substring string) {
	t.Helper()
	if !strings.Contains(got, substring) {
		t.Errorf("%q does not contain %q", got, substring)
	}
}

func wantHeader(t *testing.T, got *result, name, value string) {
	t.Helper()
	if v := got.headers.Get(name); v != value {
		t.Errorf("header %s = %q, want %q", name, v, value)
	}
}
