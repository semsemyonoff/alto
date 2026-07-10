// Package version exposes the running ALTO release version.
//
// The mechanism mirrors the beetDeck backend: the release version is baked into
// the production image at build time and falls back to a dev sentinel
// otherwise. Concretely, precedence is:
//
//  1. The APP_VERSION environment variable (set on the container image by the
//     deploy pipeline from the release tag — see build.sh / Dockerfile).
//  2. The compiled-in Version, overridable at build time via
//     -ldflags "-X github.com/semsemyonoff/ALTO/internal/version.Version=X.Y.Z".
//  3. The dev sentinel "0.0.0".
//
// The stored value is bare semver (no leading "v"); the "v" prefix is added only
// for display (see Display) and release URLs, matching beetDeck's convention.
package version

import "os"

// devSentinel is the version reported for a plain build with no release version
// injected (local dev, `go run`, `docker build` without the build-arg).
const devSentinel = "0.0.0"

// Version is the compiled-in release version. Overridable at build time via
// -ldflags "-X github.com/semsemyonoff/ALTO/internal/version.Version=X.Y.Z".
var Version = devSentinel

// Resolve returns the effective bare-semver version string, preferring the
// APP_VERSION environment variable (set on production images) over the
// compiled-in Version.
func Resolve() string {
	if v := os.Getenv("APP_VERSION"); v != "" {
		return v
	}
	return Version
}

// IsDev reports whether the resolved version is the dev sentinel (no real
// release version was injected).
func IsDev() bool {
	v := Resolve()
	return v == "" || v == devSentinel
}

// Display returns the version formatted for the UI badge: "dev" when no release
// version was injected, otherwise the version with a leading "v" (e.g.
// "v2.4.1").
func Display() string {
	if IsDev() {
		return "dev"
	}
	return "v" + Resolve()
}
