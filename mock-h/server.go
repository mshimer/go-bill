// Package mockh is a mock HTTP server for tests: register matchers against
// responses, point the code under test at a real localhost socket, then assert
// on what it sent.
//
// Mocks are consulted in registration order and the first match wins, so
// specific mocks go before broad ones and a catch-all goes last.
//
//	server := mockh.MustStart(0)
//	defer server.Close()
//
//	orders := server.MockResponse(mockh.Get("/orders/1"), mockh.JSON(`{"id":1}`))
//	server.MockResponse(mockh.Any(), mockh.NotFound())
//
//	// ... exercise the client against server.BaseURL() ...
//
//	if orders.CallCount() != 1 {
//		t.Fatalf("expected one call, got %d", orders.CallCount())
//	}
//
// Everything here is safe for concurrent use: mocks may be registered while
// requests are in flight, and call counts and the recording are
// goroutine-safe.
package mockh

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Mock is a registered matcher/responder pair. It keeps a live count of how
// many requests it has answered, which is what verification assertions use.
type Mock struct {
	matcher   Matcher
	responder Responder
	calls     atomic.Int64
}

// Matcher returns the matcher this mock was registered with.
func (m *Mock) Matcher() Matcher { return m.matcher }

// Responder returns the responder this mock was registered with.
func (m *Mock) Responder() Responder { return m.responder }

// CallCount returns how many requests this mock has answered since it was
// registered.
func (m *Mock) CallCount() int64 { return m.calls.Load() }

// MockServer is a running mock HTTP server bound to 127.0.0.1.
//
// Create one with Start or MustStart and shut it down with Close, which makes
// it usable with defer. Mocks are consulted in registration order and the
// first match wins.
type MockServer struct {
	listener net.Listener
	srv      *http.Server
	port     int

	mu       sync.RWMutex
	mocks    []*Mock
	recorded []*Request

	stopOnce sync.Once
	stopErr  error
	done     chan struct{}
}

// Start boots a mock server on the given port of 127.0.0.1. Pass 0 for an
// ephemeral port and read the one actually assigned from Port.
func Start(port int) (*MockServer, error) {
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return nil, fmt.Errorf("mockh: cannot listen on port %d: %w", port, err)
	}
	s := &MockServer{
		listener: ln,
		port:     ln.Addr().(*net.TCPAddr).Port,
		done:     make(chan struct{}),
	}
	s.srv = &http.Server{Handler: http.HandlerFunc(s.handle)}
	go func() {
		defer close(s.done)
		// Serve always ends in an error; a closed server is the normal one.
		_ = s.srv.Serve(ln)
	}()
	return s, nil
}

// MustStart is Start for test code: it panics instead of returning an error,
// so a server can be created in a single expression.
func MustStart(port int) *MockServer {
	s, err := Start(port)
	if err != nil {
		panic(err)
	}
	return s
}

// Port returns the port actually bound, which is the interesting one when the
// server was started on port 0.
func (s *MockServer) Port() int { return s.port }

// BaseURL returns the server's root URL, for example "http://localhost:54321".
func (s *MockServer) BaseURL() string {
	return "http://localhost:" + strconv.Itoa(s.port)
}

// URL returns an absolute URL for a path on this server. A leading slash is
// added when missing.
func (s *MockServer) URL(path string) string {
	if len(path) > 0 && path[0] == '/' {
		return s.BaseURL() + path
	}
	return s.BaseURL() + "/" + path
}

// Mock registers a mock: matcher decides whether it applies and responder
// builds the reply. It returns a handle whose CallCount can be asserted on.
func (s *MockServer) Mock(matcher Matcher, responder Responder) *Mock {
	m := &Mock{matcher: matcher, responder: responder}
	s.mu.Lock()
	s.mocks = append(s.mocks, m)
	s.mu.Unlock()
	return m
}

// MockResponse registers a mock that always returns the same response.
func (s *MockServer) MockResponse(matcher Matcher, response *Response) *Mock {
	return s.Mock(matcher, func(*Request) *Response { return response })
}

// Requests returns a copy of every request the server has received since it
// started or was last cleared, in arrival order. Unmatched requests are
// recorded too.
func (s *MockServer) Requests() []*Request {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]*Request(nil), s.recorded...)
}

// Clear removes every mock and every recorded request - a clean slate with
// nothing mocked. The server keeps running on the same port.
func (s *MockServer) Clear() {
	s.mu.Lock()
	s.mocks = nil
	s.recorded = nil
	s.mu.Unlock()
}

// Received returns the recorded requests satisfying the matcher, so any
// matcher doubles as a verification query.
func (s *MockServer) Received(matcher Matcher) []*Request {
	var out []*Request
	for _, r := range s.Requests() {
		if matcher(r) {
			out = append(out, r)
		}
	}
	return out
}

// ReceivedCount returns how many recorded requests satisfy the matcher.
func (s *MockServer) ReceivedCount(matcher Matcher) int {
	return len(s.Received(matcher))
}

// Stop shuts the server down and releases its port and goroutines. It is safe
// to call more than once; later calls do nothing and return the first result.
func (s *MockServer) Stop() error {
	s.stopOnce.Do(func() {
		err := s.srv.Close()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		<-s.done
		s.stopErr = err
	})
	return s.stopErr
}

// Close shuts the server down. It is an alias for Stop that satisfies
// io.Closer, which is what makes `defer server.Close()` work.
func (s *MockServer) Close() error { return s.Stop() }

// handle is the single HTTP handler: record the request, resolve it against
// the mocks, write the result. A panic anywhere becomes a 500 rather than a
// dropped connection.
func (s *MockServer) handle(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if p := recover(); p != nil {
			write(w, r, Text(http.StatusInternalServerError,
				fmt.Sprintf("mockh failed to handle the request: %v", p)))
		}
	}()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		write(w, r, Text(http.StatusInternalServerError,
			fmt.Sprintf("mockh could not read the request body: %v", err)))
		return
	}

	request := newRequest(r, body)
	s.mu.Lock()
	s.recorded = append(s.recorded, request)
	mocks := append([]*Mock(nil), s.mocks...)
	s.mu.Unlock()

	write(w, r, resolve(request, mocks))
}

// resolve walks the mocks in registration order and returns the first match's
// response, a 500 describing a panic, or a 404 when nothing matched.
func resolve(request *Request, mocks []*Mock) *Response {
	for _, m := range mocks {
		matched, panicked := safeMatch(m.matcher, request)
		if panicked != nil {
			return Text(http.StatusInternalServerError,
				fmt.Sprintf("mockh matcher panicked for %s: %v", request, panicked))
		}
		if !matched {
			continue
		}
		m.calls.Add(1)
		response, panicked := safeRespond(m.responder, request)
		if panicked != nil {
			return Text(http.StatusInternalServerError,
				fmt.Sprintf("mockh responder panicked for %s: %v", request, panicked))
		}
		if response == nil {
			return Text(http.StatusInternalServerError,
				fmt.Sprintf("mockh responder returned no response for %s", request))
		}
		return response
	}
	return Text(http.StatusNotFound,
		fmt.Sprintf("mockh: no mock matched %s (%d mock(s) registered)", request, len(mocks)))
}

// safeMatch runs a matcher, converting a panic into a value the caller can
// turn into a 500.
func safeMatch(m Matcher, r *Request) (matched bool, panicked any) {
	defer func() { panicked = recover() }()
	return m(r), nil
}

// safeRespond runs a responder, converting a panic into a value the caller can
// turn into a 500.
func safeRespond(responder Responder, r *Request) (response *Response, panicked any) {
	defer func() { panicked = recover() }()
	return responder(r), nil
}

// write sends the response, honouring its delay and the rules that strip the
// body from 204, 304 and HEAD replies.
func write(w http.ResponseWriter, r *http.Request, response *Response) {
	if response.Delay > 0 {
		time.Sleep(response.Delay)
	}

	header := w.Header()
	for name, values := range response.Headers {
		header[name] = append([]string(nil), values...)
	}

	status := response.Status
	if status == 0 {
		status = http.StatusOK
	}
	bodyAllowed := status != http.StatusNoContent &&
		status != http.StatusNotModified &&
		r.Method != http.MethodHead

	w.WriteHeader(status)
	if bodyAllowed && len(response.Body) > 0 {
		_, _ = w.Write(response.Body)
	}
}
