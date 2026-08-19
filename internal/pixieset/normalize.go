package pixieset

import (
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const maxStringBytes = 4096

func normalizeID(value string) (string, error) {
	if len(value) == 0 || len(value) > 20 {
		return "", errors.New("ID must contain 1 to 20 ASCII digits")
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return "", errors.New("ID must contain 1 to 20 ASCII digits")
		}
	}
	return value, nil
}

func normalizeCollection(item wireCollection) (Collection, error) {
	id, err := sourceID(item.ID, "collection")
	if err != nil {
		return Collection{}, err
	}
	if err := requiredString(item.Name, "Collection name"); err != nil {
		return Collection{}, err
	}
	if err := boundedString(item.Description, "Collection description"); err != nil {
		return Collection{}, err
	}
	photoCount, err := requiredInteger(item.PhotoCount, "Collection photo count", false)
	if err != nil {
		return Collection{}, err
	}
	videoCount, err := requiredInteger(item.VideoCount, "Collection video count", false)
	if err != nil {
		return Collection{}, err
	}
	eventDate, err := nullableDate(item.EventDate)
	if err != nil {
		return Collection{}, fmt.Errorf("Collection event date: %w", err)
	}
	createdAt, err := nullableDate(item.CreatedAt)
	if err != nil {
		return Collection{}, fmt.Errorf("Collection create date: %w", err)
	}
	return Collection{ID: id, Name: item.Name, Description: item.Description, PhotoCount: int(photoCount), VideoCount: int(videoCount), EventDate: eventDate, CreatedAt: createdAt}, nil
}

func normalizeSet(item wireSet, expectedCollection, expectedSet string, content bool, baseURL *url.URL) (Set, error) {
	id, err := sourceID(item.ID, "Set")
	if err != nil {
		return Set{}, err
	}
	collectionID, err := sourceID(item.CollectionID, "Collection")
	if err != nil {
		return Set{}, err
	}
	if expectedCollection != "" && collectionID != expectedCollection {
		return Set{}, fmt.Errorf("Set %s has the wrong Collection relationship", id)
	}
	if expectedSet != "" && id != expectedSet {
		return Set{}, fmt.Errorf("response Set ID is %s", id)
	}
	if err := requiredString(item.Name, "Set name"); err != nil {
		return Set{}, err
	}
	if err := boundedString(item.Description, "Set description"); err != nil {
		return Set{}, err
	}
	photoCount, err := requiredInteger(item.PhotoCount, "Set photo count", false)
	if err != nil {
		return Set{}, err
	}
	// The Set list sends a video count, but the single-Set response does not.
	// That response carries the videos themselves, so they give the count.
	videoCount, present, err := integerField(item.VideoCount, "Set video count", false)
	if err != nil {
		return Set{}, err
	}
	if !present && content {
		videoCount = int64(len(item.Videos))
	}
	// Pixieset does not send a Set rank. The order of the Sets in the response
	// gives the display order instead, and ListSets supplies it.
	rank, _, err := integerField(item.Rank, "Set rank", false)
	if err != nil {
		return Set{}, err
	}
	set := Set{ID: id, CollectionID: collectionID, Name: item.Name, Description: item.Description, PhotoCount: int(photoCount), VideoCount: int(videoCount), Rank: int(rank)}
	if !content {
		return set, nil
	}
	if item.Photos == nil {
		return Set{}, errors.New("Set photos are missing")
	}
	if int64(len(item.Photos)) != photoCount {
		return Set{}, errors.New("photo count does not match returned photos")
	}
	seen := make(map[string]struct{}, len(item.Photos))
	set.Photos = make([]Photo, 0, len(item.Photos))
	for index, item := range item.Photos {
		photo, err := normalizePhoto(item, collectionID, id, baseURL)
		if err != nil {
			return Set{}, fmt.Errorf("photo %d: %w", index, err)
		}
		if _, exists := seen[photo.ID]; exists {
			return Set{}, fmt.Errorf("duplicate photo ID %s", photo.ID)
		}
		seen[photo.ID] = struct{}{}
		set.Photos = append(set.Photos, photo)
	}
	for _, video := range item.Videos {
		set.videos = append(set.videos, append([]byte(nil), video.raw...))
		normalized, err := normalizeVideo(video, collectionID, id, baseURL)
		if err != nil {
			set.unrecognized = append(set.unrecognized, append([]byte(nil), video.raw...))
			continue
		}
		set.Videos = append(set.Videos, normalized)
	}
	return set, nil
}

func normalizePhoto(item wirePhoto, expectedCollection, expectedSet string, baseURL *url.URL) (Photo, error) {
	id, err := sourceID(item.ID, "photo")
	if err != nil {
		return Photo{}, err
	}
	collectionID, err := sourceID(item.CollectionID, "photo Collection")
	if err != nil {
		return Photo{}, err
	}
	setID, err := sourceID(item.GalleryID, "Set")
	if err != nil {
		return Photo{}, err
	}
	if collectionID != expectedCollection || setID != expectedSet {
		return Photo{}, fmt.Errorf("photo %s has an invalid relationship", id)
	}
	if err := requiredString(item.Name, "photo name"); err != nil {
		return Photo{}, err
	}
	if err := boundedString(item.Description, "photo description"); err != nil {
		return Photo{}, err
	}
	if err := imageMIME(item.MIMEType); err != nil {
		return Photo{}, err
	}
	if err := extension(item.Extension); err != nil {
		return Photo{}, err
	}
	size, err := requiredInteger(item.Size, "photo size", true)
	if err != nil {
		return Photo{}, err
	}
	width, err := requiredInteger(item.Width, "photo width", true)
	if err != nil {
		return Photo{}, err
	}
	height, err := requiredInteger(item.Height, "photo height", true)
	if err != nil {
		return Photo{}, err
	}
	rank, err := requiredInteger(item.Rank, "photo rank", false)
	if err != nil {
		return Photo{}, err
	}
	captureDate, err := nullableDate(item.CaptureDate)
	if err != nil {
		return Photo{}, fmt.Errorf("photo capture date: %w", err)
	}
	variants, err := normalizeVariants(item, baseURL)
	if err != nil {
		return Photo{}, err
	}
	return Photo{ID: id, CollectionID: collectionID, SetID: setID, Name: item.Name, Description: item.Description, MIMEType: item.MIMEType, Extension: item.Extension, Size: size, Width: int(width), Height: int(height), Rank: int(rank), CaptureDate: captureDate, ImageVariants: variants}, nil
}

func normalizeVideo(item wireVideo, expectedCollection, expectedSet string, baseURL *url.URL) (Video, error) {
	id, err := sourceID(item.ID, "video")
	if err != nil {
		return Video{}, err
	}
	if item.ProviderID.present && item.ProviderID.value != muxProviderID {
		return Video{}, errors.New("video provider is unsupported")
	}
	if err := requiredString(item.Name, "video name"); err != nil {
		return Video{}, err
	}
	muxStatus, err := requiredInteger(item.MuxStatus, "video status", false)
	if err != nil {
		return Video{}, err
	}
	width, _, err := integerField(item.Width, "video width", false)
	if err != nil {
		return Video{}, err
	}
	height, _, err := integerField(item.Height, "video height", false)
	if err != nil {
		return Video{}, err
	}
	rank, _, err := integerField(item.Rank, "video rank", false)
	if err != nil {
		return Video{}, err
	}
	var asset muxAsset
	if item.Metadata != "" {
		parsed, err := parseMuxMetadata(item.Metadata)
		if err != nil {
			return Video{}, err
		}
		asset = parsed
	}
	variants := make([]ImageVariant, 0)
	bestSize := int64(0)
	if item.VideoSource != "" {
		validated, err := normalizeVideoURL(item.VideoSource, baseURL)
		if err != nil {
			return Video{}, err
		}
		parsed, err := url.Parse(validated)
		if err != nil {
			return Video{}, err
		}
		path := parsed.Path
		playbackID := path[1 : len(path)-len(".m3u8")]
		token := parsed.Query().Get("token")
		type pair struct {
			file    muxFile
			variant ImageVariant
		}
		pairs := make([]pair, 0, len(asset.StaticRenditions.Files))
		for _, file := range asset.StaticRenditions.Files {
			if !validRenditionName(file.Name) {
				return Video{}, errors.New("video rendition name is invalid")
			}
			query := url.Values{}
			query.Set("token", token)
			pairs = append(pairs, pair{
				file: file,
				variant: ImageVariant{
					Quality: file.Name[:len(file.Name)-len(".mp4")],
					URL:     fmt.Sprintf("https://stream.mux.com/%s/%s?%s", playbackID, file.Name, query.Encode()),
				},
			})
		}
		sort.Slice(pairs, func(i, j int) bool { return pairs[i].file.Width > pairs[j].file.Width })
		for _, p := range pairs {
			variants = append(variants, p.variant)
		}
		if len(pairs) > 0 {
			bestSize = pairs[0].file.FileSize
		}
	}
	return Video{ID: id, CollectionID: expectedCollection, SetID: expectedSet, Name: item.Name, Width: int(width), Height: int(height), DurationSeconds: asset.Duration, MIMEType: "video/mp4", Extension: "mp4", Size: bestSize, Rank: int(rank), MuxStatus: int(muxStatus), Variants: variants}, nil
}

func normalizeVideoURL(value string, baseURL *url.URL) (string, error) {
	if len(value) > maxStringBytes {
		return "", errors.New("video URL is too long")
	}
	if strings.HasPrefix(value, "//") {
		value = "https:" + value
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil {
		return "", errors.New("video URL is invalid")
	}
	localFixture := baseURL != nil && isLoopbackHost(baseURL.Hostname()) && isLoopbackHost(parsed.Hostname()) && strings.EqualFold(baseURL.Hostname(), parsed.Hostname())
	if !strings.EqualFold(parsed.Scheme, "https") && !localFixture {
		return "", errors.New("video URL must use HTTPS")
	}
	if !localFixture && parsed.Port() != "" && parsed.Port() != "443" {
		return "", errors.New("video URL has an invalid port")
	}
	if !localFixture && !strings.EqualFold(parsed.Hostname(), "stream.mux.com") {
		return "", errors.New("video URL host is invalid")
	}
	path := parsed.Path
	if !strings.HasPrefix(path, "/") || !strings.HasSuffix(path, ".m3u8") {
		return "", errors.New("video URL path is invalid")
	}
	segment := path[1 : len(path)-len(".m3u8")]
	if segment == "" || strings.Contains(segment, "/") {
		return "", errors.New("video URL path is invalid")
	}
	for _, character := range segment {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || character == '-' || character == '_' {
			continue
		}
		return "", errors.New("video URL path is invalid")
	}
	if strings.EqualFold(parsed.Scheme, "https") {
		parsed.Scheme = "https"
	}
	return parsed.String(), nil
}

func sourceID(value wireID, field string) (string, error) {
	if !value.present || value.value == "" {
		return "", fmt.Errorf("%s ID is missing", field)
	}
	id, err := normalizeID(value.value)
	if err != nil {
		return "", fmt.Errorf("invalid %s ID", field)
	}
	return id, nil
}

func requiredString(value, field string) error {
	if err := boundedString(value, field); err != nil {
		return err
	}
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s is required", field)
	}
	return nil
}

func boundedString(value, field string) error {
	if len(value) > maxStringBytes {
		return fmt.Errorf("%s is too long", field)
	}
	return nil
}

func integerField(value wireInt, field string, positive bool) (int64, bool, error) {
	if !value.present || value.null {
		return 0, false, nil
	}
	if value.value < 0 || (positive && value.value <= 0) {
		return 0, true, fmt.Errorf("%s must be positive", field)
	}
	if int64(int(value.value)) != value.value {
		return 0, true, fmt.Errorf("%s is too large", field)
	}
	return value.value, true, nil
}

func requiredInteger(value wireInt, field string, positive bool) (int64, error) {
	number, present, err := integerField(value, field, positive)
	if err != nil {
		return 0, err
	}
	if !present {
		return 0, fmt.Errorf("%s is missing", field)
	}
	return number, nil
}

func imageMIME(value string) error {
	if err := boundedString(value, "photo MIME type"); err != nil {
		return err
	}
	mediaType, _, err := mime.ParseMediaType(value)
	if err != nil || !strings.HasPrefix(strings.ToLower(mediaType), "image/") {
		return errors.New("photo MIME type must be an image type")
	}
	return nil
}

func extension(value string) error {
	if err := requiredString(value, "photo extension"); err != nil {
		return err
	}
	if strings.ContainsAny(value, `/\\`) {
		return errors.New("photo extension is invalid")
	}
	return nil
}

func normalizeVariants(item wirePhoto, baseURL *url.URL) ([]ImageVariant, error) {
	variants := make([]ImageVariant, 0, 4)
	for _, candidate := range []struct {
		quality string
		value   string
	}{
		{quality: "xxlarge", value: item.PathXXLarge},
		{quality: "xlarge", value: item.PathXLarge},
		{quality: "large", value: item.PathLarge},
		{quality: "medium", value: item.PathMedium},
	} {
		if candidate.value == "" {
			continue
		}
		value, err := normalizeMediaURL(candidate.value, baseURL)
		if err != nil {
			return nil, err
		}
		variants = append(variants, ImageVariant{Quality: candidate.quality, URL: value})
	}
	if len(variants) == 0 {
		return nil, errors.New("photo image variants are missing")
	}
	return variants, nil
}

func normalizeMediaURL(value string, baseURL *url.URL) (string, error) {
	if len(value) > maxStringBytes {
		return "", errors.New("media URL is too long")
	}
	if strings.HasPrefix(value, "//") {
		value = "https:" + value
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil {
		return "", errors.New("media URL is invalid")
	}
	localFixture := baseURL != nil && isLoopbackHost(baseURL.Hostname()) && isLoopbackHost(parsed.Hostname()) && strings.EqualFold(baseURL.Hostname(), parsed.Hostname())
	if !strings.EqualFold(parsed.Scheme, "https") && !localFixture {
		return "", errors.New("media URL must use HTTPS")
	}
	if !localFixture && parsed.Port() != "" && parsed.Port() != "443" {
		return "", errors.New("media URL has an invalid port")
	}
	if !localFixture && !strings.EqualFold(parsed.Hostname(), "images.pixieset.com") {
		return "", errors.New("media URL host is invalid")
	}
	if strings.EqualFold(parsed.Scheme, "https") {
		parsed.Scheme = "https"
	}
	return parsed.String(), nil
}

func nullableDate(raw json.RawMessage) (*time.Time, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	value := strings.TrimSpace(string(raw))
	if value == "" || value == "null" {
		return nil, nil
	}
	if len(value) > maxStringBytes {
		return nil, errors.New("date is too long")
	}
	if value == `""` {
		return nil, nil
	}
	if value[0] == '"' {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return nil, errors.New("date is invalid")
		}
		text = strings.TrimSpace(text)
		if isSentinelDate(text) {
			return nil, nil
		}
		for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999Z07:00", "2006-01-02 15:04:05.999999999", "2006-01-02 15:04:05", "2006-01-02"} {
			parsed, err := time.Parse(layout, text)
			if err == nil {
				if parsed.Year() <= 1 {
					return nil, nil
				}
				parsed = parsed.UTC()
				return &parsed, nil
			}
		}
		return nil, errors.New("date is invalid")
	}
	number, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return nil, errors.New("date is invalid")
	}
	if number <= 0 {
		return nil, nil
	}
	parsed := time.Unix(number, 0).UTC()
	return &parsed, nil
}

func isSentinelDate(value string) bool {
	if strings.HasPrefix(value, "0000-") {
		return true
	}
	if len(value) >= 6 && value[0] == '-' && value[5] == '-' {
		for _, character := range value[1:5] {
			if character < '0' || character > '9' {
				return false
			}
		}
		return true
	}
	return false
}
