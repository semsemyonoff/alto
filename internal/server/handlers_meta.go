package server

import (
	"fmt"
	"net/http"

	"github.com/semsemyonoff/ALTO/internal/transcode"
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

// presetDTO is one built-in transcode preset in its full API shape.
type presetDTO struct {
	Name             string `json:"name"`
	Label            string `json:"label"`
	Codec            string `json:"codec"`
	CompressionLevel int    `json:"compression_level"`
	Bitrate          string `json:"bitrate"`
	CopyMetadata     bool   `json:"copy_metadata"`
	CopyCover        bool   `json:"copy_cover"`
	Default          bool   `json:"default"`
}

// presetsDTO is the GET /api/presets body: every built-in preset in display
// order, plus the codecs they target in first-appearance order.
type presetsDTO struct {
	Codecs  []string    `json:"codecs"`
	Presets []presetDTO `json:"presets"`
}

// presetLabel formats a display label for a preset's codec-specific parameter.
func presetLabel(p transcode.Preset) string {
	switch p.Codec {
	case transcode.CodecFLAC:
		return fmt.Sprintf("%s (compression %d)", p.Name, p.CompressionLevel)
	case transcode.CodecOpus:
		return fmt.Sprintf("%s (%s)", p.Name, p.Bitrate)
	default:
		return p.Name
	}
}

// presetIsDefault reports whether p is the pre-selected preset for its codec.
func presetIsDefault(p transcode.Preset) bool {
	switch p.Codec {
	case transcode.CodecFLAC:
		return p.Name == transcode.FLACBalanced.Name
	case transcode.CodecOpus:
		return p.Name == transcode.OpusMusicHigh.Name
	default:
		return false
	}
}

// buildPresets serialises transcode.DefaultPresets(). It is the single source
// for both GET /api/presets and the /dir page's inline tc-presets-data payload
// (see buildDockPresetsJSON), so the two can never describe different sets.
func buildPresets() presetsDTO {
	presets := transcode.DefaultPresets()
	out := presetsDTO{
		Codecs:  make([]string, 0, 2),
		Presets: make([]presetDTO, 0, len(presets)),
	}
	seen := make(map[string]struct{}, 2)
	for _, p := range presets {
		codec := string(p.Codec)
		if _, ok := seen[codec]; !ok {
			seen[codec] = struct{}{}
			out.Codecs = append(out.Codecs, codec)
		}
		out.Presets = append(out.Presets, presetDTO{
			Name:             p.Name,
			Label:            presetLabel(p),
			Codec:            codec,
			CompressionLevel: p.CompressionLevel,
			Bitrate:          p.Bitrate,
			CopyMetadata:     p.CopyMetadata,
			CopyCover:        p.CopyCover,
			Default:          presetIsDefault(p),
		})
	}
	return out
}

// handlePresets lists the built-in transcode presets so an API client can pick
// one without scraping the /dir page.
// GET /api/presets
func (s *Server) handlePresets(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, buildPresets())
}
