package media

import (
	"encoding/json"
	"path/filepath"
	"strings"
)

var extensions = func() map[string]bool {
	var catalog []struct {
		Extensions []string `json:"extensions"`
	}
	if err := json.Unmarshal([]byte(FormatCatalogJSON), &catalog); err != nil {
		panic(err)
	}
	out := map[string]bool{}
	for _, format := range catalog {
		for _, ext := range format.Extensions {
			out["."+strings.ToLower(ext)] = true
		}
	}
	return out
}()

func KnownExtension(path string) bool { return extensions[strings.ToLower(filepath.Ext(path))] }

// Text/subtitle formats may be demuxable but are not standalone audio/video.
// These still get probed: the hint only controls how an unsuccessful directory probe is reported.
func expectedMediaExtension(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".txt", ".srt", ".vtt", ".ass", ".ssa", ".sub", ".smi", ".sami", ".lrc":
		return false
	}
	return KnownExtension(path)
}

// ExpectedMediaExtension controls errors during directory scans; explicit files are always probed.
func ExpectedMediaExtension(path string) bool { return expectedMediaExtension(path) }
