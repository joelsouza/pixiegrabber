package pixieset

import (
	"encoding/json"
	"errors"
	"strings"
)

const maxVideoMetadataBytes = 64 << 10
const muxProviderID = 3
const muxStatusReady = 2
const muxStatusTimedOut = 3
const maxVideoFiles = 16

// muxAsset is the Mux Asset object Pixieset embeds, double-encoded, in a
// video record's metadata field. Only the fields the planner uses are decoded.
type muxAsset struct {
	Status           string              `json:"status"`
	Duration         float64             `json:"duration"`
	MP4Support       string              `json:"mp4_support"`
	StaticRenditions muxStaticRenditions `json:"static_renditions"`
}

type muxStaticRenditions struct {
	Status string    `json:"status"`
	Files  []muxFile `json:"files"`
}

type muxFile struct {
	Name     string `json:"name"`
	Ext      string `json:"ext"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	FileSize int64  `json:"filesize"`
}

// parseMuxMetadata decodes the double-encoded Mux Asset object. The wire sends
// a JSON string whose content is the Asset object, so raw is parsed as JSON.
func parseMuxMetadata(raw string) (muxAsset, error) {
	if len(raw) > maxVideoMetadataBytes {
		return muxAsset{}, errors.New("video metadata is too large")
	}
	var asset muxAsset
	if err := json.Unmarshal([]byte(raw), &asset); err != nil {
		return muxAsset{}, errors.New("video metadata is malformed")
	}
	if len(asset.StaticRenditions.Files) > maxVideoFiles {
		return muxAsset{}, errors.New("video metadata has too many renditions")
	}
	return asset, nil
}

// validRenditionName reports whether name is a safe URL path segment: a
// non-empty base of lowercase ASCII letters, digits, '-' and '_', ending in
// ".mp4". The base becomes part of a URL path, so anything else is rejected.
func validRenditionName(name string) bool {
	if !strings.HasSuffix(name, ".mp4") {
		return false
	}
	base := name[:len(name)-len(".mp4")]
	if base == "" {
		return false
	}
	for _, character := range base {
		if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') || character == '-' || character == '_' {
			continue
		}
		return false
	}
	return true
}
