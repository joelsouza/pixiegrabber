package browsercookies

import (
	"strings"
	"testing"
)

func TestParseSelector(t *testing.T) {
	tests := []struct {
		input string
		want  Selector
	}{
		{input: "", want: Selector{}},
		{input: "   ", want: Selector{}},
		{input: "chrome", want: Selector{Browser: "chrome"}},
		{input: "::Work", want: Selector{Container: "Work", hasContainer: true}},
		{input: "firefox:/tmp/profiles/work", want: Selector{Browser: "firefox", Profile: "/tmp/profiles/work", hasProfile: true, profileIsPath: true}},
		{input: "Brave:Profile 2", want: Selector{Browser: "brave", Profile: "Profile 2", hasProfile: true}},
		{input: "firefox::Personal", want: Selector{Browser: "firefox", Container: "Personal", hasContainer: true}},
		{input: "firefox:default-release::none", want: Selector{Browser: "firefox", Profile: "default-release", Container: "none", hasProfile: true, hasContainer: true}},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := ParseSelector(tt.input)
			if err != nil {
				t.Fatalf("ParseSelector() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("ParseSelector() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

// An absent flag arrives as an empty value, which must search every browser
// instead of stopping the run.
func TestParseSelectorWithoutAValueSearchesEveryBrowser(t *testing.T) {
	selector, err := ParseSelector("")
	if err != nil {
		t.Fatalf("ParseSelector(%q) error = %v", "", err)
	}
	if selector != (Selector{}) {
		t.Fatalf("ParseSelector(%q) = %#v, want an empty selector", "", selector)
	}
	if selector.hasBrowser() {
		t.Fatal("an empty selector names a browser")
	}
	if !(Selector{Browser: "chrome"}).hasBrowser() {
		t.Fatal("a named browser was not reported")
	}
}

func TestParseSelectorRejectsInvalidValues(t *testing.T) {
	for _, input := range []string{"unknown", "chrome:", ":profile", "chrome::container", "firefox:::container", "firefox:profile::", "firefox:one::two::three", "firefox:\nprofile"} {
		t.Run(input, func(t *testing.T) {
			if _, err := ParseSelector(input); err == nil {
				t.Fatalf("ParseSelector(%q) succeeded", input)
			}
		})
	}
}

func TestParseSelectorValidatesRawInputBeforeTrimming(t *testing.T) {
	if _, err := ParseSelector("\nchrome"); err == nil {
		t.Fatal("ParseSelector() accepted a leading control character")
	}
	if _, err := ParseSelector(" " + strings.Repeat("a", maxSelectorBytes) + " "); err == nil || !strings.Contains(err.Error(), "shorten it") {
		t.Fatalf("ParseSelector() did not reject an overlong raw selector: %v", err)
	}
}
