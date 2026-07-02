package server

import (
	"encoding/json"
	"fmt"
	"html/template"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// assetMode selects how an assetResolver resolves frontend asset tags/URLs.
type assetMode int

const (
	// assetModeTestStub renders placeholder asset tags/URLs; it requires no
	// built frontend and is only ever selected automatically inside `go test`.
	assetModeTestStub assetMode = iota
	// assetModeDev points at a running Vite dev server for HMR.
	assetModeDev
	// assetModeProd resolves hashed asset URLs from a built Vite manifest.
	assetModeProd
)

// viteDevDefaultOrigin is used for dev mode when ALTO_VITE_DEV doesn't
// specify an explicit origin.
const viteDevDefaultOrigin = "http://localhost:5173"

// viteManifestEntry mirrors the subset of Vite's manifest.json fields ALTO needs.
type viteManifestEntry struct {
	File string   `json:"file"`
	CSS  []string `json:"css,omitempty"`
}

type viteManifest map[string]viteManifestEntry

// assetResolver resolves frontend asset tags/URLs for templates under one of
// three explicit modes: prod (parsed Vite manifest), dev (Vite dev server),
// or test-stub (placeholder, no build required). The zero value is
// assetModeTestStub, so a bare `templateEngine{dir: ...}` literal (as used by
// low-level template tests) behaves safely without a built frontend.
type assetResolver struct {
	mode      assetMode
	devOrigin string
	manifest  viteManifest
	err       error  // set when prod mode couldn't load/parse the manifest
	base      string // URL path prefix for built assets, e.g. "/static/dist/"
}

// viteManifestPath returns where the built Vite manifest is expected under staticDir.
func viteManifestPath(staticDir string) string {
	return filepath.Join(staticDir, "dist", ".vite", "manifest.json")
}

// loadViteManifest reads and parses the manifest at path.
func loadViteManifest(path string) (viteManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("vite manifest not found at %s (run `npm run build` in web/frontend): %w", path, err)
	}
	var m viteManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse vite manifest %s: %w", path, err)
	}
	return m, nil
}

// newProdAssetResolver builds a resolver backed by the built Vite manifest
// under staticDir. If the manifest is missing or invalid, the resolver still
// builds successfully but every Tags/Asset call fails loudly with that error.
func newProdAssetResolver(staticDir string) assetResolver {
	m, err := loadViteManifest(viteManifestPath(staticDir))
	return assetResolver{mode: assetModeProd, manifest: m, err: err, base: "/static/dist/"}
}

// newDevAssetResolver builds a resolver pointed at a Vite dev server. An
// empty origin falls back to viteDevDefaultOrigin.
func newDevAssetResolver(origin string) assetResolver {
	if origin == "" {
		origin = viteDevDefaultOrigin
	}
	return assetResolver{mode: assetModeDev, devOrigin: origin}
}

// newTestStubAssetResolver builds a resolver that renders placeholders,
// requiring no built frontend.
func newTestStubAssetResolver() assetResolver {
	return assetResolver{mode: assetModeTestStub}
}

// newAssetResolver picks the mode explicitly, in priority order:
//  1. ALTO_VITE_DEV set → dev mode (its value is the dev server origin;
//     "1"/"true" fall back to viteDevDefaultOrigin).
//  2. a built manifest present under staticDir → prod mode.
//  3. running inside a `go test` binary → test-stub mode, so tests don't
//     require a built frontend.
//  4. otherwise → prod mode with a missing manifest, which fails loudly the
//     first time a template asks for asset tags (a real deployment forgot to
//     build the frontend).
func newAssetResolver(staticDir string) assetResolver {
	if origin, ok := os.LookupEnv("ALTO_VITE_DEV"); ok {
		if origin == "1" || origin == "true" {
			origin = ""
		}
		return newDevAssetResolver(origin)
	}
	if _, err := os.Stat(viteManifestPath(staticDir)); err == nil {
		return newProdAssetResolver(staticDir)
	}
	if testing.Testing() {
		return newTestStubAssetResolver()
	}
	return newProdAssetResolver(staticDir)
}

// Tags returns the <script>/<link> head tags for a Vite entry (e.g. "src/main.ts").
func (a assetResolver) Tags(entry string) (template.HTML, error) {
	switch a.mode {
	case assetModeDev:
		return template.HTML(fmt.Sprintf(
			`<script type="module" src="%s/@vite/client"></script>`+"\n"+
				`<script type="module" src="%s/%s"></script>`,
			a.devOrigin, a.devOrigin, entry,
		)), nil
	case assetModeTestStub:
		return template.HTML("<!-- vite assets stubbed for tests -->"), nil
	case assetModeProd:
		if a.err != nil {
			return "", a.err
		}
		e, ok := a.manifest[entry]
		if !ok {
			return "", fmt.Errorf("vite manifest: entry %q not found", entry)
		}
		var b strings.Builder
		for _, css := range e.CSS {
			fmt.Fprintf(&b, `<link rel="stylesheet" href="%s%s">`+"\n", a.base, css)
		}
		fmt.Fprintf(&b, `<script type="module" src="%s%s"></script>`, a.base, e.File)
		return template.HTML(b.String()), nil
	default:
		return "", fmt.Errorf("asset resolver: unknown mode %d", a.mode)
	}
}

// Asset returns the hashed URL for a single built asset (e.g. an image
// processed by Vite).
func (a assetResolver) Asset(path string) (string, error) {
	switch a.mode {
	case assetModeDev:
		return fmt.Sprintf("%s/%s", a.devOrigin, path), nil
	case assetModeTestStub:
		return "/static/" + path, nil
	case assetModeProd:
		if a.err != nil {
			return "", a.err
		}
		e, ok := a.manifest[path]
		if !ok {
			return "", fmt.Errorf("vite manifest: asset %q not found", path)
		}
		return a.base + e.File, nil
	default:
		return "", fmt.Errorf("asset resolver: unknown mode %d", a.mode)
	}
}
