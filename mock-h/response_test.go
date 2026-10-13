package mockh_test

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	mockh "comfy/go-bill/mock-h"
)

func TestResponseConstructors(t *testing.T) {
	t.Parallel()

	t.Run("OK sends 200 with a text body", func(t *testing.T) {
		t.Parallel()
		s, c := startServer(t)
		s.MockResponse(mockh.Any(), mockh.OK("pong"))

		res := c.get("/ping")

		wantStatus(t, res, http.StatusOK)
		wantBody(t, res, "pong")
		wantContains(t, res.headers.Get("Content-Type"), "text/plain")
	})

	t.Run("OK with no argument is an empty 200", func(t *testing.T) {
		t.Parallel()
		s, c := startServer(t)
		s.MockResponse(mockh.Any(), mockh.OK())

		res := c.get("/ping")

		wantStatus(t, res, http.StatusOK)
		wantBody(t, res, "")
	})

	t.Run("JSON sets the content type", func(t *testing.T) {
		t.Parallel()
		s, c := startServer(t)
		s.MockResponse(mockh.Get("/user"), mockh.JSON(`{"id":1,"name":"Ada"}`))

		res := c.get("/user")

		wantBody(t, res, `{"id":1,"name":"Ada"}`)
		wantHeader(t, res, "Content-Type", "application/json")
	})

	t.Run("Text carries an explicit status", func(t *testing.T) {
		t.Parallel()
		s, c := startServer(t)
		s.MockResponse(mockh.Any(), mockh.Text(503, "down for maintenance"))

		res := c.get("/x")

		wantStatus(t, res, http.StatusServiceUnavailable)
		wantBody(t, res, "down for maintenance")
		wantContains(t, res.headers.Get("Content-Type"), "text/plain")
	})

	t.Run("Status is bare, with no headers of its own", func(t *testing.T) {
		t.Parallel()
		s, c := startServer(t)
		s.MockResponse(mockh.Get("/teapot"), mockh.Status(418).WithBody("short and stout"))
		s.MockResponse(mockh.Get("/boom"), mockh.Status(500).WithBody("kaboom"))
		s.MockResponse(mockh.Get("/gone"), mockh.Status(410))

		wantStatus(t, c.get("/teapot"), 418)
		wantBody(t, c.get("/teapot"), "short and stout")
		wantStatus(t, c.get("/boom"), 500)
		wantStatus(t, c.get("/gone"), 410)
		wantBody(t, c.get("/gone"), "")
	})

	t.Run("NotFound defaults its message", func(t *testing.T) {
		t.Parallel()
		s, c := startServer(t)
		s.MockResponse(mockh.Get("/a"), mockh.NotFound())
		s.MockResponse(mockh.Get("/b"), mockh.NotFound("no such order"))

		resA := c.get("/a")
		wantStatus(t, resA, http.StatusNotFound)
		wantBody(t, resA, "Not Found")
		wantBody(t, c.get("/b"), "no such order")
	})

	t.Run("Bytes sends raw bytes under an explicit content type", func(t *testing.T) {
		t.Parallel()
		s, c := startServer(t)
		payload := []byte{0x00, 0x01, 0x02, 0xFF, 0xFE}
		s.MockResponse(mockh.Get("/blob"), mockh.Bytes(200, "application/octet-stream", payload))

		res := c.get("/blob")

		wantHeader(t, res, "Content-Type", "application/octet-stream")
		if !bytes.Equal(res.raw, payload) {
			t.Errorf("body = %v, want %v", res.raw, payload)
		}
	})
}

func TestResponseBuilders(t *testing.T) {
	t.Parallel()

	t.Run("builders return copies and never mutate the original", func(t *testing.T) {
		t.Parallel()
		base := mockh.OK("base")
		derived := base.WithStatus(201).WithBody("derived").
			WithHeader("X-One", "1").
			WithContentType("application/json").
			WithDelay(5 * time.Millisecond)

		if base.Status != http.StatusOK || string(base.Body) != "base" {
			t.Errorf("base changed: %v %q", base.Status, base.Body)
		}
		if got := base.Headers.Get("X-One"); got != "" {
			t.Errorf("base gained header X-One = %q", got)
		}
		if base.Delay != 0 {
			t.Errorf("base.Delay = %v, want 0", base.Delay)
		}
		if derived.Status != 201 || string(derived.Body) != "derived" {
			t.Errorf("derived = %v %q", derived.Status, derived.Body)
		}
		if got := derived.Headers.Get("Content-Type"); got != "application/json" {
			t.Errorf("derived Content-Type = %q", got)
		}
		if derived.Delay != 5*time.Millisecond {
			t.Errorf("derived.Delay = %v", derived.Delay)
		}
	})

	t.Run("WithBytes replaces the body", func(t *testing.T) {
		t.Parallel()
		r := mockh.Status(200).WithBytes([]byte{1, 2, 3})
		if !bytes.Equal(r.Body, []byte{1, 2, 3}) {
			t.Errorf("Body = %v", r.Body)
		}
	})

	t.Run("String names the status and body size", func(t *testing.T) {
		t.Parallel()
		if got := mockh.OK("pong").String(); got != "Response(200, 4 bytes)" {
			t.Errorf("String() = %q", got)
		}
	})

	t.Run("custom headers, several at once", func(t *testing.T) {
		t.Parallel()
		s, c := startServer(t)
		s.MockResponse(mockh.Get("/page"), mockh.OK("body").
			WithHeader("X-Request-Id", "abc-123").
			WithHeader("Cache-Control", "no-store").
			WithHeader("X-Rate-Limit", "100"))

		res := c.get("/page")

		wantHeader(t, res, "X-Request-Id", "abc-123")
		wantHeader(t, res, "Cache-Control", "no-store")
		wantHeader(t, res, "X-Rate-Limit", "100")
	})

	t.Run("WithHeader replaces earlier values for the same name", func(t *testing.T) {
		t.Parallel()
		s, c := startServer(t)
		s.MockResponse(mockh.Any(), mockh.OK("x").
			WithHeader("X-Env", "staging").
			WithHeader("X-Env", "production"))

		values := c.get("/x").headers.Values("X-Env")
		if len(values) != 1 || values[0] != "production" {
			t.Errorf("X-Env = %v, want [production]", values)
		}
	})

	t.Run("a repeated header can carry several values", func(t *testing.T) {
		t.Parallel()
		s, c := startServer(t)
		s.MockResponse(mockh.Get("/cookies"), mockh.OK("ok").WithHeader("Set-Cookie", "a=1", "b=2"))

		values := c.get("/cookies").headers.Values("Set-Cookie")
		if len(values) != 2 || values[0] != "a=1" || values[1] != "b=2" {
			t.Errorf("Set-Cookie = %v, want [a=1 b=2]", values)
		}
	})
}

func TestBodylessResponses(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		method   string
		response *mockh.Response
		status   int
	}{
		{"204 drops a body that was set", http.MethodDelete, mockh.NoContent().WithBody("discarded"), http.StatusNoContent},
		{"304 drops a body that was set", http.MethodGet, mockh.Status(304).WithBody("discarded"), http.StatusNotModified},
		{"HEAD drops a body that was set", http.MethodHead, mockh.OK("discarded"), http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, c := startServer(t)
			s.MockResponse(mockh.Any(), tc.response)

			res := c.send(tc.method, "/item/1", "", nil)

			wantStatus(t, res, tc.status)
			wantBody(t, res, "")
		})
	}
}

func TestResponders(t *testing.T) {
	t.Parallel()

	t.Run("a responder can build a reply from the request", func(t *testing.T) {
		t.Parallel()
		s, c := startServer(t)
		s.Mock(mockh.Post("/echo"), func(r *mockh.Request) *mockh.Response {
			return mockh.JSON(fmt.Sprintf(
				`{"method":%q,"path":%q,"q":%q,"auth":%q,"body":%s}`,
				r.Method, r.Path, r.Param("q"), r.Header("Authorization"), r.BodyText()))
		})

		res := c.post("/echo?q=hi", `{"n":1}`, map[string]string{"Authorization": "Bearer t"})

		wantBody(t, res, `{"method":"POST","path":"/echo","q":"hi","auth":"Bearer t","body":{"n":1}}`)
	})

	t.Run("a mock can be stateful across calls", func(t *testing.T) {
		t.Parallel()
		s, c := startServer(t)
		var attempts atomic.Int32
		s.Mock(mockh.Get("/flaky"), func(*mockh.Request) *mockh.Response {
			if attempts.Add(1) < 3 {
				return mockh.Status(503).WithBody("try again")
			}
			return mockh.OK("finally")
		})

		wantStatus(t, c.get("/flaky"), http.StatusServiceUnavailable)
		wantStatus(t, c.get("/flaky"), http.StatusServiceUnavailable)
		wantBody(t, c.get("/flaky"), "finally")
		wantBody(t, c.get("/flaky"), "finally")
	})

	t.Run("a response can be delayed to exercise client timeouts", func(t *testing.T) {
		t.Parallel()
		s, c := startServer(t)
		s.MockResponse(mockh.Get("/slow"), mockh.OK("eventually").WithDelay(300*time.Millisecond))

		start := time.Now()
		res := c.get("/slow")
		elapsed := time.Since(start)

		wantBody(t, res, "eventually")
		if elapsed < 300*time.Millisecond {
			t.Errorf("elapsed = %v, want at least 300ms", elapsed)
		}
	})

	t.Run("a responder that panics yields 500 and leaves the server usable", func(t *testing.T) {
		t.Parallel()
		s, c := startServer(t)
		s.Mock(mockh.Get("/bad"), func(*mockh.Request) *mockh.Response { panic("responder exploded") })
		s.MockResponse(mockh.Get("/good"), mockh.OK("fine"))

		failed := c.get("/bad")

		wantStatus(t, failed, http.StatusInternalServerError)
		wantContains(t, failed.body, "responder exploded")
		wantContains(t, failed.body, "GET /bad")
		wantBody(t, c.get("/good"), "fine")
	})

	t.Run("a responder returning nil yields 500", func(t *testing.T) {
		t.Parallel()
		s, c := startServer(t)
		s.Mock(mockh.Any(), func(*mockh.Request) *mockh.Response { return nil })

		res := c.get("/x")

		wantStatus(t, res, http.StatusInternalServerError)
		wantContains(t, res.body, "no response")
	})
}

func TestUnmatchedRequests(t *testing.T) {
	t.Parallel()
	s, c := startServer(t)
	s.MockResponse(mockh.Get("/known"), mockh.OK("known"))

	res := c.get("/unknown")

	wantStatus(t, res, http.StatusNotFound)
	wantContains(t, res.body, "no mock matched")
	wantContains(t, res.body, "GET /unknown")
	// The 404 names how many mocks were in play, which is the usual clue.
	wantContains(t, res.body, "1 mock(s) registered")

	if strings.Contains(res.body, "panicked") {
		t.Errorf("unexpected panic text in %q", res.body)
	}
}
