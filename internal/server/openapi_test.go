package server

// The OpenAPI document at web/static/openapi.yaml is hand-written, so nothing
// but a test keeps it honest. The drift tests here hold it to the code: the
// document's path and method set against Server.routes().
//
// What they do not do is validate the document. The parse below is a minimal
// YAML decode, so a YAML-valid but OpenAPI-invalid document passes forever;
// validity is checked manually against an external validator.
//
// Every check is a pure function over two sets, proven against fixtures rather
// than by editing the real document and eyeballing the result.

import (
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// apiPathPrefix is the surface the document describes. Routes outside it (the
// index page, /dir, the static file server) are deliberately undocumented.
const apiPathPrefix = "/api/"

// openAPIMethods are the operation keys a path item may carry. Anything else at
// that level (summary, parameters, an accidental "gett") is not an operation and
// is ignored here — a mistyped method leaves the path with fewer methods than
// routes() registers, which the diff reports.
var openAPIMethods = []string{"get", "put", "post", "delete", "options", "head", "patch", "trace"}

// openAPIDoc is the sliver of the document the drift tests read. It is
// deliberately minimal — these tests check that the document agrees with the
// code, not that it is a valid OpenAPI document.
type openAPIDoc struct {
	Paths map[string]pathItem `yaml:"paths"`
}

// pathItem keeps operations as raw nodes: the path drift test needs the method
// keys, not the operation bodies.
type pathItem map[string]yaml.Node

// fetchOpenAPIDocument serves the document through the real handler rather than
// reading the file by path: it exercises handleOpenAPI, and keeps the document's
// location in one place (the routes table and Config.StaticDir).
func fetchOpenAPIDocument(t *testing.T) []byte {
	t.Helper()

	srv, _, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/openapi.yaml", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/openapi.yaml = %d, want 200: %s", w.Code, w.Body.String())
	}
	return w.Body.Bytes()
}

func parseOpenAPIDocument(t *testing.T, data []byte) openAPIDoc {
	t.Helper()

	var doc openAPIDoc
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse openapi.yaml: %v", err)
	}
	if len(doc.Paths) == 0 {
		t.Fatal("openapi.yaml declares no paths")
	}
	return doc
}

// loadOpenAPIDocument fetches and parses the served document.
func loadOpenAPIDocument(t *testing.T) openAPIDoc {
	t.Helper()
	return parseOpenAPIDocument(t, fetchOpenAPIDocument(t))
}

// apiRouteMethods maps each registered /api/ pattern to its methods, lowercased
// to match OpenAPI's operation keys. Go 1.22+ mux wildcards use the same brace
// syntax as OpenAPI path templating, so patterns need no translation.
func apiRouteMethods(routes []route) map[string][]string {
	byPath := make(map[string][]string)
	for _, rt := range routes {
		if !strings.HasPrefix(rt.pattern, apiPathPrefix) {
			continue
		}
		byPath[rt.pattern] = append(byPath[rt.pattern], strings.ToLower(rt.method))
	}
	for p := range byPath {
		slices.Sort(byPath[p])
	}
	return byPath
}

// documentedMethods maps each documented path to its operations.
func documentedMethods(doc openAPIDoc) map[string][]string {
	byPath := make(map[string][]string, len(doc.Paths))
	for path, item := range doc.Paths {
		methods := []string{}
		for key := range item {
			if slices.Contains(openAPIMethods, strings.ToLower(key)) {
				methods = append(methods, strings.ToLower(key))
			}
		}
		slices.Sort(methods)
		byPath[path] = methods
	}
	return byPath
}

// routeDiff is the outcome of comparing the two sets. The three groups are kept
// apart because they are three different defects: a route nobody documented, a
// path the server does not serve, and a path documented under the wrong method.
type routeDiff struct {
	undocumented     []string // registered /api/ route, absent from the document
	unregistered     []string // documented path that is not a registered /api/ route
	methodMismatches []string // path on both sides, differing method sets
}

func (d routeDiff) empty() bool {
	return len(d.undocumented) == 0 && len(d.unregistered) == 0 && len(d.methodMismatches) == 0
}

// diffAPIRoutes compares registered routes against documented paths in both
// directions. Both maps are path → sorted lowercase methods.
func diffAPIRoutes(registered, documented map[string][]string) routeDiff {
	var diff routeDiff

	for _, path := range slices.Sorted(maps.Keys(registered)) {
		docMethods, ok := documented[path]
		if !ok {
			diff.undocumented = append(diff.undocumented, path)
			continue
		}
		if !slices.Equal(registered[path], docMethods) {
			diff.methodMismatches = append(diff.methodMismatches, path)
		}
	}

	for _, path := range slices.Sorted(maps.Keys(documented)) {
		if _, ok := registered[path]; !ok {
			diff.unregistered = append(diff.unregistered, path)
		}
	}

	return diff
}

// TestOpenAPICoversEveryAPIRoute is the path drift test: routes() ∩ /api/ must
// equal the document's paths, per path and per method, in both directions.
func TestOpenAPICoversEveryAPIRoute(t *testing.T) {
	srv, _, _ := newTestServer(t)

	registered := apiRouteMethods(srv.routes())
	if len(registered) == 0 {
		t.Fatal("routes() has no /api/ routes; the filter is looking in the wrong place")
	}
	documented := documentedMethods(loadOpenAPIDocument(t))

	diff := diffAPIRoutes(registered, documented)

	for _, path := range diff.undocumented {
		t.Errorf("route %s %v is registered but missing from openapi.yaml", path, registered[path])
	}
	for _, path := range diff.unregistered {
		t.Errorf("openapi.yaml documents %s, which is not a registered /api/ route", path)
	}
	for _, path := range diff.methodMismatches {
		t.Errorf("%s: server registers %v, openapi.yaml documents %v",
			path, registered[path], documented[path])
	}
}

// TestDiffAPIRoutes proves the comparison fails in both directions, and on a
// method mismatch. The document side of every case goes through the real parse,
// so the fixtures exercise the same code the real test does.
func TestDiffAPIRoutes(t *testing.T) {
	tests := []struct {
		name             string
		routes           []route
		doc              string
		undocumented     []string
		unregistered     []string
		methodMismatches []string
	}{
		{
			name: "agreeing sets",
			routes: []route{
				{method: "GET", pattern: "/api/version"},
				{method: "GET", pattern: "/api/jobs/{id}"},
				{method: "POST", pattern: "/api/jobs/{id}"},
			},
			doc: `
paths:
  /api/version:
    get: {}
  /api/jobs/{id}:
    get: {}
    post: {}
`,
		},
		{
			name: "non-api routes are outside the compared set",
			routes: []route{
				{method: "GET", pattern: "/{$}"},
				{method: "GET", pattern: "/dir"},
				{method: "GET", pattern: "/api/version"},
			},
			doc: `
paths:
  /api/version:
    get: {}
`,
		},
		{
			name: "route missing from the document",
			routes: []route{
				{method: "GET", pattern: "/api/version"},
				{method: "GET", pattern: "/api/presets"},
			},
			doc: `
paths:
  /api/version:
    get: {}
`,
			undocumented: []string{"/api/presets"},
		},
		{
			name: "documented path that is not registered",
			routes: []route{
				{method: "GET", pattern: "/api/version"},
			},
			doc: `
paths:
  /api/version:
    get: {}
  /api/ghost:
    get: {}
`,
			unregistered: []string{"/api/ghost"},
		},
		{
			name: "documented under the wrong method",
			routes: []route{
				{method: "POST", pattern: "/api/scan"},
			},
			doc: `
paths:
  /api/scan:
    get: {}
`,
			methodMismatches: []string{"/api/scan"},
		},
		{
			name: "one method of two is undocumented",
			routes: []route{
				{method: "GET", pattern: "/api/jobs/{id}"},
				{method: "POST", pattern: "/api/jobs/{id}"},
			},
			doc: `
paths:
  /api/jobs/{id}:
    get: {}
`,
			methodMismatches: []string{"/api/jobs/{id}"},
		},
		{
			name: "path-item keys that are not operations are ignored",
			routes: []route{
				{method: "GET", pattern: "/api/cover"},
			},
			doc: `
paths:
  /api/cover:
    summary: Album art
    parameters: []
    get: {}
`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			documented := documentedMethods(parseOpenAPIDocument(t, []byte(tt.doc)))
			diff := diffAPIRoutes(apiRouteMethods(tt.routes), documented)

			if !slices.Equal(diff.undocumented, tt.undocumented) {
				t.Errorf("undocumented = %v, want %v", diff.undocumented, tt.undocumented)
			}
			if !slices.Equal(diff.unregistered, tt.unregistered) {
				t.Errorf("unregistered = %v, want %v", diff.unregistered, tt.unregistered)
			}
			if !slices.Equal(diff.methodMismatches, tt.methodMismatches) {
				t.Errorf("methodMismatches = %v, want %v", diff.methodMismatches, tt.methodMismatches)
			}
			wantEmpty := tt.undocumented == nil && tt.unregistered == nil && tt.methodMismatches == nil
			if diff.empty() != wantEmpty {
				t.Errorf("diff.empty() = %v, want %v", diff.empty(), wantEmpty)
			}
		})
	}
}
