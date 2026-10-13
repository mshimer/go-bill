package mockh_test

import (
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"testing"

	mockh "comfy/go-bill/mock-h"
)

func TestStartPicksAFreePort(t *testing.T) {
	t.Parallel()
	s, c := startServer(t)

	if s.Port() <= 0 {
		t.Fatalf("Port() = %d, want a bound port", s.Port())
	}
	if want := "http://localhost:" + strconv.Itoa(s.Port()); s.BaseURL() != want {
		t.Errorf("BaseURL() = %q, want %q", s.BaseURL(), want)
	}

	s.MockResponse(mockh.Any(), mockh.OK("up"))
	wantBody(t, c.get("/"), "up")
}

func TestStartOnAnExplicitPortAndStopReleasesIt(t *testing.T) {
	// Borrow an ephemeral port, then prove we can bind that exact number.
	borrowed := mockh.MustStart(0)
	port := borrowed.Port()
	if err := borrowed.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s, err := mockh.Start(port)
	if err != nil {
		t.Fatalf("Start(%d): %v", port, err)
	}
	if s.Port() != port {
		t.Errorf("Port() = %d, want %d", s.Port(), port)
	}
	s.MockResponse(mockh.Any(), mockh.OK("bound"))
	wantBody(t, newClient(t, s).get("/"), "bound")
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// After Close the port is free again - rebinding proves resources were released.
	rebound, err := mockh.Start(port)
	if err != nil {
		t.Fatalf("rebind Start(%d): %v", port, err)
	}
	t.Cleanup(func() { _ = rebound.Close() })
	rebound.MockResponse(mockh.Any(), mockh.OK("rebound"))
	wantBody(t, newClient(t, rebound).get("/"), "rebound")
}

func TestStartOnABusyPortReportsAnError(t *testing.T) {
	t.Parallel()
	s, _ := startServer(t)

	if _, err := mockh.Start(s.Port()); err == nil {
		t.Fatal("Start on a busy port should fail")
	}
}

func TestURLBuildsAbsoluteURLs(t *testing.T) {
	t.Parallel()
	s, _ := startServer(t)

	cases := []struct{ in, want string }{
		{"/a/b", s.BaseURL() + "/a/b"},
		{"a/b", s.BaseURL() + "/a/b"},
		{"", s.BaseURL() + "/"},
	}
	for _, tc := range cases {
		t.Run("URL("+tc.in+")", func(t *testing.T) {
			if got := s.URL(tc.in); got != tc.want {
				t.Errorf("URL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestFirstRegisteredMockWins(t *testing.T) {
	t.Parallel()

	t.Run("specific before broad", func(t *testing.T) {
		t.Parallel()
		s, c := startServer(t)
		s.MockResponse(mockh.Get("/orders/1"), mockh.OK("specific"))
		s.MockResponse(mockh.PathPrefix("/orders"), mockh.OK("broad"))

		wantBody(t, c.get("/orders/1"), "specific")
		wantBody(t, c.get("/orders/2"), "broad")
	})

	t.Run("a catch-all registered first shadows later mocks", func(t *testing.T) {
		t.Parallel()
		s, c := startServer(t)
		s.MockResponse(mockh.Any(), mockh.OK("catch-all"))
		s.MockResponse(mockh.Get("/specific"), mockh.OK("never reached"))

		wantBody(t, c.get("/specific"), "catch-all")
	})
}

func TestMockCallCount(t *testing.T) {
	t.Parallel()
	s, c := startServer(t)
	health := s.MockResponse(mockh.Get("/health"), mockh.OK("UP"))
	other := s.MockResponse(mockh.Any(), mockh.OK("other"))

	if health.CallCount() != 0 {
		t.Errorf("CallCount() = %d, want 0", health.CallCount())
	}
	for range 3 {
		c.get("/health")
	}
	c.get("/other")

	if health.CallCount() != 3 {
		t.Errorf("health.CallCount() = %d, want 3", health.CallCount())
	}
	if other.CallCount() != 1 {
		t.Errorf("other.CallCount() = %d, want 1", other.CallCount())
	}
	if health.Matcher() == nil || health.Responder() == nil {
		t.Error("Matcher()/Responder() should expose what was registered")
	}
}

func TestRequestsAreRecorded(t *testing.T) {
	t.Parallel()
	s, c := startServer(t)
	s.MockResponse(mockh.Any(), mockh.OK())

	c.get("/a?x=1")
	c.post("/b", "payload", map[string]string{"X-Trace": "t-1"})

	requests := s.Requests()
	if len(requests) != 2 {
		t.Fatalf("Requests() has %d entries, want 2", len(requests))
	}

	first := requests[0]
	if first.Method != "GET" || first.Path != "/a" || first.Param("x") != "1" {
		t.Errorf("first request = %s, param x = %q", first, first.Param("x"))
	}
	if first.ReceivedAt.IsZero() {
		t.Error("ReceivedAt should be set")
	}

	second := requests[1]
	if second.Method != "POST" || second.BodyText() != "payload" || second.Header("X-Trace") != "t-1" {
		t.Errorf("second request = %s, body = %q, trace = %q",
			second, second.BodyText(), second.Header("X-Trace"))
	}
}

func TestRequestsReturnsACopy(t *testing.T) {
	t.Parallel()
	s, c := startServer(t)
	s.MockResponse(mockh.Any(), mockh.OK())
	c.get("/a")

	snapshot := s.Requests()
	snapshot[0] = nil
	c.get("/b")

	if again := s.Requests(); len(again) != 2 || again[0] == nil || again[0].Path != "/a" {
		t.Errorf("the recording was affected by mutating a snapshot: %v", again)
	}
}

func TestReceivedAndReceivedCount(t *testing.T) {
	t.Parallel()
	s, c := startServer(t)
	s.MockResponse(mockh.Any(), mockh.OK())

	c.get("/orders")
	c.post("/orders", `{"id":1}`)
	c.post("/orders", `{"id":2}`)

	cases := []struct {
		name    string
		matcher mockh.Matcher
		want    int
	}{
		{"posts", mockh.Post("/orders"), 2},
		{"gets", mockh.Get("/orders"), 1},
		{"by body", mockh.BodyContains(`"id":2`), 1},
		{"deletes", mockh.Delete("/orders"), 0},
		{"everything", mockh.Any(), 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := s.ReceivedCount(tc.matcher); got != tc.want {
				t.Errorf("ReceivedCount = %d, want %d", got, tc.want)
			}
		})
	}

	posts := s.Received(mockh.Post("/orders"))
	if len(posts) != 2 || posts[0].BodyText() != `{"id":1}` || posts[1].BodyText() != `{"id":2}` {
		t.Errorf("Received returned %v in the wrong order or shape", posts)
	}
}

func TestUnmatchedRequestsAreRecordedToo(t *testing.T) {
	t.Parallel()
	s, c := startServer(t)

	wantStatus(t, c.get("/nothing-mocked"), http.StatusNotFound)

	requests := s.Requests()
	if len(requests) != 1 || requests[0].Path != "/nothing-mocked" {
		t.Fatalf("Requests() = %v, want one entry for /nothing-mocked", requests)
	}
}

func TestClearRemovesMocksAndTheRecording(t *testing.T) {
	t.Parallel()
	s, c := startServer(t)
	s.MockResponse(mockh.Get("/a"), mockh.OK("a"))
	wantBody(t, c.get("/a"), "a")
	if len(s.Requests()) != 1 {
		t.Fatalf("Requests() has %d entries, want 1", len(s.Requests()))
	}

	s.Clear()

	if len(s.Requests()) != 0 {
		t.Errorf("Requests() has %d entries after Clear, want 0", len(s.Requests()))
	}
	wantStatus(t, c.get("/a"), http.StatusNotFound)

	// Still alive: new mocks work on the same port.
	s.MockResponse(mockh.Get("/a"), mockh.OK("a again"))
	wantBody(t, c.get("/a"), "a again")
}

func TestStopRefusesFurtherConnections(t *testing.T) {
	t.Parallel()
	s := mockh.MustStart(0)
	s.MockResponse(mockh.Any(), mockh.OK("up"))
	c := newClient(t, s)
	wantBody(t, c.get("/"), "up")

	if err := s.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if _, err := c.trySend(http.MethodGet, "/", "", nil); err == nil {
		t.Fatal("a request to a stopped server should fail")
	}
}

func TestStopIsIdempotent(t *testing.T) {
	t.Parallel()
	s := mockh.MustStart(0)

	for i := range 3 {
		if err := s.Stop(); err != nil {
			t.Fatalf("Stop #%d: %v", i+1, err)
		}
	}
	// Close is an alias, so it is idempotent with Stop.
	if err := s.Close(); err != nil {
		t.Fatalf("Close after Stop: %v", err)
	}
}

func TestServersAreIndependent(t *testing.T) {
	t.Parallel()
	a, ca := startServer(t)
	b, cb := startServer(t)

	a.MockResponse(mockh.Any(), mockh.OK("from A"))
	b.MockResponse(mockh.Any(), mockh.OK("from B"))

	if a.Port() == b.Port() {
		t.Fatalf("both servers bound port %d", a.Port())
	}
	wantBody(t, ca.get("/x"), "from A")
	wantBody(t, cb.get("/x"), "from B")
}

func TestConcurrentRequests(t *testing.T) {
	t.Parallel()
	s, c := startServer(t)

	const template = "/item/{id}"
	counter := s.Mock(mockh.PathTemplateMatching(template), func(r *mockh.Request) *mockh.Response {
		return mockh.OK(r.PathVariables(template)["id"])
	})

	const count = 50
	var (
		mu     sync.Mutex
		bodies = map[string]bool{}
		wg     sync.WaitGroup
	)
	for i := 1; i <= count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body := c.get(fmt.Sprintf("/item/%d", i)).body
			mu.Lock()
			bodies[body] = true
			mu.Unlock()
		}()
	}
	wg.Wait()

	if len(bodies) != count {
		t.Errorf("got %d distinct bodies, want %d", len(bodies), count)
	}
	for i := 1; i <= count; i++ {
		if !bodies[strconv.Itoa(i)] {
			t.Errorf("missing response body %d", i)
		}
	}
	if got := len(s.Requests()); got != count {
		t.Errorf("Requests() has %d entries, want %d", got, count)
	}
	if got := counter.CallCount(); got != count {
		t.Errorf("CallCount() = %d, want %d", got, count)
	}
}

func TestConcurrentRegistrationWhileServing(t *testing.T) {
	t.Parallel()
	s, c := startServer(t)
	s.MockResponse(mockh.Any(), mockh.OK("catch-all"))

	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.MockResponse(mockh.Get(fmt.Sprintf("/late/%d", i)), mockh.OK("late"))
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			wantStatus(t, c.get("/x"), http.StatusOK)
		}()
	}
	wg.Wait()

	if got := len(s.Requests()); got != 20 {
		t.Errorf("Requests() has %d entries, want 20", got)
	}
}

func TestDeferredCloseStopsTheServer(t *testing.T) {
	t.Parallel()
	s := mockh.MustStart(0)
	c := newClient(t, s)

	func() {
		defer func() {
			if err := s.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
		}()
		s.MockResponse(mockh.Any(), mockh.OK("inside"))
		wantBody(t, c.get("/"), "inside")
	}()

	if _, err := c.trySend(http.MethodGet, "/", "", nil); err == nil {
		t.Fatal("a request after Close should fail")
	}
}
