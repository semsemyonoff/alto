package server

import (
	"os"
	"testing"
)

// TestMain isolates the package's tests from an ambient ALTO_VITE_DEV.
//
// The app runs inside a container that exports ALTO_VITE_DEV (see compose.yaml),
// and newAssetResolver treats any set value as dev mode. Without this baseline,
// tests that assume dev mode is OFF fail whenever the suite runs in that
// container (e.g. the pre-push hook's `dwe cmd app.test`). Tests that need dev
// mode set it explicitly via t.Setenv, which restores the empty baseline after.
func TestMain(m *testing.M) {
	_ = os.Unsetenv("ALTO_VITE_DEV")
	os.Exit(m.Run())
}
