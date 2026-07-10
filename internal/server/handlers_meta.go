package server

import (
	"net/http"

	"github.com/semsemyonoff/ALTO/internal/version"
)

// handleVersion reports the running ALTO release version, mirroring the
// beetDeck backend's GET /api/version. The value is bare semver (or the dev
// sentinel "0.0.0"); `display` is the header-badge form ("v2.4.1" or "dev").
// GET /api/version
func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"version": version.Resolve(),
		"display": version.Display(),
		"dev":     version.IsDev(),
		"ffmpeg":  s.cfg.FFmpegVersion,
	})
}
