package archive

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"strconv"
	"strings"
	"unicode/utf8"

	"pixiegrabber/internal/pixieset"
	"pixiegrabber/internal/store"
)

// ErrUnsupportedVideo is returned only after a durable sanitized diagnostic
// write succeeds. Ordinary diagnostic validation and write failures do not
// unwrap to this sentinel.
var ErrUnsupportedVideo = errors.New("unsupported video")

const (
	// These limits apply before any diagnostic data is written. They keep a
	// source response from turning the diagnostic into an unbounded parser or
	// output sink.
	maxVideoDiagnosticInputBytes     = 4 << 20
	maxVideoDiagnosticOutputBytes    = 512 << 10
	maxVideoDiagnosticDepth          = 64
	maxVideoDiagnosticNodes          = 10000
	maxVideoDiagnosticFields         = 10000
	maxVideoDiagnosticArrayLength    = 1024
	maxVideoDiagnosticObjectKeyBytes = 256
	maxVideoDiagnosticTotalKeyBytes  = 64 << 10
	maxVideoDiagnosticEnumBytes      = 64
	maxVideoDiagnosticSchemaKeyBytes = 64

	// Media limits are deliberately generous but keep impossible source facts
	// out of the diagnostic contract.
	maxVideoDiagnosticDimension   int64   = 65_535
	maxVideoDiagnosticFileBytes   int64   = 1 << 40
	maxVideoDiagnosticRank        int64   = 1_000_000
	maxVideoDiagnosticBitrate     int64   = 1_000_000_000
	maxVideoDiagnosticDuration    float64 = 7 * 24 * 60 * 60
	maxVideoDiagnosticNumberLimit float64 = 1e308

	diagnosticFilename    = "pixiegrabber-unsupported-video.json"
	diagnosticPlaceholder = "[redacted]"
)

// UnsupportedVideoError is returned only after the sanitized diagnostic has
// been written. It contains no source video data.
type UnsupportedVideoError struct {
	DiagnosticPath string
}

// Path returns the only source-independent detail carried by this error.
func (e *UnsupportedVideoError) Path() string {
	if e == nil {
		return ""
	}
	return e.DiagnosticPath
}

func (e *UnsupportedVideoError) Error() string {
	if e == nil {
		return "video download is unsupported and images were not started"
	}
	return fmt.Sprintf("video download is unsupported and images were not started; diagnostic: %q", e.DiagnosticPath)
}

func (e *UnsupportedVideoError) Unwrap() error { return ErrUnsupportedVideo }

// CheckVideos stops archive work before image plans or downloads start when a
// Set contains an unsupported video. It writes one sanitized sample from the
// first such Set. Sets without videos do not touch the output root. The caller
// must hold the output-root lock; Task 6 calls this immediately after each
// GetSet, before it builds any plan or download work.
func CheckVideos(s store.Store, sets []pixieset.Set) error {
	for _, set := range sets {
		if !set.HasVideos() {
			continue
		}
		raw, ok := set.FirstVideo()
		if !ok {
			return errors.New("read unsupported video diagnostic: video data is unavailable")
		}
		path, err := writeVideoDiagnostic(s, raw)
		if err != nil {
			return err
		}
		return &UnsupportedVideoError{DiagnosticPath: path}
	}
	return nil
}

func writeVideoDiagnostic(s store.Store, raw []byte) (string, error) {
	value, err := sanitizeVideoJSON(raw)
	if err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return "", errors.New("marshal unsupported video diagnostic")
	}
	data = append(data, '\n')
	if len(data) > maxVideoDiagnosticOutputBytes {
		return "", errors.New("marshal unsupported video diagnostic: output is too large")
	}
	if err := s.Put(diagnosticFilename, bytes.NewReader(data), int64(len(data)), nil); err != nil {
		return "", err
	}
	path, err := s.DisplayPath(diagnosticFilename)
	if err != nil {
		return "", err
	}
	return path, nil
}

type diagnosticObject map[string]any

type diagnosticLimits struct {
	nodes         int
	fields        int
	totalKeyBytes int
}

func sanitizeVideoJSON(raw []byte) (any, error) {
	if len(raw) > maxVideoDiagnosticInputBytes {
		return nil, errors.New("sanitize unsupported video: input is too large")
	}
	if !utf8.Valid(raw) {
		return nil, errors.New("sanitize unsupported video: input is not valid UTF-8")
	}
	if len(raw) == 0 {
		return nil, errors.New("sanitize unsupported video: input is empty")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	limits := diagnosticLimits{}
	first, err := decoder.Token()
	if err != nil {
		return nil, errors.New("sanitize unsupported video: malformed JSON")
	}
	opening, ok := first.(json.Delim)
	if !ok || opening != '{' {
		return nil, errors.New("sanitize unsupported video: top level must be an object")
	}
	value, err := sanitizeDiagnosticObject(decoder, &limits, 1, true)
	if err != nil {
		return nil, err
	}
	if token, err := decoder.Token(); err != io.EOF {
		if err != nil {
			return nil, errors.New("sanitize unsupported video: malformed JSON")
		}
		_ = token
		return nil, errors.New("sanitize unsupported video: multiple JSON values")
	}
	return value, nil
}

func sanitizeDiagnosticObject(decoder *json.Decoder, limits *diagnosticLimits, depth int, allowFacts bool) (diagnosticObject, error) {
	if depth > maxVideoDiagnosticDepth {
		return nil, errors.New("sanitize unsupported video: JSON is too deep")
	}
	object := make(diagnosticObject)
	seenKeys := make(map[string]struct{})
	redactedKeys := make(map[string]struct{})
	nextRedacted := 1
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, errors.New("sanitize unsupported video: malformed JSON")
		}
		key, ok := keyToken.(string)
		if !ok || len(key) > maxVideoDiagnosticObjectKeyBytes || !utf8.ValidString(key) {
			return nil, errors.New("sanitize unsupported video: object key is invalid")
		}
		if _, duplicate := seenKeys[key]; duplicate {
			return nil, errors.New("sanitize unsupported video: duplicate object key")
		}
		seenKeys[key] = struct{}{}
		limits.fields++
		limits.totalKeyBytes += len(key)
		if limits.fields > maxVideoDiagnosticFields || limits.totalKeyBytes > maxVideoDiagnosticTotalKeyBytes {
			return nil, errors.New("sanitize unsupported video: object is too wide")
		}
		validKey := validDiagnosticSchemaKey(key)
		field := ""
		if allowFacts && validKey {
			field = key
		}
		value, err := sanitizeDiagnosticValue(decoder, limits, depth, field, allowFacts && validKey)
		if err != nil {
			return nil, err
		}
		outputKey := key
		if !validKey {
			outputKey = nextDiagnosticRedactedKey(object, &nextRedacted)
			redactedKeys[outputKey] = struct{}{}
		} else if _, collision := object[key]; collision {
			// A valid key can appear after an invalid key whose deterministic
			// placeholder used the same spelling. Preserve the valid schema key
			// and move the earlier redaction to a fresh placeholder.
			movedKey := nextDiagnosticRedactedKey(object, &nextRedacted)
			object[movedKey] = object[key]
			delete(object, key)
			delete(redactedKeys, key)
			redactedKeys[movedKey] = struct{}{}
		}
		object[outputKey] = value
	}
	closing, err := decoder.Token()
	if err != nil {
		return nil, errors.New("sanitize unsupported video: malformed JSON")
	}
	if delimiter, ok := closing.(json.Delim); !ok || delimiter != '}' {
		return nil, errors.New("sanitize unsupported video: malformed JSON")
	}
	return object, nil
}

func sanitizeDiagnosticArray(decoder *json.Decoder, limits *diagnosticLimits, depth int) ([]any, error) {
	if depth > maxVideoDiagnosticDepth {
		return nil, errors.New("sanitize unsupported video: JSON is too deep")
	}
	array := make([]any, 0)
	for decoder.More() {
		if len(array) >= maxVideoDiagnosticArrayLength {
			return nil, errors.New("sanitize unsupported video: array is too long")
		}
		value, err := sanitizeDiagnosticValue(decoder, limits, depth, "", false)
		if err != nil {
			return nil, err
		}
		array = append(array, value)
	}
	closing, err := decoder.Token()
	if err != nil {
		return nil, errors.New("sanitize unsupported video: malformed JSON")
	}
	if delimiter, ok := closing.(json.Delim); !ok || delimiter != ']' {
		return nil, errors.New("sanitize unsupported video: malformed JSON")
	}
	return array, nil
}

func sanitizeDiagnosticValue(decoder *json.Decoder, limits *diagnosticLimits, depth int, field string, allowFacts bool) (any, error) {
	limits.nodes++
	if limits.nodes > maxVideoDiagnosticNodes {
		return nil, errors.New("sanitize unsupported video: JSON has too many values")
	}
	token, err := decoder.Token()
	if err != nil {
		return nil, errors.New("sanitize unsupported video: malformed JSON")
	}
	switch value := token.(type) {
	case nil:
		return nil, nil
	case bool:
		return false, nil
	case string:
		if allowFacts {
			if canonical, ok := safeDiagnosticEnum(field, value); ok {
				return canonical, nil
			}
		}
		return diagnosticPlaceholder, nil
	case json.Number:
		if !allowFacts || !safeDiagnosticNumberField(field) {
			if err := validateDiagnosticNumber(value); err != nil {
				return nil, err
			}
			return int64(0), nil
		}
		return canonicalDiagnosticNumber(field, value)
	case json.Delim:
		switch value {
		case '{':
			return sanitizeDiagnosticObject(decoder, limits, depth+1, false)
		case '[':
			return sanitizeDiagnosticArray(decoder, limits, depth+1)
		default:
			return nil, errors.New("sanitize unsupported video: malformed JSON")
		}
	default:
		return nil, errors.New("sanitize unsupported video: malformed JSON")
	}
}

func validDiagnosticSchemaKey(key string) bool {
	if len(key) == 0 || len(key) > maxVideoDiagnosticSchemaKeyBytes || !utf8.ValidString(key) {
		return false
	}
	for index := 0; index < len(key); index++ {
		character := key[index]
		if index == 0 {
			if !isDiagnosticSchemaStart(character) {
				return false
			}
			continue
		}
		if !isDiagnosticSchemaStart(character) && (character < '0' || character > '9') {
			return false
		}
	}
	return true
}

func isDiagnosticSchemaStart(character byte) bool {
	return character == '_' || character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z'
}

func nextDiagnosticRedactedKey(object diagnosticObject, next *int) string {
	for {
		candidate := fmt.Sprintf("_redacted_field_%d", *next)
		*next++
		if _, exists := object[candidate]; !exists {
			return candidate
		}
	}
}

func validateDiagnosticNumber(number json.Number) error {
	value, err := strconv.ParseFloat(string(number), 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || math.Abs(value) > maxVideoDiagnosticNumberLimit {
		return errors.New("sanitize unsupported video: number is not finite")
	}
	return nil
}

func canonicalDiagnosticNumber(field string, number json.Number) (any, error) {
	field = strings.ToLower(field)
	if field == "duration" || field == "duration_seconds" {
		value, err := strconv.ParseFloat(string(number), 64)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > maxVideoDiagnosticDuration {
			return nil, errors.New("sanitize unsupported video: duration is impossible")
		}
		if value == 0 {
			return float64(0), nil
		}
		return value, nil
	}
	rat, ok := new(big.Rat).SetString(string(number))
	if !ok || !rat.IsInt() || !rat.Num().IsInt64() {
		return nil, errors.New("sanitize unsupported video: integer media fact is impossible")
	}
	value := rat.Num().Int64()
	minimum, maximum := diagnosticIntegerRange(field)
	if value < minimum || value > maximum {
		return nil, errors.New("sanitize unsupported video: integer media fact is impossible")
	}
	return value, nil
}

func diagnosticIntegerRange(field string) (int64, int64) {
	switch strings.ToLower(field) {
	case "width", "height":
		return 1, maxVideoDiagnosticDimension
	case "size", "file_size":
		return 0, maxVideoDiagnosticFileBytes
	case "rank":
		return 0, maxVideoDiagnosticRank
	case "bitrate":
		return 0, maxVideoDiagnosticBitrate
	default:
		return 0, 0
	}
}

var safeDiagnosticEnums = map[string]map[string]struct{}{
	"type": {
		"audio": {}, "image": {}, "media": {}, "movie": {}, "photo": {}, "video": {},
	},
	"kind": {
		"audio": {}, "image": {}, "media": {}, "movie": {}, "photo": {}, "video": {},
	},
	"mime_type": {
		"audio/mpeg": {}, "audio/mp4": {}, "audio/ogg": {}, "audio/webm": {},
		"image/jpeg": {}, "image/png": {}, "image/webp": {},
		"video/3gpp": {}, "video/mp2t": {}, "video/mp4": {}, "video/mpeg": {},
		"video/ogg": {}, "video/quicktime": {}, "video/webm": {}, "video/x-m4v": {},
		"video/x-matroska": {}, "video/x-msvideo": {},
	},
	"ext": {
		"3gp": {}, "avi": {}, "m4v": {}, "mkv": {}, "mov": {}, "mp4": {}, "mpg": {},
		"mpeg": {}, "m3u8": {}, "ogv": {}, "ts": {}, "webm": {},
	},
	"extension": {
		"3gp": {}, "avi": {}, "m4v": {}, "mkv": {}, "mov": {}, "mp4": {}, "mpg": {},
		"mpeg": {}, "m3u8": {}, "ogv": {}, "ts": {}, "webm": {},
	},
	"format": {
		"3gp": {}, "avi": {}, "dash": {}, "hls": {}, "m4v": {}, "mkv": {}, "mov": {},
		"mp4": {}, "mpg": {}, "mpeg": {}, "m3u8": {}, "ogv": {}, "ts": {}, "webm": {},
	},
	"codec": {
		"aac": {}, "av1": {}, "avc": {}, "h264": {}, "h265": {}, "hevc": {}, "mpeg-4": {},
		"mpeg4": {}, "opus": {}, "theora": {}, "vorbis": {}, "vp8": {}, "vp9": {},
	},
}

func safeDiagnosticEnum(field, value string) (string, bool) {
	allowed, ok := safeDiagnosticEnums[strings.ToLower(field)]
	if !ok || len(value) == 0 || len(value) > maxVideoDiagnosticEnumBytes || !utf8.ValidString(value) {
		return "", false
	}
	if fieldName := strings.ToLower(field); fieldName != "mime_type" {
		for _, character := range value {
			if character > 0x7f || character == '/' || character == '\\' || character == '?' || character == '#' || character == '@' || character == '=' || character <= 0x20 || character == 0x7f {
				return "", false
			}
		}
	}
	canonical := strings.ToLower(value)
	_, ok = allowed[canonical]
	return canonical, ok
}

func safeDiagnosticNumberField(field string) bool {
	switch strings.ToLower(field) {
	case "width", "height", "size", "file_size", "duration", "duration_seconds", "rank", "bitrate":
		return true
	default:
		return false
	}
}
