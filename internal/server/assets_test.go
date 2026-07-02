package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeViteManifest(t *testing.T, staticDir string, manifest viteManifest) {
	t.Helper()
	path := viteManifestPath(staticDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("Marshal manifest: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("WriteFile manifest: %v", err)
	}
}

func TestNewProdAssetResolver_ManifestParsesToHashedTags(t *testing.T) {
	staticDir := t.TempDir()
	writeViteManifest(t, staticDir, viteManifest{
		"src/main.ts": {File: "assets/main-abc123.js", CSS: []string{"assets/main-def456.css"}},
	})

	resolver := newProdAssetResolver(staticDir)
	tags, err := resolver.Tags("src/main.ts")
	if err != nil {
		t.Fatalf("Tags: %v", err)
	}
	if !strings.Contains(string(tags), `href="/static/dist/assets/main-def456.css"`) {
		t.Errorf("tags should reference hashed CSS; got:\n%s", tags)
	}
	if !strings.Contains(string(tags), `src="/static/dist/assets/main-abc123.js"`) {
		t.Errorf("tags should reference hashed JS entry; got:\n%s", tags)
	}
}

func TestNewProdAssetResolver_UnknownEntryErrors(t *testing.T) {
	staticDir := t.TempDir()
	writeViteManifest(t, staticDir, viteManifest{
		"src/main.ts": {File: "assets/main-abc123.js"},
	})

	resolver := newProdAssetResolver(staticDir)
	if _, err := resolver.Tags("src/missing.ts"); err == nil {
		t.Error("Tags for an unknown manifest entry should error")
	}
}

func TestNewProdAssetResolver_MissingManifestFailsLoudly(t *testing.T) {
	staticDir := t.TempDir() // no dist/.vite/manifest.json written

	resolver := newProdAssetResolver(staticDir)
	if _, err := resolver.Tags("src/main.ts"); err == nil {
		t.Error("Tags should fail loudly when the manifest is missing")
	}
	if _, err := resolver.Asset("logo.svg"); err == nil {
		t.Error("Asset should fail loudly when the manifest is missing")
	}
}

func TestNewDevAssetResolver_EmitsViteClient(t *testing.T) {
	resolver := newDevAssetResolver("")
	tags, err := resolver.Tags("src/main.ts")
	if err != nil {
		t.Fatalf("Tags: %v", err)
	}
	if !strings.Contains(string(tags), viteDevDefaultOrigin+"/@vite/client") {
		t.Errorf("dev tags should include the @vite/client script; got:\n%s", tags)
	}
	if !strings.Contains(string(tags), viteDevDefaultOrigin+"/src/main.ts") {
		t.Errorf("dev tags should include the entry module; got:\n%s", tags)
	}
}

func TestNewDevAssetResolver_CustomOrigin(t *testing.T) {
	resolver := newDevAssetResolver("http://example.internal:9999")
	tags, err := resolver.Tags("src/main.ts")
	if err != nil {
		t.Fatalf("Tags: %v", err)
	}
	if !strings.Contains(string(tags), "http://example.internal:9999/@vite/client") {
		t.Errorf("dev tags should honor a custom origin; got:\n%s", tags)
	}
}

func TestNewTestStubAssetResolver_RendersPlaceholder(t *testing.T) {
	resolver := newTestStubAssetResolver()
	tags, err := resolver.Tags("src/main.ts")
	if err != nil {
		t.Fatalf("Tags: %v", err)
	}
	if !strings.Contains(string(tags), "stubbed") {
		t.Errorf("stub tags should be an obvious placeholder; got:\n%s", tags)
	}
	if _, err := resolver.Asset("logo.svg"); err != nil {
		t.Fatalf("Asset: %v", err)
	}
}

func TestAssetResolver_ZeroValueIsTestStub(t *testing.T) {
	var resolver assetResolver
	tags, err := resolver.Tags("src/main.ts")
	if err != nil {
		t.Fatalf("Tags: %v", err)
	}
	if !strings.Contains(string(tags), "stubbed") {
		t.Errorf("zero-value resolver should behave as test-stub; got:\n%s", tags)
	}
}

func TestNewAssetResolver_DevEnvTakesPriority(t *testing.T) {
	staticDir := t.TempDir()
	writeViteManifest(t, staticDir, viteManifest{"src/main.ts": {File: "assets/main.js"}})
	t.Setenv("ALTO_VITE_DEV", "http://localhost:1234")

	resolver := newAssetResolver(staticDir)
	if resolver.mode != assetModeDev {
		t.Fatalf("want dev mode when ALTO_VITE_DEV is set, got mode %d", resolver.mode)
	}
	if resolver.devOrigin != "http://localhost:1234" {
		t.Errorf("want dev origin from env, got %q", resolver.devOrigin)
	}
}

func TestNewAssetResolver_TruthyDevEnvUsesDefaultOrigin(t *testing.T) {
	t.Setenv("ALTO_VITE_DEV", "true")
	resolver := newAssetResolver(t.TempDir())
	if resolver.mode != assetModeDev || resolver.devOrigin != viteDevDefaultOrigin {
		t.Fatalf("want default dev origin, got mode=%d origin=%q", resolver.mode, resolver.devOrigin)
	}
}

func TestNewAssetResolver_PresentManifestSelectsProd(t *testing.T) {
	staticDir := t.TempDir()
	writeViteManifest(t, staticDir, viteManifest{"src/main.ts": {File: "assets/main.js"}})

	resolver := newAssetResolver(staticDir)
	if resolver.mode != assetModeProd {
		t.Fatalf("want prod mode when a manifest is present, got mode %d", resolver.mode)
	}
	if resolver.err != nil {
		t.Errorf("unexpected resolver error: %v", resolver.err)
	}
}

func TestNewAssetResolver_DefaultsToTestStubUnderGoTest(t *testing.T) {
	// No ALTO_VITE_DEV, no manifest: since this runs inside `go test`,
	// testing.Testing() is true and the resolver must fall back to the stub
	// rather than the loud-failure prod branch.
	resolver := newAssetResolver(t.TempDir())
	if resolver.mode != assetModeTestStub {
		t.Fatalf("want test-stub mode under go test with no manifest/env, got mode %d", resolver.mode)
	}
}
