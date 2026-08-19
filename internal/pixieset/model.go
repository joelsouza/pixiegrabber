package pixieset

import "time"

// Collection is the normalized summary returned by dashboard discovery.
type Collection struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Description string     `json:"description"`
	PhotoCount  int        `json:"photo_count"`
	VideoCount  int        `json:"video_count"`
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
	Videos       []Video `json:"videos,omitempty"`
	videos       [][]byte
	unrecognized [][]byte
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

// Video is a normalized Mux video record. Variants are the MP4 renditions,
// ordered by descending width. URL is transient source data and is not
// serialized with normalized models.
type Video struct {
	ID              string         `json:"id"`
	CollectionID    string         `json:"collection_id"`
	SetID           string         `json:"set_id"`
	Name            string         `json:"name"`
	Width           int            `json:"width"`
	Height          int            `json:"height"`
	DurationSeconds float64        `json:"duration_seconds,omitempty"`
	MIMEType        string         `json:"mime_type"`
	Extension       string         `json:"ext"`
	Size            int64          `json:"size"`
	Rank            int            `json:"rank"`
	MuxStatus       int            `json:"-"`
	Variants        []ImageVariant `json:"image_variants,omitempty"`
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

// FirstUnrecognizedVideo returns a copy of the first raw video object that
// could not be normalized, for diagnostics.
func (s Set) FirstUnrecognizedVideo() ([]byte, bool) {
	if len(s.unrecognized) == 0 {
		return nil, false
	}
	return append([]byte(nil), s.unrecognized[0]...), true
}
