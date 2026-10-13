package mockh_test

import (
	"net/http"
	"regexp"
	"strings"
	"testing"

	mockh "comfy/go-bill/mock-h"
)

func TestMethodMatching(t *testing.T) {
	t.Parallel()

	t.Run("matches on verb alone", func(t *testing.T) {
		t.Parallel()
		s, c := startServer(t)
		s.MockResponse(mockh.Method("POST"), mockh.OK("posted"))

		wantBody(t, c.post("/anything", ""), "posted")
		wantStatus(t, c.get("/anything"), http.StatusNotFound)
	})

	t.Run("verb matching is case-insensitive", func(t *testing.T) {
		t.Parallel()
		s, c := startServer(t)
		s.MockResponse(mockh.Method("post"), mockh.OK("posted"))

		wantBody(t, c.post("/x", ""), "posted")
	})

	t.Run("every verb has a path shorthand", func(t *testing.T) {
		t.Parallel()
		s, c := startServer(t)
		s.MockResponse(mockh.Get("/r"), mockh.OK("GET"))
		s.MockResponse(mockh.Post("/r"), mockh.OK("POST"))
		s.MockResponse(mockh.Put("/r"), mockh.OK("PUT"))
		s.MockResponse(mockh.Patch("/r"), mockh.OK("PATCH"))
		s.MockResponse(mockh.Delete("/r"), mockh.OK("DELETE"))
		s.MockResponse(mockh.Options("/r"), mockh.OK("OPTIONS"))

		wantBody(t, c.get("/r"), "GET")
		wantBody(t, c.post("/r", ""), "POST")
		wantBody(t, c.put("/r", ""), "PUT")
		wantBody(t, c.patch("/r", ""), "PATCH")
		wantBody(t, c.delete("/r"), "DELETE")
		wantBody(t, c.options("/r"), "OPTIONS")
	})

	t.Run("HEAD matches and returns headers without a body", func(t *testing.T) {
		t.Parallel()
		s, c := startServer(t)
		s.MockResponse(mockh.Head("/r"), mockh.OK("ignored").WithHeader("X-Total", "42"))

		res := c.head("/r")

		wantStatus(t, res, http.StatusOK)
		wantHeader(t, res, "X-Total", "42")
		wantBody(t, res, "")
	})
}

func TestPathMatching(t *testing.T) {
	t.Parallel()

	t.Run("exact path", func(t *testing.T) {
		t.Parallel()
		s, c := startServer(t)
		s.MockResponse(mockh.Path("/orders"), mockh.OK("orders"))

		wantBody(t, c.get("/orders"), "orders")
		wantStatus(t, c.get("/orders/1"), http.StatusNotFound)
	})

	t.Run("path prefix", func(t *testing.T) {
		t.Parallel()
		s, c := startServer(t)
		s.MockResponse(mockh.PathPrefix("/api/v2/"), mockh.OK("v2"))

		wantBody(t, c.get("/api/v2/orders"), "v2")
		wantBody(t, c.get("/api/v2/customers/7"), "v2")
		wantStatus(t, c.get("/api/v1/orders"), http.StatusNotFound)
	})

	t.Run("path regexp must match in full", func(t *testing.T) {
		t.Parallel()
		s, c := startServer(t)
		s.MockResponse(mockh.PathMatching(regexp.MustCompile(`/orders/\d+`)), mockh.OK("an order"))

		wantBody(t, c.get("/orders/123"), "an order")
		wantStatus(t, c.get("/orders/abc"), http.StatusNotFound)
		wantStatus(t, c.get("/v1/orders/123"), http.StatusNotFound)
	})

	t.Run("query string is not part of the path", func(t *testing.T) {
		t.Parallel()
		s, c := startServer(t)
		s.MockResponse(mockh.Path("/search"), mockh.OK("searched"))

		wantBody(t, c.get("/search?q=go&page=2"), "searched")
	})
}

func TestPathTemplate(t *testing.T) {
	t.Parallel()

	const template = "/users/{userId}/orders/{orderId}"

	t.Run("captures variables over the wire", func(t *testing.T) {
		t.Parallel()
		s, c := startServer(t)
		s.Mock(mockh.PathTemplateMatching(template), func(r *mockh.Request) *mockh.Response {
			vars := r.PathVariables(template)
			return mockh.OK("user=" + vars["userId"] + " order=" + vars["orderId"])
		})

		wantBody(t, c.get("/users/7/orders/99"), "user=7 order=99")
		// A placeholder spans exactly one segment.
		wantStatus(t, c.get("/users/7/orders"), http.StatusNotFound)
		wantStatus(t, c.get("/users/7/orders/99/items"), http.StatusNotFound)
	})

	t.Run("compiled template", func(t *testing.T) {
		t.Parallel()
		tpl := mockh.NewPathTemplate(template)

		if tpl.Template() != template {
			t.Errorf("Template() = %q, want %q", tpl.Template(), template)
		}
		if names := strings.Join(tpl.Names(), ","); names != "userId,orderId" {
			t.Errorf("Names() = %q, want %q", names, "userId,orderId")
		}

		cases := []struct {
			path  string
			match bool
			vars  map[string]string
		}{
			{"/users/7/orders/99", true, map[string]string{"userId": "7", "orderId": "99"}},
			{"/users/ada/orders/x-1", true, map[string]string{"userId": "ada", "orderId": "x-1"}},
			{"/users/7/orders", false, map[string]string{}},
			{"/users//orders/99", false, map[string]string{}},
			{"/users/7/8/orders/99", false, map[string]string{}},
			{"/other", false, map[string]string{}},
		}
		for _, tc := range cases {
			t.Run(tc.path, func(t *testing.T) {
				if got := tpl.Matches(tc.path); got != tc.match {
					t.Errorf("Matches(%q) = %v, want %v", tc.path, got, tc.match)
				}
				got := tpl.Extract(tc.path)
				if len(got) != len(tc.vars) {
					t.Fatalf("Extract(%q) = %v, want %v", tc.path, got, tc.vars)
				}
				for name, want := range tc.vars {
					if got[name] != want {
						t.Errorf("Extract(%q)[%q] = %q, want %q", tc.path, name, got[name], want)
					}
				}
			})
		}
	})

	t.Run("template without placeholders is a literal", func(t *testing.T) {
		t.Parallel()
		tpl := mockh.NewPathTemplate("/health")

		if !tpl.Matches("/health") {
			t.Error("want /health to match")
		}
		if tpl.Matches("/healthz") {
			t.Error("want /healthz not to match")
		}
		if len(tpl.Names()) != 0 {
			t.Errorf("Names() = %v, want none", tpl.Names())
		}
	})
}

func TestQueryMatching(t *testing.T) {
	t.Parallel()

	t.Run("name and value", func(t *testing.T) {
		t.Parallel()
		s, c := startServer(t)
		s.MockResponse(mockh.And(mockh.Get("/search"), mockh.Query("q", "go")), mockh.OK("hit"))

		wantBody(t, c.get("/search?q=go"), "hit")
		wantStatus(t, c.get("/search?q=java"), http.StatusNotFound)
		wantStatus(t, c.get("/search"), http.StatusNotFound)
	})

	t.Run("presence only", func(t *testing.T) {
		t.Parallel()
		s, c := startServer(t)
		s.MockResponse(mockh.HasQuery("debug"), mockh.OK("debugging"))

		wantBody(t, c.get("/x?debug=true"), "debugging")
		wantBody(t, c.get("/x?debug"), "debugging")
		wantStatus(t, c.get("/x"), http.StatusNotFound)
	})

	t.Run("value regexp must match in full", func(t *testing.T) {
		t.Parallel()
		s, c := startServer(t)
		s.MockResponse(mockh.QueryMatching("page", regexp.MustCompile(`\d+`)), mockh.OK("paged"))

		wantBody(t, c.get("/x?page=12"), "paged")
		wantStatus(t, c.get("/x?page=last"), http.StatusNotFound)
		wantStatus(t, c.get("/x?page=12a"), http.StatusNotFound)
	})

	t.Run("repeated parameters are all visible", func(t *testing.T) {
		t.Parallel()
		s, c := startServer(t)
		s.Mock(mockh.HasQuery("tag"), func(r *mockh.Request) *mockh.Response {
			return mockh.OK(strings.Join(r.ParamValues("tag"), ","))
		})

		wantBody(t, c.get("/x?tag=a&tag=b&tag=c"), "a,b,c")
	})

	t.Run("values are url-decoded", func(t *testing.T) {
		t.Parallel()
		s, c := startServer(t)
		s.Mock(mockh.HasQuery("q"), func(r *mockh.Request) *mockh.Response {
			return mockh.OK(r.Param("q"))
		})

		wantBody(t, c.get("/x?q=hello%20world"), "hello world")
		wantBody(t, c.get("/x?q=a%26b%3Dc"), "a&b=c")
	})

	t.Run("malformed escapes pass through instead of failing", func(t *testing.T) {
		t.Parallel()
		s, c := startServer(t)
		s.Mock(mockh.HasQuery("q"), func(r *mockh.Request) *mockh.Response {
			return mockh.OK(r.Param("q"))
		})

		wantBody(t, c.get("/x?q=100%25"), "100%")
		wantBody(t, c.get("/x?q=broken%zz"), "broken%zz")
	})

	t.Run("matching on a decoded value", func(t *testing.T) {
		t.Parallel()
		s, c := startServer(t)
		s.MockResponse(mockh.Query("city", "New York"), mockh.OK("nyc"))

		wantBody(t, c.get("/x?city=New%20York"), "nyc")
	})

	t.Run("raw query and url are preserved", func(t *testing.T) {
		t.Parallel()
		s, c := startServer(t)
		s.Mock(mockh.Any(), func(r *mockh.Request) *mockh.Response {
			return mockh.OK(r.RawQuery + "|" + r.URL() + "|" + r.String())
		})

		wantBody(t, c.get("/x?a=1&a=2"), "a=1&a=2|/x?a=1&a=2|GET /x?a=1&a=2")
		wantBody(t, c.get("/x"), "|/x|GET /x")
	})
}

func TestHeaderMatching(t *testing.T) {
	t.Parallel()

	t.Run("name and value", func(t *testing.T) {
		t.Parallel()
		s, c := startServer(t)
		s.MockResponse(mockh.Header("X-Tenant", "acme"), mockh.OK("acme"))

		wantBody(t, c.get("/x", map[string]string{"X-Tenant": "acme"}), "acme")
		wantStatus(t, c.get("/x", map[string]string{"X-Tenant": "globex"}), http.StatusNotFound)
		wantStatus(t, c.get("/x"), http.StatusNotFound)
	})

	t.Run("header names are case-insensitive", func(t *testing.T) {
		t.Parallel()
		s, c := startServer(t)
		s.MockResponse(mockh.Header("x-tenant", "acme"), mockh.OK("acme"))

		wantBody(t, c.get("/x", map[string]string{"X-TENANT": "acme"}), "acme")
	})

	t.Run("header values are case-insensitive", func(t *testing.T) {
		t.Parallel()
		s, c := startServer(t)
		s.MockResponse(mockh.Header("X-Tenant", "ACME"), mockh.OK("acme"))

		wantBody(t, c.get("/x", map[string]string{"X-Tenant": "acme"}), "acme")
	})

	t.Run("presence only", func(t *testing.T) {
		t.Parallel()
		s, c := startServer(t)
		s.MockResponse(mockh.HasHeader("Authorization"), mockh.OK("authed"))

		wantBody(t, c.get("/x", map[string]string{"Authorization": "Bearer t"}), "authed")
		wantStatus(t, c.get("/x"), http.StatusNotFound)
	})

	t.Run("substring match, such as a bearer token prefix", func(t *testing.T) {
		t.Parallel()
		s, c := startServer(t)
		s.MockResponse(mockh.HeaderContaining("Authorization", "Bearer "), mockh.OK("bearer"))

		wantBody(t, c.get("/x", map[string]string{"Authorization": "Bearer abc123"}), "bearer")
		wantStatus(t, c.get("/x", map[string]string{"Authorization": "Basic abc123"}), http.StatusNotFound)
	})

	t.Run("content negotiation on Accept", func(t *testing.T) {
		t.Parallel()
		s, c := startServer(t)
		s.MockResponse(mockh.HeaderContaining("Accept", "application/json"), mockh.JSON(`{"ok":true}`))
		s.MockResponse(mockh.HeaderContaining("Accept", "text/plain"), mockh.OK("ok"))

		wantBody(t, c.get("/x", map[string]string{"Accept": "application/json"}), `{"ok":true}`)
		wantBody(t, c.get("/x", map[string]string{"Accept": "text/plain"}), "ok")
	})

	t.Run("request header accessors", func(t *testing.T) {
		t.Parallel()
		s, c := startServer(t)
		s.Mock(mockh.Any(), func(r *mockh.Request) *mockh.Response {
			return mockh.OK(strings.Join([]string{
				r.Header("x-trace"),
				strings.Join(r.HeaderValues("X-Trace"), ","),
				boolText(r.HasHeader("X-Trace")),
				boolText(r.HasHeader("X-Missing")),
				r.Header("X-Missing"),
			}, "|"))
		})

		wantBody(t, c.get("/x", map[string]string{"X-Trace": "t-1"}), "t-1|t-1|true|false|")
	})
}

func boolText(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func TestBodyMatching(t *testing.T) {
	t.Parallel()

	t.Run("exact body", func(t *testing.T) {
		t.Parallel()
		s, c := startServer(t)
		s.MockResponse(mockh.Body(`{"id":1}`), mockh.OK("exact"))

		wantBody(t, c.post("/x", `{"id":1}`), "exact")
		wantStatus(t, c.post("/x", `{"id":2}`), http.StatusNotFound)
	})

	t.Run("body contains a substring", func(t *testing.T) {
		t.Parallel()
		s, c := startServer(t)
		s.MockResponse(mockh.BodyContains(`"status":"paid"`), mockh.OK("paid"))

		wantBody(t, c.post("/x", `{"id":1,"status":"paid"}`), "paid")
		wantStatus(t, c.post("/x", `{"id":1,"status":"open"}`), http.StatusNotFound)
	})

	t.Run("body regexp matches anywhere", func(t *testing.T) {
		t.Parallel()
		s, c := startServer(t)
		s.MockResponse(mockh.BodyMatching(regexp.MustCompile(`"amount"\s*:\s*\d+`)), mockh.OK("has amount"))

		wantBody(t, c.post("/x", `{"amount": 250}`), "has amount")
		wantStatus(t, c.post("/x", `{"amount": "n/a"}`), http.StatusNotFound)
	})

	t.Run("empty body", func(t *testing.T) {
		t.Parallel()
		s, c := startServer(t)
		s.MockResponse(mockh.And(mockh.Post("/x"), mockh.EmptyBody()), mockh.OK("empty"))
		s.MockResponse(mockh.Post("/x"), mockh.OK("not empty"))

		wantBody(t, c.post("/x", ""), "empty")
		wantBody(t, c.post("/x", "payload"), "not empty")
	})
}

func TestCombinators(t *testing.T) {
	t.Parallel()

	t.Run("And requires every clause", func(t *testing.T) {
		t.Parallel()
		s, c := startServer(t)
		s.MockResponse(mockh.And(mockh.Post("/orders"), mockh.Header("X-Key", "k")), mockh.OK("created"))

		wantBody(t, c.post("/orders", "", map[string]string{"X-Key": "k"}), "created")
		wantStatus(t, c.post("/orders", ""), http.StatusNotFound)
		wantStatus(t, c.get("/orders", map[string]string{"X-Key": "k"}), http.StatusNotFound)
	})

	t.Run("Or accepts any clause", func(t *testing.T) {
		t.Parallel()
		s, c := startServer(t)
		s.MockResponse(mockh.Or(mockh.Path("/health"), mockh.Path("/healthz")), mockh.OK("UP"))

		wantBody(t, c.get("/health"), "UP")
		wantBody(t, c.get("/healthz"), "UP")
		wantStatus(t, c.get("/status"), http.StatusNotFound)
	})

	t.Run("Not inverts", func(t *testing.T) {
		t.Parallel()
		s, c := startServer(t)
		s.MockResponse(mockh.Not(mockh.HasHeader("Authorization")), mockh.Status(401).WithBody("unauthorized"))
		s.MockResponse(mockh.Any(), mockh.OK("welcome"))

		res := c.get("/x")
		wantStatus(t, res, http.StatusUnauthorized)
		wantBody(t, res, "unauthorized")
		wantBody(t, c.get("/x", map[string]string{"Authorization": "Bearer t"}), "welcome")
	})

	t.Run("empty And matches and empty Or does not", func(t *testing.T) {
		t.Parallel()
		s, c := startServer(t)
		s.MockResponse(mockh.Or(), mockh.OK("never"))
		s.MockResponse(mockh.And(), mockh.OK("always"))

		wantBody(t, c.get("/x"), "always")
	})

	t.Run("several clauses chained together", func(t *testing.T) {
		t.Parallel()
		s, c := startServer(t)
		matcher := mockh.And(
			mockh.Method("PUT"),
			mockh.PathTemplateMatching("/users/{id}"),
			mockh.Header("Content-Type", "application/json"),
			mockh.BodyContains(`"name"`),
		)
		s.MockResponse(matcher, mockh.OK("updated"))

		headers := map[string]string{"Content-Type": "application/json"}
		wantBody(t, c.put("/users/1", `{"name":"Ada"}`, headers), "updated")
		wantStatus(t, c.put("/users/1", `{"age":36}`, headers), http.StatusNotFound)
		wantStatus(t, c.put("/users/1", `{"name":"Ada"}`), http.StatusNotFound)
	})

	t.Run("Any matches everything", func(t *testing.T) {
		t.Parallel()
		s, c := startServer(t)
		s.MockResponse(mockh.Any(), mockh.OK("catch-all"))

		wantBody(t, c.get("/literally/anything?a=1"), "catch-all")
		wantBody(t, c.delete("/other"), "catch-all")
	})
}

func TestCustomMatcherFuncs(t *testing.T) {
	t.Parallel()

	t.Run("a plain func is a valid matcher", func(t *testing.T) {
		t.Parallel()
		s, c := startServer(t)
		s.MockResponse(func(r *mockh.Request) bool {
			return r.Method == "GET" && len(r.Path) > 10
		}, mockh.OK("long path"))

		wantBody(t, c.get("/a-very-long-path"), "long path")
		wantStatus(t, c.get("/short"), http.StatusNotFound)
	})

	t.Run("a matcher can combine anything about the request", func(t *testing.T) {
		t.Parallel()
		s, c := startServer(t)
		s.MockResponse(func(r *mockh.Request) bool {
			return r.Method == "POST" &&
				strings.HasPrefix(r.Path, "/api/") &&
				r.Param("version") == "2" &&
				strings.HasPrefix(r.Header("X-Api-Key"), "live_") &&
				strings.Contains(r.BodyText(), `"total"`)
		}, mockh.OK("all conditions met"))

		headers := map[string]string{"X-Api-Key": "live_abc"}
		wantBody(t, c.post("/api/checkout?version=2", `{"total":10}`, headers), "all conditions met")
		wantStatus(t, c.post("/api/checkout?version=1", `{"total":10}`, headers), http.StatusNotFound)
	})

	t.Run("a matcher that panics yields 500 and leaves the server usable", func(t *testing.T) {
		t.Parallel()
		s, c := startServer(t)
		s.MockResponse(func(*mockh.Request) bool { panic("boom") }, mockh.OK("never"))
		s.MockResponse(mockh.Any(), mockh.OK("unreachable"))

		res := c.get("/x")

		wantStatus(t, res, http.StatusInternalServerError)
		wantContains(t, res.body, "boom")
		wantContains(t, res.body, "GET /x")

		s.Clear()
		s.MockResponse(mockh.Any(), mockh.OK("still alive"))
		wantBody(t, c.get("/x"), "still alive")
	})
}
