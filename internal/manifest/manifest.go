// Package manifest defines and persists the local Collection manifest.
package manifest

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"pixiegrabber/internal/store"
)

const (
	// CurrentSchemaVersion is the only manifest schema understood by this
	// package.
	CurrentSchemaVersion = 1
	ManifestFilename     = "collection.json"
	// MaxManifestBytes is the maximum size accepted by Load.
	MaxManifestBytes int64 = 64 << 20
)

var (
	// ErrNotFound distinguishes an absent manifest from an invalid one or an
	// inaccessible file.
	ErrNotFound = errors.New("manifest not found")
	// ErrMissing is a descriptive alias for ErrNotFound.
	ErrMissing = ErrNotFound
	// ErrUnknownSchema is returned when a manifest uses a version that this
	// package cannot safely interpret.
	ErrUnknownSchema = errors.New("unknown manifest schema version")
	// ErrInvalid is the root error for a manifest that fails validation.
	ErrInvalid = errors.New("invalid manifest")
)

// PresenceState records whether a source object is still present remotely.
type PresenceState string

const (
	PresenceUnknown PresenceState = "unknown"
	PresencePresent PresenceState = "present"
	PresenceMissing PresenceState = "missing"
)

// DownloadState records local download progress for a Reference or Placement.
type DownloadState string

const (
	DownloadPending  DownloadState = "pending"
	DownloadComplete DownloadState = "complete"
	DownloadFailed   DownloadState = "failed"
	// DownloadInProgress is a legacy transient value. Normalize converts it
	// to DownloadPending before validation.
	DownloadInProgress DownloadState = "in_progress"
	// DownloadSkipped is a legacy value and is not valid persisted state.
	DownloadSkipped DownloadState = "skipped"
)

// RunState records the last Collection run state.
type RunState string

const (
	RunIncomplete RunState = "incomplete"
	RunComplete   RunState = "complete"
	// RunRunning is a legacy transient value. Normalize converts it to
	// RunIncomplete before validation.
	RunRunning RunState = "running"
	// RunPending and RunFailed are legacy values and are not valid persisted
	// states.
	RunPending RunState = "pending"
	RunFailed  RunState = "failed"
)

// Manifest is the versioned, self-contained state for one Collection.
type Manifest struct {
	SchemaVersion int         `json:"schema_version"`
	Collection    Collection  `json:"collection"`
	Sets          []Set       `json:"sets"`
	References    []Reference `json:"references"`
}

// Collection contains normalized source metadata and run timestamps.
type Collection struct {
	ID              string        `json:"id"`
	Name            string        `json:"name"`
	Description     string        `json:"description,omitempty"`
	SourceCreated   *time.Time    `json:"source_created_at,omitempty"`
	SourceUpdated   *time.Time    `json:"source_updated_at,omitempty"`
	PresenceState   PresenceState `json:"presence_state"`
	RunState        RunState      `json:"run_state"`
	LastDiscoveryAt *time.Time    `json:"last_discovery_at,omitempty"`
	LastAttemptAt   *time.Time    `json:"last_attempt_at,omitempty"`
	LastSuccessAt   *time.Time    `json:"last_success_at,omitempty"`
	LastVerifiedAt  *time.Time    `json:"last_verified_at,omitempty"`
}

// Set contains one source Set and its local presence state.
type Set struct {
	ID            string        `json:"id"`
	Name          string        `json:"name"`
	Description   string        `json:"description,omitempty"`
	SourceOrder   int           `json:"source_order"`
	SourceCreated *time.Time    `json:"source_created_at,omitempty"`
	SourceUpdated *time.Time    `json:"source_updated_at,omitempty"`
	PresenceState PresenceState `json:"presence_state"`
}

// Reference is one Collection-scoped media record. A Reference can have more
// than one Placement when the same media appears in multiple Sets.
type Reference struct {
	ID               string        `json:"id"`
	Name             string        `json:"name"`
	Description      string        `json:"description,omitempty"`
	SourceOrder      int           `json:"source_order"`
	SourceCreated    *time.Time    `json:"source_created_at,omitempty"`
	SourceUpdated    *time.Time    `json:"source_updated_at,omitempty"`
	CapturedAt       *time.Time    `json:"captured_at,omitempty"`
	MediaType        string        `json:"media_type"`
	OriginalFilename string        `json:"original_filename,omitempty"`
	Width            int           `json:"width,omitempty"`
	Height           int           `json:"height,omitempty"`
	DurationSeconds  *float64      `json:"duration_seconds,omitempty"`
	MIMEType         string        `json:"mime_type,omitempty"`
	SelectedQuality  string        `json:"selected_quality,omitempty"`
	SHA256           string        `json:"sha256,omitempty"`
	PresenceState    PresenceState `json:"presence_state"`
	DownloadState    DownloadState `json:"download_state"`
	Failure          *Failure      `json:"failure,omitempty"`
	Placements       []Placement   `json:"placements"`
}

// Placement records one local path for a Reference in one Set.
type Placement struct {
	SetID           string        `json:"set_id"`
	Path            string        `json:"path"`
	InstalledSHA256 string        `json:"installed_sha256,omitempty"`
	PresenceState   PresenceState `json:"presence_state"`
	DownloadState   DownloadState `json:"download_state"`
	LastAttemptAt   *time.Time    `json:"last_attempt_at,omitempty"`
	LastSuccessAt   *time.Time    `json:"last_success_at,omitempty"`
	Failure         *Failure      `json:"failure,omitempty"`
}

// Failure is the sanitized information needed to resume a failed operation.
// It intentionally has no URL, request, credential, or source-payload field.
type Failure struct {
	Code    string     `json:"code"`
	Message string     `json:"message"`
	At      *time.Time `json:"at,omitempty"`
}

// Normalize converts all manifest timestamps to UTC. It also ensures a
// failure timestamp uses the same canonical representation.
func (m *Manifest) Normalize() {
	if m == nil {
		return
	}
	if m.Sets == nil {
		m.Sets = []Set{}
	}
	if m.References == nil {
		m.References = []Reference{}
	}
	m.Collection.SourceCreated = utc(m.Collection.SourceCreated)
	m.Collection.SourceUpdated = utc(m.Collection.SourceUpdated)
	m.Collection.LastDiscoveryAt = utc(m.Collection.LastDiscoveryAt)
	m.Collection.LastAttemptAt = utc(m.Collection.LastAttemptAt)
	m.Collection.LastSuccessAt = utc(m.Collection.LastSuccessAt)
	m.Collection.LastVerifiedAt = utc(m.Collection.LastVerifiedAt)
	if m.Collection.RunState == RunRunning {
		m.Collection.RunState = RunIncomplete
	}
	for i := range m.Sets {
		m.Sets[i].SourceCreated = utc(m.Sets[i].SourceCreated)
		m.Sets[i].SourceUpdated = utc(m.Sets[i].SourceUpdated)
	}
	for i := range m.References {
		r := &m.References[i]
		if r.Placements == nil {
			r.Placements = []Placement{}
		}
		r.SourceCreated = utc(r.SourceCreated)
		r.SourceUpdated = utc(r.SourceUpdated)
		r.CapturedAt = utc(r.CapturedAt)
		if r.DownloadState == DownloadInProgress {
			r.DownloadState = DownloadPending
		}
		if r.Failure != nil {
			r.Failure.At = utc(r.Failure.At)
		}
		for j := range r.Placements {
			p := &r.Placements[j]
			if p.DownloadState == DownloadInProgress {
				p.DownloadState = DownloadPending
			}
			p.LastAttemptAt = utc(p.LastAttemptAt)
			p.LastSuccessAt = utc(p.LastSuccessAt)
			if p.Failure != nil {
				p.Failure.At = utc(p.Failure.At)
			}
		}
	}
}

// Validate checks the schema, identity relationships, and portable placement
// paths. It does not touch the filesystem.
func (m *Manifest) Validate() error {
	if m == nil {
		return fmt.Errorf("%w: nil manifest", ErrInvalid)
	}
	if m.SchemaVersion != CurrentSchemaVersion {
		return fmt.Errorf("%w: %d (supported: %d)", ErrUnknownSchema, m.SchemaVersion, CurrentSchemaVersion)
	}
	if err := validateID("collection", m.Collection.ID); err != nil {
		return err
	}
	if err := validatePresence("collection", m.Collection.PresenceState); err != nil {
		return err
	}
	if err := validateRunState(m.Collection.RunState); err != nil {
		return err
	}

	setIDs := make(map[string]struct{}, len(m.Sets))
	setPresence := make(map[string]PresenceState, len(m.Sets))
	for i := range m.Sets {
		s := &m.Sets[i]
		if err := validateID(fmt.Sprintf("set %d", i), s.ID); err != nil {
			return err
		}
		if _, exists := setIDs[s.ID]; exists {
			return fmt.Errorf("%w: duplicate set ID %q", ErrInvalid, s.ID)
		}
		setIDs[s.ID] = struct{}{}
		setPresence[s.ID] = s.PresenceState
		if err := validatePresence("set "+s.ID, s.PresenceState); err != nil {
			return err
		}
	}

	referenceIDs := make(map[string]struct{}, len(m.References))
	allPlacementPaths := make(map[string]string)
	for i := range m.References {
		r := &m.References[i]
		if err := validateID(fmt.Sprintf("reference %d", i), r.ID); err != nil {
			return err
		}
		if _, exists := referenceIDs[r.ID]; exists {
			return fmt.Errorf("%w: duplicate reference ID %q", ErrInvalid, r.ID)
		}
		referenceIDs[r.ID] = struct{}{}
		if err := validatePresence("reference "+r.ID, r.PresenceState); err != nil {
			return err
		}
		if err := validateDownloadState(r.DownloadState); err != nil {
			return fmt.Errorf("%w: reference %q: %v", ErrInvalid, r.ID, err)
		}
		if err := validateChecksum("reference "+r.ID, r.SHA256); err != nil {
			return err
		}
		if err := validateFailureConsistency("reference "+r.ID, r.DownloadState, r.Failure); err != nil {
			return err
		}
		if m.Collection.PresenceState == PresenceMissing && r.PresenceState == PresencePresent {
			return fmt.Errorf("%w: missing Collection cannot contain present reference %q", ErrInvalid, r.ID)
		}

		placementSets := make(map[string]struct{}, len(r.Placements))
		placementPaths := make(map[string]struct{}, len(r.Placements))
		presentPlacements := 0
		for j := range r.Placements {
			p := &r.Placements[j]
			if err := validateID("placement set", p.SetID); err != nil {
				return err
			}
			if _, exists := setIDs[p.SetID]; !exists {
				return fmt.Errorf("%w: placement %q refers to unknown set %q", ErrInvalid, r.ID, p.SetID)
			}
			if _, exists := placementSets[p.SetID]; exists {
				return fmt.Errorf("%w: duplicate placement for reference %q and set %q", ErrInvalid, r.ID, p.SetID)
			}
			placementSets[p.SetID] = struct{}{}
			if err := validateRelativePath(p.Path); err != nil {
				return fmt.Errorf("%w: reference %q placement: %v", ErrInvalid, r.ID, err)
			}
			if err := validatePlacementLayout(p.Path, p.SetID, r.ID); err != nil {
				return fmt.Errorf("%w: reference %q placement: %v", ErrInvalid, r.ID, err)
			}
			if err := validateChecksum("placement "+p.Path, p.InstalledSHA256); err != nil {
				return err
			}
			if _, exists := placementPaths[p.Path]; exists {
				return fmt.Errorf("%w: duplicate placement path %q", ErrInvalid, p.Path)
			}
			placementPaths[p.Path] = struct{}{}
			if previousReference, exists := allPlacementPaths[p.Path]; exists {
				return fmt.Errorf("%w: placement path %q is used by references %q and %q", ErrInvalid, p.Path, previousReference, r.ID)
			}
			allPlacementPaths[p.Path] = r.ID
			if err := validatePresence("placement "+p.Path, p.PresenceState); err != nil {
				return err
			}
			if p.PresenceState == PresencePresent {
				presentPlacements++
				if r.PresenceState == PresenceMissing {
					return fmt.Errorf("%w: missing reference %q cannot contain present placements", ErrInvalid, r.ID)
				}
				if setPresence[p.SetID] != PresencePresent {
					return fmt.Errorf("%w: present placement for reference %q is under a missing set", ErrInvalid, r.ID)
				}
				if m.Collection.PresenceState == PresenceMissing {
					return fmt.Errorf("%w: missing Collection cannot contain present placements", ErrInvalid)
				}
			}
			if err := validateDownloadState(p.DownloadState); err != nil {
				return fmt.Errorf("%w: placement %q: %v", ErrInvalid, p.Path, err)
			}
			if err := validateFailureConsistency("placement "+p.Path, p.DownloadState, p.Failure); err != nil {
				return err
			}
		}
		if r.PresenceState == PresencePresent && presentPlacements == 0 {
			return fmt.Errorf("%w: present reference %q requires a present placement", ErrInvalid, r.ID)
		}
	}
	if m.Collection.PresenceState == PresenceMissing {
		for id, presence := range setPresence {
			if presence == PresencePresent {
				return fmt.Errorf("%w: missing Collection cannot contain present set %q", ErrInvalid, id)
			}
		}
	}
	return nil
}

// Load reads, validates, and normalizes a manifest. An absent file can be
// recognized with errors.Is(err, ErrNotFound) or IsNotFound(err).
func Load(s store.Store, rel string) (*Manifest, error) {
	f, err := s.Open(rel)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, rel)
		}
		return nil, fmt.Errorf("open manifest %q: %w", rel, err)
	}
	defer f.Close()

	data, err := io.ReadAll(io.LimitReader(f, MaxManifestBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read manifest %q: %w", rel, err)
	}
	if int64(len(data)) > MaxManifestBytes {
		return nil, fmt.Errorf("%w: manifest %q exceeds %d bytes", ErrInvalid, rel, MaxManifestBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var m Manifest
	if err := decoder.Decode(&m); err != nil {
		return nil, fmt.Errorf("decode manifest %q: %w", rel, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("%w: manifest %q contains multiple JSON values", ErrInvalid, rel)
		}
		return nil, fmt.Errorf("decode manifest %q: trailing data: %w", rel, err)
	}
	m.Normalize()
	if err := m.Validate(); err != nil {
		return nil, fmt.Errorf("validate manifest %q: %w", rel, err)
	}
	return &m, nil
}

// Write validates and atomically replaces rel with a human-readable manifest.
// The value may be either Manifest or *Manifest. The file and its temporary
// replacement are owner-readable and owner-writable only.
func Write(s store.Store, rel string, value any) error {
	m, err := manifestValue(value)
	if err != nil {
		return err
	}
	m.Normalize()
	if err := m.Validate(); err != nil {
		return fmt.Errorf("validate manifest before write: %w", err)
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}
	data = append(data, '\n')

	return s.Put(rel, bytes.NewReader(data), int64(len(data)), nil)
}

// IsNotFound reports whether err represents an absent collection manifest.
func IsNotFound(err error) bool {
	return errors.Is(err, ErrNotFound)
}

// IsMissing reports whether err represents an absent collection manifest.
func IsMissing(err error) bool {
	return IsNotFound(err)
}

func manifestValue(value any) (Manifest, error) {
	switch v := value.(type) {
	case Manifest:
		return v, nil
	case *Manifest:
		if v == nil {
			return Manifest{}, fmt.Errorf("%w: nil manifest", ErrInvalid)
		}
		return *v, nil
	default:
		return Manifest{}, fmt.Errorf("%w: Write expects Manifest or *Manifest, got %T", ErrInvalid, value)
	}
}

func utc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	v := t.UTC()
	return &v
}

func validateID(subject, id string) error {
	if id == "" || strings.TrimSpace(id) != id || id == "." || id == ".." {
		return fmt.Errorf("%w: %s ID is empty or malformed", ErrInvalid, subject)
	}
	if strings.ContainsAny(id, "/\\") {
		return fmt.Errorf("%w: %s ID contains a path separator", ErrInvalid, subject)
	}
	for _, r := range id {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: %s ID contains a control character", ErrInvalid, subject)
		}
	}
	return nil
}

func validatePresence(subject string, state PresenceState) error {
	if state == PresencePresent || state == PresenceMissing {
		return nil
	}
	return fmt.Errorf("%w: %s has invalid presence state %q", ErrInvalid, subject, state)
}

func validateDownloadState(state DownloadState) error {
	switch state {
	case DownloadPending, DownloadComplete, DownloadFailed:
		return nil
	default:
		return fmt.Errorf("invalid download state %q", state)
	}
}

func validateRunState(state RunState) error {
	switch state {
	case RunIncomplete, RunComplete:
		return nil
	default:
		return fmt.Errorf("%w: invalid run state %q", ErrInvalid, state)
	}
}

func validateChecksum(subject, checksum string) error {
	if checksum == "" {
		return nil
	}
	if len(checksum) != 64 {
		return fmt.Errorf("%w: %s checksum must contain exactly 64 hexadecimal characters", ErrInvalid, subject)
	}
	if _, err := hex.DecodeString(checksum); err != nil {
		return fmt.Errorf("%w: %s checksum is not hexadecimal", ErrInvalid, subject)
	}
	return nil
}

func validateFailureConsistency(subject string, state DownloadState, failure *Failure) error {
	if state == DownloadFailed {
		if failure == nil {
			return fmt.Errorf("%w: %s failed download requires failure code and message", ErrInvalid, subject)
		}
		if err := validateFailureText(subject+" failure code", failure.Code); err != nil {
			return err
		}
		if err := validateFailureText(subject+" failure message", failure.Message); err != nil {
			return err
		}
		return nil
	}
	if failure != nil {
		return fmt.Errorf("%w: %s has failure details but is not failed", ErrInvalid, subject)
	}
	return nil
}

func validateFailureText(subject, value string) error {
	if value == "" || strings.TrimSpace(value) != value {
		return fmt.Errorf("%w: %s must be non-empty and trimmed", ErrInvalid, subject)
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: %s contains a control character", ErrInvalid, subject)
		}
	}
	if strings.Contains(strings.ToLower(value), "://") {
		return fmt.Errorf("%w: %s contains a URL", ErrInvalid, subject)
	}
	return nil
}

func validateRelativePath(value string) error {
	if value == "" || value == "." {
		return errors.New("placement path is empty")
	}
	if filepath.IsAbs(value) || path.IsAbs(value) || isWindowsAbsolute(value) {
		return errors.New("placement path must be relative")
	}
	if strings.Contains(value, "\\") {
		return errors.New("placement path must use portable separators")
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return errors.New("placement path contains a control character")
		}
	}
	parts := strings.Split(value, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return errors.New("placement path contains an empty or dot segment")
		}
	}
	if path.Clean(value) != value {
		return errors.New("placement path is not normalized")
	}
	return nil
}

func validatePlacementLayout(value, setID, referenceID string) error {
	parts := strings.Split(value, "/")
	if len(parts) != 2 {
		return errors.New("placement path must contain exactly one Set and one Reference component")
	}
	setSuffix := "--" + setID
	if len(parts[0]) <= len(setSuffix) || !strings.HasSuffix(parts[0], setSuffix) {
		return errors.New("placement Set component has the wrong stable ID")
	}
	referenceMarker := "--" + referenceID + "."
	markerAt := strings.LastIndex(parts[1], referenceMarker)
	if markerAt <= 0 || markerAt+len(referenceMarker) >= len(parts[1]) {
		return errors.New("placement Reference component has the wrong stable ID or extension")
	}
	return nil
}

func isWindowsAbsolute(value string) bool {
	if strings.HasPrefix(value, "//") || strings.HasPrefix(value, `\\`) {
		return true
	}
	return len(value) >= 2 && value[1] == ':'
}
