package pixieset

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

type wireDashboardResponse struct {
	Data wireDashboardData `json:"data"`
}

type wireDashboardData struct {
	Data wireCollectionData `json:"data"`
	Meta wireMeta           `json:"meta"`
}

type wireCollectionData struct {
	Collections []wireCollection `json:"collections"`
}

type wireMeta struct {
	CurrentPage wireInt `json:"current_page"`
	LastPage    wireInt `json:"last_page"`
}

type wireSetListResponse struct {
	Data []wireSet `json:"data"`
}

type wireSetResponse struct {
	Data wireSet `json:"data"`
}

type wireCollection struct {
	ID          wireID          `json:"id"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	PhotoCount  wireInt         `json:"photo_count"`
	VideoCount  wireInt         `json:"video_count"`
	EventDate   json.RawMessage `json:"event_date"`
	CreatedAt   json.RawMessage `json:"create_at"`
}

type wireSet struct {
	ID           wireID            `json:"id"`
	CollectionID wireID            `json:"collection_id"`
	Name         string            `json:"name"`
	Description  string            `json:"description"`
	PhotoCount   wireInt           `json:"photo_count"`
	VideoCount   wireInt           `json:"video_count"`
	Rank         wireInt           `json:"rank"`
	Photos       []wirePhoto  `json:"photos"`
	Videos       []wireVideo  `json:"videos"`
}

type wirePhoto struct {
	ID           wireID          `json:"id"`
	CollectionID wireID          `json:"collection_id"`
	GalleryID    wireID          `json:"gallery_id"`
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	MIMEType     string          `json:"mime_type"`
	Extension    string          `json:"ext"`
	Size         wireInt         `json:"size"`
	Width        wireInt         `json:"width"`
	Height       wireInt         `json:"height"`
	Rank         wireInt         `json:"rank"`
	CaptureDate  json.RawMessage `json:"capture_date"`
	PathXXLarge  string          `json:"path_xxlarge"`
	PathXLarge   string          `json:"path_xlarge"`
	PathLarge    string          `json:"path_large"`
	PathMedium   string          `json:"path_medium"`
}

type wireVideo struct {
	ID          wireID  `json:"id"`
	ProviderID  wireInt `json:"provider_id"`
	Name        string  `json:"name"`
	Width       wireInt `json:"width"`
	Height      wireInt `json:"height"`
	MuxStatus   wireInt `json:"mux_status"`
	Metadata    string  `json:"metadata"`
	VideoSource string  `json:"video_source"`
	Rank        wireInt `json:"rank"`
	raw         []byte
}

// UnmarshalJSON decodes a video record leniently. The raw bytes are always
// retained so the fail-closed diagnostic still has something to write; if the
// inner decode fails, the fields are left zeroed and nil is returned, and
// normalizeVideo rejects the record.
func (video *wireVideo) UnmarshalJSON(data []byte) error {
	video.raw = append([]byte(nil), data...)
	var decoded struct {
		ID          wireID  `json:"id"`
		ProviderID  wireInt `json:"provider_id"`
		Name        string  `json:"name"`
		Width       wireInt `json:"width"`
		Height      wireInt `json:"height"`
		MuxStatus   wireInt `json:"mux_status"`
		Metadata    string  `json:"metadata"`
		VideoSource string  `json:"video_source"`
		Rank        wireInt `json:"rank"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return nil
	}
	video.ID = decoded.ID
	video.ProviderID = decoded.ProviderID
	video.Name = decoded.Name
	video.Width = decoded.Width
	video.Height = decoded.Height
	video.MuxStatus = decoded.MuxStatus
	video.Metadata = decoded.Metadata
	video.VideoSource = decoded.VideoSource
	video.Rank = decoded.Rank
	return nil
}

type wireID struct {
	value   string
	present bool
}

func (id *wireID) UnmarshalJSON(data []byte) error {
	id.present = true
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "null" || trimmed == "" {
		return nil
	}
	if trimmed[0] == '"' {
		var value string
		if err := json.Unmarshal(data, &value); err != nil {
			return errors.New("ID is malformed")
		}
		id.value = value
		return nil
	}
	if len(trimmed) > 20 {
		return errors.New("ID is malformed")
	}
	for _, character := range trimmed {
		if character < '0' || character > '9' {
			return errors.New("ID is malformed")
		}
	}
	id.value = trimmed
	return nil
}

type wireInt struct {
	value   int64
	present bool
	null    bool
}

func (number *wireInt) UnmarshalJSON(data []byte) error {
	number.present = true
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "null" {
		number.null = true
		return nil
	}
	if trimmed == "" {
		return errors.New("integer is malformed")
	}
	if trimmed[0] == '"' {
		var value string
		if err := json.Unmarshal(data, &value); err != nil {
			return errors.New("integer is malformed")
		}
		trimmed = value
	}
	value, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil {
		return errors.New("integer is malformed")
	}
	number.value = value
	return nil
}

func decodeDashboard(body []byte) (wireDashboardData, error) {
	var response wireDashboardResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return wireDashboardData{}, fmt.Errorf("decode dashboard response: %w", err)
	}
	if response.Data.Data.Collections == nil {
		return wireDashboardData{}, errors.New("dashboard collections are missing")
	}
	return response.Data, nil
}

func decodeSetList(body []byte) (wireSetListResponse, error) {
	var response wireSetListResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return wireSetListResponse{}, fmt.Errorf("decode Set list response: %w", err)
	}
	if response.Data == nil {
		return wireSetListResponse{}, errors.New("Set list is missing")
	}
	return response, nil
}

func decodeSet(body []byte) (wireSetResponse, error) {
	var response wireSetResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return wireSetResponse{}, fmt.Errorf("decode Set response: %w", err)
	}
	return response, nil
}
