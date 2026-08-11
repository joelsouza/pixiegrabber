package pixieset

import "time"

// Collection is the normalized summary returned by dashboard discovery.
type Collection struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Description string     `json:"description"`
	PhotoCount  int        `json:"photo_count"`
	VideoCount  int        `json:"video_count"`
	Rank        int        `json:"rank"`
	EventDate   *time.Time `json:"event_date,omitempty"`
	CreatedAt   *time.Time `json:"create_at,omitempty"`
}

// Set is a named group of photos within a Collection. Photos are populated by
// GetSet; ListSets returns the summary fields only.
type Set struct {
	ID           string  `json:"id"`
	CollectionID string  `json:"collection_id"`
	Name         string  `json:"name"`
	Description  string  `json:"description"`
	PhotoCount   int     `json:"photo_count"`
	VideoCount   int     `json:"video_count"`
	Rank         int     `json:"rank"`
	Photos       []Photo `json:"photos,omitempty"`
	videos       [][]byte
}

// ImageVariant is one source image quality. URL is transient source data and
// is not serialized with normalized models.
type ImageVariant struct {
	Quality string `json:"quality"`
	URL     string `json:"-"`
}

// Photo is a normalized image record.
type Photo struct {
	ID            string         `json:"id"`
	CollectionID  string         `json:"collection_id"`
	SetID         string         `json:"set_id"`
	Name          string         `json:"name"`
	Description   string         `json:"description"`
	MIMEType      string         `json:"mime_type"`
	Extension     string         `json:"ext"`
	Size          int64          `json:"size"`
	Width         int            `json:"width"`
	Height        int            `json:"height"`
	Rank          int            `json:"rank"`
	CaptureDate   *time.Time     `json:"capture_date,omitempty"`
	ImageVariants []ImageVariant `json:"image_variants,omitempty"`
}

// HasVideos reports whether the Set contains unsupported video objects.
func (s Set) HasVideos() bool { return len(s.videos) != 0 }

// FirstVideo returns a copy of the first raw video object for diagnostics.
func (s Set) FirstVideo() ([]byte, bool) {
	if len(s.videos) == 0 {
		return nil, false
	}
	return append([]byte(nil), s.videos[0]...), true
}
