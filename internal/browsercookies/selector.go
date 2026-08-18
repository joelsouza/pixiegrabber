package browsercookies

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
)

const maxSelectorBytes = 4096

var supportedBrowsers = map[string]struct{}{
	"brave":    {},
	"chrome":   {},
	"chromium": {},
	"edge":     {},
	"firefox":  {},
	"safari":   {},
}

// Selector identifies one browser profile and an optional Firefox container.
type Selector struct {
	Browser   string
	Profile   string
	Container string

	hasProfile   bool
	hasContainer bool
}

// ParseSelector parses BROWSER[:PROFILE][::CONTAINER].
func ParseSelector(value string) (Selector, error) {
	if len(value) > maxSelectorBytes || hasControl(value) {
		return Selector{}, errors.New("browser selector is invalid; remove control characters or shorten it")
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return Selector{}, errors.New("browser selector is required; use BROWSER[:PROFILE][::CONTAINER]")
	}

	selector := Selector{}
	parts := strings.Split(value, "::")
	if len(parts) > 2 {
		return Selector{}, errors.New("browser selector has too many container separators; use at most one ::CONTAINER suffix")
	}
	base := parts[0]
	if len(parts) == 2 {
		selector.Container = strings.TrimSpace(parts[1])
		if selector.Container == "" || strings.Contains(selector.Container, ":") {
			return Selector{}, errors.New("firefox container is invalid; provide one name after the :: separator")
		}
		selector.hasContainer = true
	}

	browser, profile, hasProfile := strings.Cut(base, ":")
	if strings.Contains(profile, ":") {
		return Selector{}, errors.New("browser profile is invalid; provide one profile name")
	}
	selector.Browser = strings.ToLower(strings.TrimSpace(browser))
	if _, supported := supportedBrowsers[selector.Browser]; !supported {
		return Selector{}, fmt.Errorf("browser %q is not supported; use brave, chrome, chromium, edge, firefox, or safari", selector.Browser)
	}
	if hasProfile {
		selector.Profile = strings.TrimSpace(profile)
		if selector.Profile == "" {
			return Selector{}, errors.New("browser profile is empty; omit the colon to use the default profile")
		}
		selector.hasProfile = true
	}
	if selector.hasContainer && selector.Browser != "firefox" {
		return Selector{}, errors.New("browser containers are supported only for Firefox")
	}
	return selector, nil
}

func hasControl(value string) bool {
	return strings.IndexFunc(value, unicode.IsControl) >= 0
}
