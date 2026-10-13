package mockh

import (
	"net/http"
	"regexp"
	"strings"
)

// Matcher decides whether a mock applies to an incoming request.
//
// It is a plain function, so the ready-made matchers in this package compose
// with And, Or and Not, and a hand-written closure is just as valid a matcher
// as anything here.
type Matcher func(*Request) bool

// Responder produces the response for a request that has already matched.
//
// Because it is called per request it can be stateful - counting attempts to
// simulate a flaky upstream, for instance - as long as that state is
// goroutine-safe.
type Responder func(*Request) *Response

// And returns a matcher that holds only when every given matcher holds.
// Matchers are evaluated in order and evaluation stops at the first failure.
// And with no arguments matches everything.
func And(matchers ...Matcher) Matcher {
	return func(r *Request) bool {
		for _, m := range matchers {
			if !m(r) {
				return false
			}
		}
		return true
	}
}

// Or returns a matcher that holds when at least one of the given matchers
// holds. Matchers are evaluated in order and evaluation stops at the first
// success. Or with no arguments matches nothing.
func Or(matchers ...Matcher) Matcher {
	return func(r *Request) bool {
		for _, m := range matchers {
			if m(r) {
				return true
			}
		}
		return false
	}
}

// Not returns a matcher that inverts m.
func Not(m Matcher) Matcher {
	return func(r *Request) bool { return !m(r) }
}

// PathTemplate is a URI path template with {name} placeholders, such as
// "/users/{id}/orders/{orderId}". A placeholder matches exactly one
// non-empty path segment, so "/users/{id}" matches "/users/7" but neither
// "/users" nor "/users/7/orders".
type PathTemplate struct {
	template string
	names    []string
	re       *regexp.Regexp
}

var placeholderPattern = regexp.MustCompile(`\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// NewPathTemplate compiles a path template. Text around the placeholders is
// matched literally. The template is compiled once, so a template reused
// across requests should be built once and kept.
func NewPathTemplate(template string) *PathTemplate {
	t := &PathTemplate{template: template}
	var pattern strings.Builder
	pattern.WriteString(`\A`)
	cursor := 0
	for _, loc := range placeholderPattern.FindAllStringSubmatchIndex(template, -1) {
		pattern.WriteString(regexp.QuoteMeta(template[cursor:loc[0]]))
		pattern.WriteString(`([^/]+)`)
		t.names = append(t.names, template[loc[2]:loc[3]])
		cursor = loc[1]
	}
	pattern.WriteString(regexp.QuoteMeta(template[cursor:]))
	pattern.WriteString(`\z`)
	t.re = regexp.MustCompile(pattern.String())
	return t
}

// Template returns the template string this was compiled from.
func (t *PathTemplate) Template() string { return t.template }

// Names returns the placeholder names, in the order they appear.
func (t *PathTemplate) Names() []string { return append([]string(nil), t.names...) }

// Matches reports whether path fits the template in full.
func (t *PathTemplate) Matches(path string) bool { return t.re.MatchString(path) }

// Extract maps each variable name to its value in path, or returns an empty
// map when path does not fit the template.
func (t *PathTemplate) Extract(path string) map[string]string {
	groups := t.re.FindStringSubmatch(path)
	if groups == nil {
		return map[string]string{}
	}
	out := make(map[string]string, len(t.names))
	for i, name := range t.names {
		out[name] = groups[i+1]
	}
	return out
}

// anchored returns a regexp equivalent to re but required to match the whole
// subject, mirroring Kotlin's Regex.matches.
func anchored(re *regexp.Regexp) *regexp.Regexp {
	if a, err := regexp.Compile(`\A(?:` + re.String() + `)\z`); err == nil {
		return a
	}
	return re
}

// Any matches every request. Registered first it shadows every later mock, so
// it belongs last, as a catch-all.
func Any() Matcher {
	return func(*Request) bool { return true }
}

// Method matches on the HTTP verb alone, case-insensitively.
func Method(verb string) Matcher {
	return func(r *Request) bool { return strings.EqualFold(r.Method, verb) }
}

// Path matches an exact request path. The query string is not part of the
// path.
func Path(exact string) Matcher {
	return func(r *Request) bool { return r.Path == exact }
}

// PathPrefix matches any path starting with prefix.
func PathPrefix(prefix string) Matcher {
	return func(r *Request) bool { return strings.HasPrefix(r.Path, prefix) }
}

// PathMatching matches paths that the regexp matches in full, not merely
// somewhere inside.
func PathMatching(re *regexp.Regexp) Matcher {
	full := anchored(re)
	return func(r *Request) bool { return full.MatchString(r.Path) }
}

// PathTemplateMatching matches a {placeholder} template such as
// "/users/{id}". Use Request.PathVariables with the same template to read the
// captured values inside the responder.
func PathTemplateMatching(template string) Matcher {
	compiled := NewPathTemplate(template)
	return func(r *Request) bool { return compiled.Matches(r.Path) }
}

// Get matches a GET to an exact path - the common case, verb plus path.
func Get(path string) Matcher { return And(Method(http.MethodGet), Path(path)) }

// Post matches a POST to an exact path.
func Post(path string) Matcher { return And(Method(http.MethodPost), Path(path)) }

// Put matches a PUT to an exact path.
func Put(path string) Matcher { return And(Method(http.MethodPut), Path(path)) }

// Patch matches a PATCH to an exact path.
func Patch(path string) Matcher { return And(Method(http.MethodPatch), Path(path)) }

// Delete matches a DELETE to an exact path.
func Delete(path string) Matcher { return And(Method(http.MethodDelete), Path(path)) }

// Head matches a HEAD to an exact path. A matched HEAD gets the mock's
// headers and status but never its body.
func Head(path string) Matcher { return And(Method(http.MethodHead), Path(path)) }

// Options matches an OPTIONS request to an exact path.
func Options(path string) Matcher { return And(Method(http.MethodOptions), Path(path)) }

// Query matches when the named query parameter has the given value, comparing
// decoded values. A repeated parameter matches if any of its values does.
func Query(name, value string) Matcher {
	return func(r *Request) bool {
		for _, v := range r.ParamValues(name) {
			if v == value {
				return true
			}
		}
		return false
	}
}

// HasQuery matches when the named query parameter is present, whatever its
// value - "?debug" counts just as much as "?debug=true".
func HasQuery(name string) Matcher {
	return func(r *Request) bool { return r.HasParam(name) }
}

// QueryMatching matches when some value of the named query parameter is
// matched in full by the regexp.
func QueryMatching(name string, re *regexp.Regexp) Matcher {
	full := anchored(re)
	return func(r *Request) bool {
		for _, v := range r.ParamValues(name) {
			if full.MatchString(v) {
				return true
			}
		}
		return false
	}
}

// Header matches when the named header has the given value. Header names and
// values are both compared case-insensitively.
func Header(name, value string) Matcher {
	return func(r *Request) bool {
		for _, v := range r.HeaderValues(name) {
			if strings.EqualFold(v, value) {
				return true
			}
		}
		return false
	}
}

// HasHeader matches when the named header is present, whatever its value.
func HasHeader(name string) Matcher {
	return func(r *Request) bool { return r.HasHeader(name) }
}

// HeaderContaining matches when some value of the named header contains the
// substring, case-insensitively - handy for a token scheme ("Bearer ") or
// content negotiation on Accept.
func HeaderContaining(name, substring string) Matcher {
	needle := strings.ToLower(substring)
	return func(r *Request) bool {
		for _, v := range r.HeaderValues(name) {
			if strings.Contains(strings.ToLower(v), needle) {
				return true
			}
		}
		return false
	}
}

// Body matches when the request body, read as UTF-8 text, equals exact.
func Body(exact string) Matcher {
	return func(r *Request) bool { return r.BodyText() == exact }
}

// BodyContains matches when the request body contains the substring,
// case-sensitively.
func BodyContains(substring string) Matcher {
	return func(r *Request) bool { return strings.Contains(r.BodyText(), substring) }
}

// BodyMatching matches when the regexp matches anywhere in the request body.
func BodyMatching(re *regexp.Regexp) Matcher {
	return func(r *Request) bool { return re.MatchString(r.BodyText()) }
}

// EmptyBody matches requests that carry no body at all.
func EmptyBody() Matcher {
	return func(r *Request) bool { return len(r.Body) == 0 }
}
