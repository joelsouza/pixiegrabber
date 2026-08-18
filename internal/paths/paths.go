// Package paths builds the portable names used by a local Collection archive.
package paths

import (
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// MaxComponentLength is the maximum byte length used for a generated path
// component. It is the limit supported by the common platforms.
const MaxComponentLength = 255

const fallbackName = "untitled"

// Component returns a readable, portable component in the form
// name--stableID. The stable ID is kept at the end so a rename does not change
// the identity of a local component.
func Component(name, stableID string) string {
	return component(name, stableID, "")
}

// CollectionComponent returns the portable directory component for a
// Collection.
func CollectionComponent(name, collectionID string) string {
	return Component(name, collectionID)
}

// SetComponent returns the portable directory component for a Set.
func SetComponent(name, setID string) string {
	return Component(name, setID)
}

// ReferenceComponent returns a portable reference filename. Extension may
// start with a dot. The ID and extension are retained when the readable name
// must be shortened.
func ReferenceComponent(name, stableID, extension string) string {
	ext := cleanExtension(extension)
	name = removeMatchingExtension(name, ext)
	if ext != "" {
		ext = "." + ext
	}
	return component(name, stableID, ext)
}

func removeMatchingExtension(name, extension string) string {
	if extension == "" {
		return name
	}
	name = strings.TrimRight(name, " .")
	suffix := "." + extension
	if len(name) >= len(suffix) && strings.EqualFold(name[len(name)-len(suffix):], suffix) {
		return name[:len(name)-len(suffix)]
	}
	return name
}

// CollectionPath returns the path for one Collection below root.
func CollectionPath(root, name, collectionID string) string {
	return filepath.Join(root, Component(name, collectionID))
}

// SetPath returns the path for one Set below a Collection directory.
func SetPath(collectionPath, name, setID string) string {
	return filepath.Join(collectionPath, Component(name, setID))
}

// ReferencePath returns the path for one Reference below a Set directory.
func ReferencePath(setPath, name, mediaID, extension string) string {
	return filepath.Join(setPath, ReferenceComponent(name, mediaID, extension))
}

// SanitizeName returns the readable part of a path component without a stable
// ID. It is useful when a caller needs to display the same normalized name.
func SanitizeName(name string) string {
	return cleanName(name)
}

func component(name, stableID, suffix string) string {
	cleanID := cleanStableID(stableID)
	stableSuffix := "--" + cleanID + suffix
	base := cleanName(name)
	limit := MaxComponentLength - len(stableSuffix)
	if limit < 1 {
		// IDs from Pixieset are short. If a caller supplies a longer ID, retain
		// the complete identity suffix rather than silently changing it.
		return stableSuffix
	}
	base = truncateUTF8(base, limit)
	if base == "" {
		base = fallbackName
		base = truncateUTF8(base, limit)
	}
	return base + stableSuffix
}

func cleanName(name string) string {
	clean := cleanComponent(name)
	if clean == "" || clean == "." || clean == ".." {
		return fallbackName
	}
	return clean
}

func cleanStableID(id string) string {
	clean := cleanComponent(id)
	if clean == "" {
		return "unknown"
	}
	return clean
}

func cleanExtension(extension string) string {
	extension = strings.TrimSpace(extension)
	extension = strings.TrimLeft(extension, ".")
	return cleanComponent(extension)
}

func cleanComponent(value string) string {
	var b strings.Builder
	for _, r := range strings.ToValidUTF8(value, "�") {
		if unsafeRune(r) {
			b.WriteByte('_')
			continue
		}
		b.WriteRune(r)
	}
	return strings.TrimRight(b.String(), " .")
}

func unsafeRune(r rune) bool {
	if r < 0x20 || r == 0x7f {
		return true
	}
	switch r {
	case '/', '\\', ':', '*', '?', '"', '<', '>', '|':
		return true
	default:
		return false
	}
}

func truncateUTF8(value string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(value) <= maxBytes {
		return value
	}
	value = value[:maxBytes]
	for len(value) > 0 && !utf8.ValidString(value) {
		_, size := utf8.DecodeLastRuneInString(value)
		if size == 0 {
			break
		}
		value = value[:len(value)-size]
	}
	return value
}
