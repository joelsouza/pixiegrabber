package paths

import (
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestComponentKeepsReadableSafeUnicodeAndStableID(t *testing.T) {
	got := Component("Café / summer: 2026", "media-42")
	if got != "Café _ summer_ 2026--media-42" {
		t.Fatalf("Component() = %q", got)
	}
	if !strings.HasSuffix(got, "--media-42") {
		t.Fatalf("component lost stable ID: %q", got)
	}
}

func TestComponentNormalizesUnsafeNames(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		{name: "", want: "untitled--id"},
		{name: ".", want: "untitled--id"},
		{name: "..", want: "untitled--id"},
		{name: "CON", want: "CON--id"},
		{name: "name. ", want: "name--id"},
		{name: "bad\tname", want: "bad_name--id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Component(tt.name, "id"); got != tt.want {
				t.Fatalf("Component(%q, id) = %q, want %q", tt.name, got, tt.want)
			}
		})
	}
}

func TestReferenceComponentCapsNameButRetainsIDAndExtension(t *testing.T) {
	name := strings.Repeat("非常に長い名前", 80)
	got := ReferenceComponent(name, "reference-123", "jpeg")
	if !strings.HasSuffix(got, "--reference-123.jpeg") {
		t.Fatalf("reference suffix = %q", got)
	}
	if len(got) > MaxComponentLength {
		t.Fatalf("reference component has %d bytes, want at most %d", len(got), MaxComponentLength)
	}
	if !utf8.ValidString(got) {
		t.Fatal("reference component is not valid UTF-8")
	}
}

func TestReferenceComponentRemovesMatchingSourceExtension(t *testing.T) {
	got := ReferenceComponent("IMG_1.JPG", "ID", ".JPG")
	if got != "IMG_1--ID.JPG" {
		t.Fatalf("ReferenceComponent() = %q, want %q", got, "IMG_1--ID.JPG")
	}

	got = ReferenceComponent("IMG_1.jpeg", "ID", ".JPG")
	if got != "IMG_1.jpeg--ID.JPG" {
		t.Fatalf("non-matching extension was removed: %q", got)
	}
}

func TestPathsFollowCollectionLayout(t *testing.T) {
	root := t.TempDir()
	collection := CollectionPath(root, "My collection", "collection-1")
	set := SetPath(collection, "Set one", "set-1")
	reference := ReferencePath(set, "A photo", "media-1", ".jpg")

	wantCollection := filepath.Join(root, "My collection--collection-1")
	wantSet := filepath.Join(wantCollection, "Set one--set-1")
	wantReference := filepath.Join(wantSet, "A photo--media-1.jpg")
	if collection != wantCollection || set != wantSet || reference != wantReference {
		t.Fatalf("paths = %q, %q, %q", collection, set, reference)
	}
}
