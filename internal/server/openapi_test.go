package server

// The OpenAPI document at web/static/openapi.yaml is hand-written, so nothing
// but a test keeps it honest. The drift tests here hold it to the code: the
// document's path and method set against Server.routes(), its Error.code enum
// against allAPIErrorCodes, and each component schema's properties against the
// json tags of the DTO struct behind it.
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
	"reflect"
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

// schemaDTOs maps each component schema to the Go struct whose json tags define
// its properties. This is the rot vector the path drift test cannot see:
// renaming a field in trackDTO leaves every other test in the package green.
var schemaDTOs = map[string]reflect.Type{
	"Library":           reflect.TypeFor[libraryDTO](),
	"Directory":         reflect.TypeFor[directoryDTO](),
	"Track":             reflect.TypeFor[trackDTO](),
	"ScanState":         reflect.TypeFor[scanStateDTO](),
	"ScanDirResult":     reflect.TypeFor[scanDirResultDTO](),
	"ScanEvent":         reflect.TypeFor[ScanEvent](),
	"Preset":            reflect.TypeFor[presetDTO](),
	"TranscodeRequest":  reflect.TypeFor[transcodeRequest](),
	"TranscodeAccepted": reflect.TypeFor[transcodeAcceptedDTO](),
	"SkippedTrack":      reflect.TypeFor[skippedDTO](),
	"JobEvent":          reflect.TypeFor[jobEvent](),
	"JobDetail":         reflect.TypeFor[jobDetailDTO](),
	"Error":             reflect.TypeFor[apiErrorDTO](),
}

// unreflectableSchemas names the component schemas no struct backs, with the
// reason. They are documented but unchecked, and saying so here is the point:
// a silent gap would make this test's coverage look larger than it is.
// TestOpenAPISchemaTableCoversEveryComponent holds the two lists to the
// document, so a new schema must land in one of them.
//
// The map-backed bodies are legacy — AGENTS.md requires a named DTO for every
// new JSON response — and converting them is a follow-up, not this plan's job.
var unreflectableSchemas = map[string]string{
	"JobLog":    "handleTranscodeLog builds it from a map[string]any (handlers_transcode.go)",
	"Version":   "handleVersion writes a map[string]any (handlers_meta.go)",
	"JobStatus": "a scalar string enum, not an object",
}

// Also outside this test, for a different reason: the response bodies the
// document describes inline rather than as a component — the /api/dir wrapper
// (handlers_api.go, a map literal), POST /api/scan's map[string]string, the
// {"status": …} acknowledgements of job cancel/remove, and the /api/presets and
// /api/jobs wrappers (presetsDTO and the jobs list, structs the document
// spells out at the operation). Only named components are compared.

// dtoPropertyNames returns the sorted JSON property names a struct marshals to,
// honouring `-` and the option suffix (omitempty, string, …). Embedded structs
// are deliberately not flattened: no DTO in the table has one, and quietly
// flattening a future one would hide the very drift this test looks for.
func dtoPropertyNames(t reflect.Type) []string {
	names := []string{}
	for field := range t.Fields() {
		if !field.IsExported() {
			continue
		}
		tag := field.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if name == "" {
			name = field.Name
		}
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// schemaDiff is the outcome of comparing one struct against one schema. Only
// property *names* are compared: matching Go kinds to JSON Schema types is a
// much larger job for much less value, so a field whose type changed from
// string to int passes here.
type schemaDiff struct {
	undocumented []string // json field of the struct, absent from the schema
	unknown      []string // schema property backed by no json field
}

func (d schemaDiff) empty() bool {
	return len(d.undocumented) == 0 && len(d.unknown) == 0
}

// diffSchemaProperties compares the two name sets in both directions.
func diffSchemaProperties(fields, properties []string) schemaDiff {
	var diff schemaDiff

	documented := make(map[string]bool, len(properties))
	for _, name := range properties {
		documented[name] = true
	}
	declared := make(map[string]bool, len(fields))
	for _, name := range fields {
		declared[name] = true
		if !documented[name] {
			diff.undocumented = append(diff.undocumented, name)
		}
	}

	for _, name := range properties {
		if !declared[name] {
			diff.unknown = append(diff.unknown, name)
		}
	}

	return diff
}

// TestOpenAPISchemasMatchDTOs is the schema drift test: every documented
// component schema's property set must equal the json tags of the DTO behind
// it, in both directions. See unreflectableSchemas for what it cannot cover.
func TestOpenAPISchemasMatchDTOs(t *testing.T) {
	doc := loadOpenAPIDocument(t)

	for _, name := range slices.Sorted(maps.Keys(schemaDTOs)) {
		t.Run(name, func(t *testing.T) {
			schema, ok := doc.Components.Schemas[name]
			if !ok {
				t.Fatalf("openapi.yaml has no components.schemas.%s", name)
			}
			dto := schemaDTOs[name]
			properties := slices.Sorted(maps.Keys(schema.Properties))
			if len(properties) == 0 {
				t.Fatalf("components.schemas.%s declares no properties", name)
			}

			diff := diffSchemaProperties(dtoPropertyNames(dto), properties)

			for _, field := range diff.undocumented {
				t.Errorf("%s has a json field %q that the %s schema does not document",
					dto, field, name)
			}
			for _, property := range diff.unknown {
				t.Errorf("the %s schema documents a property %q that %s does not marshal",
					name, property, dto)
			}
		})
	}
}

// TestOpenAPISchemaTableCoversEveryComponent keeps schemaDTOs and
// unreflectableSchemas honest against the document itself: a component schema
// in neither list would be silently uncovered, and an entry naming a schema the
// document no longer declares is dead weight that reads like coverage.
func TestOpenAPISchemaTableCoversEveryComponent(t *testing.T) {
	doc := loadOpenAPIDocument(t)

	for _, name := range slices.Sorted(maps.Keys(doc.Components.Schemas)) {
		_, mapped := schemaDTOs[name]
		_, excluded := unreflectableSchemas[name]
		switch {
		case mapped && excluded:
			t.Errorf("schema %s is both mapped to a DTO and listed as unreflectable", name)
		case !mapped && !excluded:
			t.Errorf("schema %s is in neither schemaDTOs nor unreflectableSchemas, "+
				"so nothing checks it against the code", name)
		}
	}

	for _, name := range slices.Sorted(maps.Keys(schemaDTOs)) {
		if _, ok := doc.Components.Schemas[name]; !ok {
			t.Errorf("schemaDTOs maps %s, which the document does not declare", name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(unreflectableSchemas)) {
		if _, ok := doc.Components.Schemas[name]; !ok {
			t.Errorf("unreflectableSchemas lists %s, which the document does not declare", name)
		}
	}
}

// Fixture structs for TestDiffSchemaProperties. They stand in for a DTO so the
// proof does not depend on editing a real one.
type (
	// schemaFixtureDTO is the shape the fixture documents agree with.
	schemaFixtureDTO struct {
		ID    int64  `json:"id"`
		Name  string `json:"name"`
		Note  string `json:"note,omitempty"`
		Extra any    `json:"-"`
		//nolint:unused // reflected over, never read
		hidden string
	}

	// schemaFixtureRenamedDTO is schemaFixtureDTO with `name` renamed, the
	// exact drift a path-only test cannot see.
	schemaFixtureRenamedDTO struct {
		ID    int64  `json:"id"`
		Title string `json:"title"`
		Note  string `json:"note,omitempty"`
	}

	// schemaFixtureUntaggedDTO has a field with no json tag, which marshals
	// under its Go name.
	schemaFixtureUntaggedDTO struct {
		ID   int64 `json:"id"`
		Name string
	}
)

// TestDiffSchemaProperties proves the comparison fails in both directions, and
// that the tag rules (`-`, omitempty, an absent tag) are honoured. Every
// fixture document goes through the real parse.
func TestDiffSchemaProperties(t *testing.T) {
	const agreeingDoc = `
components:
  schemas:
    Fixture:
      properties:
        id:
          type: integer
        name:
          type: string
        note:
          type: string
`

	tests := []struct {
		name         string
		dto          reflect.Type
		doc          string
		undocumented []string
		unknown      []string
	}{
		{
			name: "agreeing sets, with omitempty and a skipped field",
			dto:  reflect.TypeFor[schemaFixtureDTO](),
			doc:  agreeingDoc,
		},
		{
			name:         "field renamed in Go",
			dto:          reflect.TypeFor[schemaFixtureRenamedDTO](),
			doc:          agreeingDoc,
			undocumented: []string{"title"},
			unknown:      []string{"name"},
		},
		{
			name: "field the document never got",
			dto:  reflect.TypeFor[schemaFixtureDTO](),
			doc: `
components:
  schemas:
    Fixture:
      properties:
        id:
          type: integer
        name:
          type: string
`,
			undocumented: []string{"note"},
		},
		{
			name: "documented property backed by no field",
			dto:  reflect.TypeFor[schemaFixtureDTO](),
			doc: `
components:
  schemas:
    Fixture:
      properties:
        id:
          type: integer
        name:
          type: string
        note:
          type: string
        retired:
          type: string
`,
			unknown: []string{"retired"},
		},
		{
			name: "a json:\"-\" field is not a property",
			dto:  reflect.TypeFor[schemaFixtureDTO](),
			doc: `
components:
  schemas:
    Fixture:
      properties:
        id:
          type: integer
        name:
          type: string
        note:
          type: string
        Extra:
          type: string
`,
			unknown: []string{"Extra"},
		},
		{
			name: "an untagged field marshals under its Go name",
			dto:  reflect.TypeFor[schemaFixtureUntaggedDTO](),
			doc: `
components:
  schemas:
    Fixture:
      properties:
        id:
          type: integer
        Name:
          type: string
`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			schema := parseOpenAPIDocument(t, []byte(tt.doc)).Components.Schemas["Fixture"]
			properties := slices.Sorted(maps.Keys(schema.Properties))

			diff := diffSchemaProperties(dtoPropertyNames(tt.dto), properties)

			if !slices.Equal(diff.undocumented, tt.undocumented) {
				t.Errorf("undocumented = %v, want %v", diff.undocumented, tt.undocumented)
			}
			if !slices.Equal(diff.unknown, tt.unknown) {
				t.Errorf("unknown = %v, want %v", diff.unknown, tt.unknown)
			}
			wantEmpty := tt.undocumented == nil && tt.unknown == nil
			if diff.empty() != wantEmpty {
				t.Errorf("diff.empty() = %v, want %v", diff.empty(), wantEmpty)
			}
		})
	}
}
