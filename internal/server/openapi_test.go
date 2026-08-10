package server

// The OpenAPI document at web/static/openapi.yaml is hand-written, so nothing
// but a test keeps it honest. The drift tests here hold it to the code: the
// document's path and method set against Server.routes(), and its Error.code
// enum against allAPIErrorCodes.
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
	Paths      map[string]pathItem `yaml:"paths"`
	Components openAPIComponents   `yaml:"components"`
}

// pathItem keeps operations as raw nodes: the path drift test needs the method
// keys, not the operation bodies.
type pathItem map[string]yaml.Node

type openAPIComponents struct {
	Schemas map[string]schemaNode `yaml:"schemas"`
}

// schemaNode is one component schema, decoded only as far as the drift tests
// reach: the property names and, for scalars, the enum.
type schemaNode struct {
	Enum       []string              `yaml:"enum"`
	Properties map[string]schemaNode `yaml:"properties"`
}

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
	return doc
}

// loadOpenAPIDocument fetches and parses the served document. The emptiness
// checks are here rather than in the parse so that fixtures may exercise one
// section of the document without carrying the others.
func loadOpenAPIDocument(t *testing.T) openAPIDoc {
	t.Helper()

	doc := parseOpenAPIDocument(t, fetchOpenAPIDocument(t))
	if len(doc.Paths) == 0 {
		t.Fatal("openapi.yaml declares no paths")
	}
	if len(doc.Components.Schemas) == 0 {
		t.Fatal("openapi.yaml declares no component schemas")
	}
	return doc
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

// errorCodeEnum reads components.schemas.Error.properties.code.enum — the
// document's half of the error-code contract. A missing Error schema, or an
// Error without a code enum, yields an empty slice, which the diff reports as
// every code being undocumented.
func errorCodeEnum(doc openAPIDoc) []string {
	return doc.Components.Schemas["Error"].Properties["code"].Enum
}

// codeDiff is the outcome of comparing allAPIErrorCodes against the documented
// enum. Order is deliberately not compared — the document keeps the constant
// block's order for readability, but a reordering is not a contract change.
type codeDiff struct {
	undocumented []string // code the server can emit, absent from the enum
	unknown      []string // enum value backed by no code constant
	duplicated   []string // enum value listed more than once; set equality cannot see it
}

func (d codeDiff) empty() bool {
	return len(d.undocumented) == 0 && len(d.unknown) == 0 && len(d.duplicated) == 0
}

func diffErrorCodes(codes, enum []string) codeDiff {
	var diff codeDiff

	inEnum := make(map[string]int, len(enum))
	for _, value := range enum {
		inEnum[value]++
	}
	inCode := make(map[string]bool, len(codes))
	for _, code := range codes {
		inCode[code] = true
		if inEnum[code] == 0 {
			diff.undocumented = append(diff.undocumented, code)
		}
	}

	for _, value := range slices.Sorted(maps.Keys(inEnum)) {
		if !inCode[value] {
			diff.unknown = append(diff.unknown, value)
		}
		if inEnum[value] > 1 {
			diff.duplicated = append(diff.duplicated, value)
		}
	}

	return diff
}

// TestOpenAPIDocumentsEveryErrorCode is the error-code drift test:
// allAPIErrorCodes must equal the document's Error.code enum in both
// directions. It says nothing about *when* a code is returned — only that the
// set of codes the server can emit is the set the document publishes.
func TestOpenAPIDocumentsEveryErrorCode(t *testing.T) {
	enum := errorCodeEnum(loadOpenAPIDocument(t))
	if len(enum) == 0 {
		t.Fatal("openapi.yaml has no components.schemas.Error.properties.code.enum")
	}

	diff := diffErrorCodes(allAPIErrorCodes, enum)

	for _, code := range diff.undocumented {
		t.Errorf("error code %q is in allAPIErrorCodes but missing from the Error.code enum", code)
	}
	for _, code := range diff.unknown {
		t.Errorf("openapi.yaml documents error code %q, which no constant declares", code)
	}
	for _, code := range diff.duplicated {
		t.Errorf("the Error.code enum lists %q more than once", code)
	}
}

// TestDiffErrorCodes proves the comparison fails in both directions. Every
// fixture goes through the real parse and the real enum lookup, so those are
// exercised too.
func TestDiffErrorCodes(t *testing.T) {
	tests := []struct {
		name         string
		codes        []string
		doc          string
		undocumented []string
		unknown      []string
		duplicated   []string
	}{
		{
			name:  "agreeing sets",
			codes: []string{"invalid_request", "job_not_found"},
			doc: `
components:
  schemas:
    Error:
      properties:
        code:
          enum:
            - invalid_request
            - job_not_found
`,
		},
		{
			name:  "order is not part of the contract",
			codes: []string{"invalid_request", "job_not_found"},
			doc: `
components:
  schemas:
    Error:
      properties:
        code:
          enum:
            - job_not_found
            - invalid_request
`,
		},
		{
			name:  "code missing from the document",
			codes: []string{"invalid_request", "job_not_found"},
			doc: `
components:
  schemas:
    Error:
      properties:
        code:
          enum:
            - invalid_request
`,
			undocumented: []string{"job_not_found"},
		},
		{
			name:  "documented code that no constant declares",
			codes: []string{"invalid_request"},
			doc: `
components:
  schemas:
    Error:
      properties:
        code:
          enum:
            - invalid_request
            - retired_code
`,
			unknown: []string{"retired_code"},
		},
		{
			name:  "drift in both directions at once",
			codes: []string{"invalid_request", "no_cover"},
			doc: `
components:
  schemas:
    Error:
      properties:
        code:
          enum:
            - invalid_request
            - retired_code
`,
			undocumented: []string{"no_cover"},
			unknown:      []string{"retired_code"},
		},
		{
			name:  "duplicate enum entry",
			codes: []string{"invalid_request", "no_cover"},
			doc: `
components:
  schemas:
    Error:
      properties:
        code:
          enum:
            - invalid_request
            - no_cover
            - invalid_request
`,
			duplicated: []string{"invalid_request"},
		},
		{
			name:  "the enum moved or was renamed away",
			codes: []string{"invalid_request"},
			doc: `
components:
  schemas:
    Error:
      properties:
        error:
          type: string
`,
			undocumented: []string{"invalid_request"},
		},
		{
			name:  "no Error schema at all",
			codes: []string{"invalid_request"},
			doc: `
components:
  schemas:
    Version:
      properties:
        version:
          type: string
`,
			undocumented: []string{"invalid_request"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			enum := errorCodeEnum(parseOpenAPIDocument(t, []byte(tt.doc)))
			diff := diffErrorCodes(tt.codes, enum)

			if !slices.Equal(diff.undocumented, tt.undocumented) {
				t.Errorf("undocumented = %v, want %v", diff.undocumented, tt.undocumented)
			}
			if !slices.Equal(diff.unknown, tt.unknown) {
				t.Errorf("unknown = %v, want %v", diff.unknown, tt.unknown)
			}
			if !slices.Equal(diff.duplicated, tt.duplicated) {
				t.Errorf("duplicated = %v, want %v", diff.duplicated, tt.duplicated)
			}
			wantEmpty := tt.undocumented == nil && tt.unknown == nil && tt.duplicated == nil
			if diff.empty() != wantEmpty {
				t.Errorf("diff.empty() = %v, want %v", diff.empty(), wantEmpty)
			}
		})
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
