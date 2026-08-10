package server

// The error-code contract has three ways to rot that no runtime test can see:
// a code constant that never reaches allAPIErrorCodes, a handler that emits a
// code no constant declares, and a route registered without going through
// routes() — invisible to the OpenAPI path drift test, because http.ServeMux
// cannot enumerate what it has been given. One AST pass over the package closes
// all three.
//
// Every check runs twice: over the real package (TestErrorCodeHygiene) and over
// intentionally-wrong fixture sources, so the checks are proven to fail rather
// than assumed to.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// codeConstNamePattern is the naming convention every API error-code constant
// follows. The pass collects by name, so a code constant named outside it is
// invisible here — which is what the convention is for.
var codeConstNamePattern = regexp.MustCompile(`^code[A-Z]`)

// errorCodeSliceName is the slice that must list every code constant.
const errorCodeSliceName = "allAPIErrorCodes"

// registerRoutesAllowedPatterns are the only patterns allowed to be registered
// directly in registerRoutes instead of through routes(): a fallback, a
// non-/api/ alias, and a prefix-matched http.Handler.
var registerRoutesAllowedPatterns = []string{"GET /{path...}", "GET /favicon.ico", "GET /static/"}

// astPackage is a parsed set of files sharing one FileSet, so positions are
// comparable across files.
type astPackage struct {
	fset  *token.FileSet
	files []*ast.File
}

func (p astPackage) pos(n ast.Node) string {
	return p.fset.Position(n.Pos()).String()
}

// parseServerPackage parses every non-test file of the package under test.
// Files are read from the directory rather than named individually: a code
// constant later declared in a new file must not be able to hide from this pass.
// (parser.ParseDir would say this in one call but is deprecated, which
// staticcheck rejects.)
func parseServerPackage(t *testing.T) astPackage {
	t.Helper()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	pkg := astPackage{fset: token.NewFileSet()}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(pkg.fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		pkg.files = append(pkg.files, f)
	}
	if len(pkg.files) == 0 {
		t.Fatal("no non-test Go files found in the package directory")
	}
	return pkg
}

// parseFixture parses one or more fixture sources as a single package, so a
// check can be proven to see a declaration in any file of it.
func parseFixture(t *testing.T, srcs ...string) astPackage {
	t.Helper()

	pkg := astPackage{fset: token.NewFileSet()}
	for i, src := range srcs {
		f, err := parser.ParseFile(pkg.fset, fmt.Sprintf("fixture%d.go", i), src, 0)
		if err != nil {
			t.Fatalf("parse fixture %d: %v", i, err)
		}
		pkg.files = append(pkg.files, f)
	}
	return pkg
}

// stringLiteral returns the value of an untagged string literal, or "" for any
// other expression.
func stringLiteral(e ast.Expr) string {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return ""
	}
	s, err := strconv.Unquote(lit.Value)
	if err != nil {
		return ""
	}
	return s
}

// collectCodeConstants maps each error-code constant name to its string value.
// The value is "" when it is not a plain string literal, which is itself a
// finding rather than something to skip.
func collectCodeConstants(pkg astPackage) map[string]string {
	consts := make(map[string]string)
	for _, f := range pkg.files {
		ast.Inspect(f, func(n ast.Node) bool {
			decl, ok := n.(*ast.GenDecl)
			if !ok || decl.Tok != token.CONST {
				return true
			}
			for _, spec := range decl.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range vs.Names {
					if !codeConstNamePattern.MatchString(name.Name) {
						continue
					}
					value := ""
					if i < len(vs.Values) {
						value = stringLiteral(vs.Values[i])
					}
					consts[name.Name] = value
				}
			}
			return true
		})
	}
	return consts
}

// codeSliceEntry is one element of the allAPIErrorCodes literal. ident is empty
// for a bare string entry; value is empty when the identifier resolves to no
// code constant.
type codeSliceEntry struct {
	ident string
	value string
}

func collectErrorCodeSlice(pkg astPackage, consts map[string]string) (entries []codeSliceEntry, found bool) {
	for _, f := range pkg.files {
		ast.Inspect(f, func(n ast.Node) bool {
			decl, ok := n.(*ast.GenDecl)
			if !ok || decl.Tok != token.VAR {
				return true
			}
			for _, spec := range decl.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range vs.Names {
					if name.Name != errorCodeSliceName || i >= len(vs.Values) {
						continue
					}
					lit, ok := vs.Values[i].(*ast.CompositeLit)
					if !ok {
						continue
					}
					found = true
					for _, elt := range lit.Elts {
						if ident, ok := elt.(*ast.Ident); ok {
							entries = append(entries, codeSliceEntry{ident: ident.Name, value: consts[ident.Name]})
							continue
						}
						entries = append(entries, codeSliceEntry{value: stringLiteral(elt)})
					}
				}
			}
			return true
		})
	}
	return entries, found
}

// checkCodeConstantsListed holds the constant block and allAPIErrorCodes to each
// other in both directions.
func checkCodeConstantsListed(consts map[string]string, entries []codeSliceEntry, found bool) []string {
	if !found {
		return []string{fmt.Sprintf("no %s slice literal declared in the package", errorCodeSliceName)}
	}

	var problems []string
	listed := make(map[string]bool, len(entries))
	for _, e := range entries {
		switch {
		case e.ident == "":
			problems = append(problems, fmt.Sprintf(
				"%s lists the bare string %q; list the code constant instead", errorCodeSliceName, e.value))
		case e.value == "":
			problems = append(problems, fmt.Sprintf(
				"%s lists %s, which is not an API error-code constant", errorCodeSliceName, e.ident))
			continue
		}
		if listed[e.value] {
			problems = append(problems, fmt.Sprintf("%s lists %q more than once", errorCodeSliceName, e.value))
		}
		listed[e.value] = true
	}

	for _, name := range slices.Sorted(maps.Keys(consts)) {
		value := consts[name]
		if value == "" {
			problems = append(problems, fmt.Sprintf(
				"constant %s has no string literal value, so the drift tests cannot read its code", name))
			continue
		}
		if !listed[value] {
			problems = append(problems, fmt.Sprintf(
				"constant %s (%q) is missing from %s", name, value, errorCodeSliceName))
		}
	}
	return problems
}

// checkWriteAPIErrorCalls requires every emitted code to resolve to a declared
// code constant. "Any identifier" would be too weak: a local
// const foo = "undocumented" satisfies it and emits a code that appears in
// neither the slice nor the document.
func checkWriteAPIErrorCalls(pkg astPackage, consts map[string]string) []string {
	const codeArgIndex = 2

	var problems []string
	for _, f := range pkg.files {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if fn, ok := call.Fun.(*ast.Ident); !ok || fn.Name != "writeAPIError" {
				return true
			}
			if len(call.Args) <= codeArgIndex {
				problems = append(problems, fmt.Sprintf(
					"%s: writeAPIError called with %d arguments", pkg.pos(call), len(call.Args)))
				return true
			}
			arg := call.Args[codeArgIndex]
			ident, ok := arg.(*ast.Ident)
			if !ok {
				problems = append(problems, fmt.Sprintf(
					"%s: writeAPIError code argument is not an identifier; pass a code constant", pkg.pos(arg)))
				return true
			}
			if _, ok := consts[ident.Name]; !ok {
				problems = append(problems, fmt.Sprintf(
					"%s: writeAPIError code argument %s is not an API error-code constant", pkg.pos(arg), ident.Name))
			}
			return true
		})
	}
	return problems
}

// muxRegistration reports whether call adds a route to a mux. Matching on the
// method name alone would collide with slog.Handler.Handle, so a call qualifies
// only if it also looks like a registration: a mux-named receiver, or a literal
// or concatenated pattern as its first argument.
func muxRegistration(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || (sel.Sel.Name != "Handle" && sel.Sel.Name != "HandleFunc") || len(call.Args) != 2 {
		return false
	}
	if muxReceiver(sel.X) {
		return true
	}
	switch arg := call.Args[0].(type) {
	case *ast.BasicLit:
		return arg.Kind == token.STRING
	case *ast.BinaryExpr:
		return arg.Op == token.ADD
	default:
		return false
	}
}

func muxReceiver(x ast.Expr) bool {
	switch r := x.(type) {
	case *ast.Ident:
		return strings.Contains(strings.ToLower(r.Name), "mux")
	case *ast.SelectorExpr:
		return strings.Contains(strings.ToLower(r.Sel.Name), "mux")
	default:
		return false
	}
}

func muxRegistrations(f *ast.File) []*ast.CallExpr {
	var calls []*ast.CallExpr
	ast.Inspect(f, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok && muxRegistration(call) {
			calls = append(calls, call)
		}
		return true
	})
	return calls
}

// checkRegisterRoutes requires that the only route registrations in the package
// are the routes() loop and the three pattern-whitelisted exceptions. The
// weaker rule "registrations live inside registerRoutes" does not close the back
// door: a route added directly in registerRoutes satisfies it and still bypasses
// routes(), where the OpenAPI path drift test has no way to see it.
func checkRegisterRoutes(pkg astPackage) []string {
	var problems []string

	var decl *ast.FuncDecl
	for _, f := range pkg.files {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Name.Name != "registerRoutes" || fd.Body == nil {
				continue
			}
			if decl != nil {
				problems = append(problems, fmt.Sprintf("%s: a second registerRoutes declaration", pkg.pos(fd)))
				continue
			}
			decl = fd
		}
	}
	if decl == nil {
		return append(problems, "no registerRoutes function found")
	}

	var loop *ast.RangeStmt
	ast.Inspect(decl.Body, func(n ast.Node) bool {
		rng, ok := n.(*ast.RangeStmt)
		if !ok {
			return true
		}
		call, ok := rng.X.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); !ok || sel.Sel.Name != "routes" {
			return true
		}
		if loop != nil {
			problems = append(problems, fmt.Sprintf("%s: a second routes() loop in registerRoutes", pkg.pos(rng)))
			return true
		}
		loop = rng
		return true
	})
	if loop == nil {
		problems = append(problems, "registerRoutes does not loop over routes(); that table is what the OpenAPI drift test enumerates")
	}

	// inLoop counts registrations inside the routes() loop. The exemption is
	// positional, so it has to be "exactly one": a second registration nested in
	// that body would otherwise be whitelisted by position alone — the same back
	// door as registering directly in registerRoutes, and equally invisible to
	// the path drift test.
	inLoop := 0
	for _, f := range pkg.files {
		for _, reg := range muxRegistrations(f) {
			if reg.Pos() < decl.Pos() || reg.Pos() >= decl.End() {
				problems = append(problems, fmt.Sprintf(
					"%s: route registered outside registerRoutes", pkg.pos(reg)))
				continue
			}
			if loop != nil && reg.Pos() >= loop.Pos() && reg.Pos() < loop.End() {
				inLoop++
				if inLoop > 1 {
					problems = append(problems, fmt.Sprintf(
						"%s: a second registration inside the routes() loop; add the route to routes() instead",
						pkg.pos(reg)))
				}
				continue // the table loop itself, whose pattern is built per entry
			}
			pattern := stringLiteral(reg.Args[0])
			if !slices.Contains(registerRoutesAllowedPatterns, pattern) {
				problems = append(problems, fmt.Sprintf(
					"%s: %q is registered directly in registerRoutes; add it to routes() instead",
					pkg.pos(reg), pattern))
			}
		}
	}
	if loop != nil && inLoop == 0 {
		problems = append(problems, fmt.Sprintf(
			"%s: the routes() loop registers nothing; the table would be inert", pkg.pos(loop)))
	}
	return problems
}

// TestErrorCodeHygiene runs the three checks over the real package.
func TestErrorCodeHygiene(t *testing.T) {
	pkg := parseServerPackage(t)
	consts := collectCodeConstants(pkg)
	if len(consts) == 0 {
		t.Fatalf("no constants matching %s found; the pass is looking in the wrong place", codeConstNamePattern)
	}
	entries, found := collectErrorCodeSlice(pkg, consts)

	checks := []struct {
		name     string
		problems []string
	}{
		{"every code constant is listed in " + errorCodeSliceName, checkCodeConstantsListed(consts, entries, found)},
		{"every emitted code resolves to a code constant", checkWriteAPIErrorCalls(pkg, consts)},
		{"every route goes through routes()", checkRegisterRoutes(pkg)},
	}
	for _, c := range checks {
		t.Run(c.name, func(t *testing.T) {
			for _, p := range c.problems {
				t.Error(p)
			}
		})
	}
}

// TestAllAPIErrorCodesNoDuplicates checks the slice's values at runtime, where
// two constants sharing a code string look identical to the AST pass.
func TestAllAPIErrorCodesNoDuplicates(t *testing.T) {
	seen := make(map[string]bool, len(allAPIErrorCodes))
	for _, code := range allAPIErrorCodes {
		if code == "" {
			t.Error("allAPIErrorCodes contains an empty code")
		}
		if seen[code] {
			t.Errorf("allAPIErrorCodes lists %q more than once", code)
		}
		seen[code] = true
	}
}

// TestAllJobStatusesMatchConstants pins allJobStatuses to the JobStatus const
// block, the way checkCodeConstantsListed pins allAPIErrorCodes to the code
// constants. Without it a new status constant leaves the enum drift test
// (TestOpenAPIDocumentsEveryJobStatus) comparing the document against a stale
// slice and passing.
func TestAllJobStatusesMatchConstants(t *testing.T) {
	declared := jobStatusConstants(parseServerPackage(t))
	if len(declared) == 0 {
		t.Fatal("no JobStatus constants found; the pass is looking in the wrong place")
	}

	listed := map[string]bool{}
	for _, s := range allJobStatuses {
		if listed[string(s)] {
			t.Errorf("allJobStatuses lists %q more than once", s)
		}
		listed[string(s)] = true
	}

	for status := range declared {
		if !listed[status] {
			t.Errorf("JobStatus constant %q is not listed in allJobStatuses", status)
		}
	}
	for status := range listed {
		if !declared[status] {
			t.Errorf("allJobStatuses lists %q, which no JobStatus constant declares", status)
		}
	}
}

// jobStatusConstants collects the values of every explicitly JobStatus-typed
// constant in the package. The type is what identifies them — a name pattern
// would miss a constant named otherwise, and the block has no iota to carry a
// type across specs.
func jobStatusConstants(pkg astPackage) map[string]bool {
	declared := map[string]bool{}
	for _, f := range pkg.files {
		for _, d := range f.Decls {
			gen, ok := d.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				if ident, ok := vs.Type.(*ast.Ident); !ok || ident.Name != "JobStatus" {
					continue
				}
				for _, v := range vs.Values {
					if lit := stringLiteral(v); lit != "" {
						declared[lit] = true
					}
				}
			}
		}
	}
	return declared
}

func TestCheckCodeConstantsListed(t *testing.T) {
	tests := []struct {
		name string
		srcs []string
		want string // substring of the expected problem; empty means "no problems"
	}{
		{
			name: "every constant listed",
			srcs: []string{`package server
const (
	codeOne = "one"
	codeTwo = "two"
)
var allAPIErrorCodes = []string{codeOne, codeTwo}`},
		},
		{
			name: "constant missing from the slice",
			srcs: []string{`package server
const (
	codeOne = "one"
	codeTwo = "two"
)
var allAPIErrorCodes = []string{codeOne}`},
			want: `constant codeTwo ("two") is missing from allAPIErrorCodes`,
		},
		{
			name: "constant declared in another file of the package is still seen",
			srcs: []string{`package server
const codeOne = "one"
var allAPIErrorCodes = []string{codeOne}`, `package server
const codeTwo = "two"`},
			want: `constant codeTwo ("two") is missing from allAPIErrorCodes`,
		},
		{
			name: "constant declared inside a function body is still seen",
			srcs: []string{`package server
const codeOne = "one"
var allAPIErrorCodes = []string{codeOne}
func other() { const codeSneaky = "sneaky"; _ = codeSneaky }`},
			want: `constant codeSneaky ("sneaky") is missing from allAPIErrorCodes`,
		},
		{
			name: "slice entry that resolves to nothing",
			srcs: []string{`package server
const codeOne = "one"
var allAPIErrorCodes = []string{codeOne, codeGone}`},
			want: "allAPIErrorCodes lists codeGone, which is not an API error-code constant",
		},
		{
			name: "bare string instead of the constant",
			srcs: []string{`package server
const codeOne = "one"
var allAPIErrorCodes = []string{"one"}`},
			want: `allAPIErrorCodes lists the bare string "one"`,
		},
		{
			name: "duplicate entry",
			srcs: []string{`package server
const codeOne = "one"
var allAPIErrorCodes = []string{codeOne, codeOne}`},
			want: `allAPIErrorCodes lists "one" more than once`,
		},
		{
			name: "no slice at all",
			srcs: []string{`package server
const codeOne = "one"`},
			want: "no allAPIErrorCodes slice literal declared in the package",
		},
		{
			name: "constant without a string literal value",
			srcs: []string{`package server
const codeOne = other
var allAPIErrorCodes = []string{codeOne}`},
			want: "constant codeOne has no string literal value",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pkg := parseFixture(t, tt.srcs...)
			consts := collectCodeConstants(pkg)
			entries, found := collectErrorCodeSlice(pkg, consts)
			assertProblem(t, checkCodeConstantsListed(consts, entries, found), tt.want)
		})
	}
}

func TestCheckWriteAPIErrorCalls(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want string
	}{
		{
			name: "code constant passed",
			src: `package server
const codeOne = "one"
func h() { writeAPIError(w, 400, codeOne, "boom", nil) }`,
		},
		{
			name: "string literal passed",
			src: `package server
const codeOne = "one"
func h() { writeAPIError(w, 400, "undocumented", "boom", nil) }`,
			want: "writeAPIError code argument is not an identifier",
		},
		{
			name: "identifier that is not a code constant",
			src: `package server
const codeOne = "one"
func h() { const sneaky = "undocumented"; writeAPIError(w, 400, sneaky, "boom", nil) }`,
			want: "writeAPIError code argument sneaky is not an API error-code constant",
		},
		{
			name: "field selector passed",
			src: `package server
const codeOne = "one"
func h() { writeAPIError(w, 400, tt.code, "boom", nil) }`,
			want: "writeAPIError code argument is not an identifier",
		},
		{
			name: "too few arguments",
			src: `package server
const codeOne = "one"
func h() { writeAPIError(w, 400) }`,
			want: "writeAPIError called with 2 arguments",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pkg := parseFixture(t, tt.src)
			assertProblem(t, checkWriteAPIErrorCalls(pkg, collectCodeConstants(pkg)), tt.want)
		})
	}
}

func TestCheckRegisterRoutes(t *testing.T) {
	const good = `package server
func (s *Server) registerRoutes() {
	for _, rt := range s.routes() {
		s.mux.HandleFunc(rt.method+" "+rt.pattern, rt.handler)
	}
	s.mux.HandleFunc("GET /{path...}", s.handleNotFoundPage)
	s.mux.Handle("GET /favicon.ico", faviconHandler)
	s.mux.Handle("GET /static/", staticHandler)
}`

	tests := []struct {
		name string
		srcs []string
		want string
	}{
		{name: "table plus the three exceptions", srcs: []string{good}},
		{
			name: "route added directly inside registerRoutes",
			srcs: []string{`package server
func (s *Server) registerRoutes() {
	for _, rt := range s.routes() {
		s.mux.HandleFunc(rt.method+" "+rt.pattern, rt.handler)
	}
	s.mux.HandleFunc("GET /api/probe", s.handleProbe)
	s.mux.HandleFunc("GET /{path...}", s.handleNotFoundPage)
}`},
			want: `"GET /api/probe" is registered directly in registerRoutes`,
		},
		{
			name: "route registered from a helper function in another file",
			srcs: []string{good, `package server
func (s *Server) registerDebugRoutes() {
	s.mux.HandleFunc("GET /api/probe", s.handleProbe)
}`},
			want: "route registered outside registerRoutes",
		},
		{
			name: "route smuggled into the routes() loop body",
			srcs: []string{`package server
func (s *Server) registerRoutes() {
	for _, rt := range s.routes() {
		s.mux.HandleFunc(rt.method+" "+rt.pattern, rt.handler)
		s.mux.HandleFunc("GET /api/probe", s.handleProbe)
	}
}`},
			want: "a second registration inside the routes() loop",
		},
		{
			name: "routes() loop that registers nothing",
			srcs: []string{`package server
func (s *Server) registerRoutes() {
	for _, rt := range s.routes() {
		_ = rt
	}
}`},
			want: "the routes() loop registers nothing",
		},
		{
			name: "routes table no longer looped over",
			srcs: []string{`package server
func (s *Server) registerRoutes() {
	s.mux.HandleFunc("GET /api/libraries", s.handleLibraries)
}`},
			want: "registerRoutes does not loop over routes()",
		},
		{
			name: "no registerRoutes at all",
			srcs: []string{`package server`},
			want: "no registerRoutes function found",
		},
		{
			name: "slog handler is not mistaken for a registration",
			srcs: []string{good, `package server
func (h *logHandler) log(ctx context.Context, rec slog.Record) error {
	return h.inner.Handle(ctx, rec)
}`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertProblem(t, checkRegisterRoutes(parseFixture(t, tt.srcs...)), tt.want)
		})
	}
}

// assertProblem requires that want (a substring) is reported, or that nothing is
// reported when want is empty.
func assertProblem(t *testing.T, problems []string, want string) {
	t.Helper()

	if want == "" {
		if len(problems) != 0 {
			t.Errorf("expected no problems, got %v", problems)
		}
		return
	}
	for _, p := range problems {
		if strings.Contains(p, want) {
			return
		}
	}
	t.Errorf("expected a problem containing %q, got %v", want, problems)
}
