package browsercookies

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/browserutils/kooky"
)

const (
	maxUserAgentMetadataBytes = 64 << 10
	maxSafariOutputBytes      = 4 << 10
	safariCommandTimeout      = 500 * time.Millisecond
)

type userAgentCommand func(context.Context, string, ...string) *exec.Cmd

// detectUserAgent returns a browser User-Agent without making browser calls.
// Metadata is optional, so every unsuccessful detection path returns an empty string.
func detectUserAgent(ctx context.Context, selected kooky.CookieStore) string {
	return detectUserAgentWith(ctx, selected, runtime.GOOS, exec.CommandContext, nil)
}

// detectUserAgentWith keeps OS and Safari process details injectable for tests.
func detectUserAgentWith(ctx context.Context, selected kooky.CookieStore, goos string, command userAgentCommand, safariPaths []string) string {
	if ctx == nil || ctx.Err() != nil || isNilStore(selected) {
		return ""
	}
	platform := userAgentPlatform(goos)
	if platform == "" {
		return ""
	}

	switch strings.ToLower(selected.Browser()) {
	case "brave", "chrome", "chromium", "edge":
		return detectChromiumUserAgent(selected, platform)
	case "firefox":
		return detectFirefoxUserAgent(selected, platform)
	case "safari":
		if goos != "darwin" {
			return ""
		}
		return detectSafariUserAgent(ctx, selected, command, safariPaths)
	default:
		return ""
	}
}

func userAgentPlatform(goos string) string {
	switch goos {
	case "darwin":
		return "Macintosh; Intel Mac OS X 10_15_7"
	case "linux":
		return "X11; Linux x86_64"
	default:
		return ""
	}
}

func detectChromiumUserAgent(selected kooky.CookieStore, platform string) string {
	profileDir, _, ok := chromiumProfileDirectory(selected.FilePath())
	if !ok {
		return ""
	}
	data, ok := readUserAgentMetadata(filepath.Join(filepath.Dir(profileDir), "Last Version"))
	if !ok {
		return ""
	}
	version, ok := validDottedVersion(strings.TrimSpace(string(data)), 4)
	if !ok {
		return ""
	}

	ua := "Mozilla/5.0 (" + platform + ") AppleWebKit/537.36 (KHTML, like Gecko) Chrome/" + version + " Safari/537.36"
	if strings.EqualFold(selected.Browser(), "edge") {
		ua += " Edg/" + version
	}
	return ua
}

func detectFirefoxUserAgent(selected kooky.CookieStore, platform string) string {
	data, ok := readUserAgentMetadata(filepath.Join(filepath.Dir(selected.FilePath()), "compatibility.ini"))
	if !ok {
		return ""
	}
	version, ok := firefoxCompatibilityVersion(data)
	if !ok {
		return ""
	}
	return "Mozilla/5.0 (" + platform + "; rv:" + version + ") Gecko/20100101 Firefox/" + version
}

func firefoxCompatibilityVersion(data []byte) (string, bool) {
	inCompatibility := false
	version := ""
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "[Compatibility]" {
			inCompatibility = true
			continue
		}
		if strings.HasPrefix(line, "[") {
			if !strings.HasSuffix(line, "]") {
				return "", false
			}
			inCompatibility = false
			continue
		}
		if !inCompatibility {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found || strings.TrimSpace(key) != "LastVersion" {
			continue
		}
		if index := strings.IndexByte(value, '_'); index >= 0 {
			value = value[:index]
		}
		if version != "" {
			return "", false
		}
		var valid bool
		version, valid = validDottedVersion(strings.TrimSpace(value), 2)
		if !valid {
			return "", false
		}
	}
	return version, version != ""
}

func validDottedVersion(value string, minimumComponents int) (string, bool) {
	if len(value) == 0 || len(value) > 64 {
		return "", false
	}
	components := strings.Split(value, ".")
	if len(components) < minimumComponents {
		return "", false
	}
	for _, component := range components {
		if component == "" {
			return "", false
		}
		for _, character := range component {
			if character < '0' || character > '9' {
				return "", false
			}
		}
	}
	return value, true
}

func detectSafariUserAgent(ctx context.Context, selected kooky.CookieStore, command userAgentCommand, safariPaths []string) string {
	if command == nil {
		command = exec.CommandContext
	}
	if len(safariPaths) == 0 {
		safariPaths = []string{
			"/System/Applications/Safari.app/Contents/Info.plist",
			"/Applications/Safari.app/Contents/Info.plist",
		}
	}

	for _, path := range safariPaths {
		if !userAgentMetadataFile(path) {
			continue
		}
		for _, key := range []string{"CFBundleShortVersionString", "CFBundleVersion"} {
			output, ok := safariPlutilValue(ctx, command, key, path)
			if !ok {
				continue
			}
			version, valid := validDottedVersion(strings.TrimSpace(string(output)), 2)
			if valid {
				return "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/" + version + " Safari/605.1.15"
			}
		}
	}
	return ""
}

func safariPlutilValue(ctx context.Context, command userAgentCommand, key, path string) ([]byte, bool) {
	commandContext, cancel := context.WithTimeout(ctx, safariCommandTimeout)
	defer cancel()
	cmd := command(commandContext, "/usr/bin/plutil", "-extract", key, "raw", "-o", "-", path)
	if cmd == nil {
		return nil, false
	}
	output := &boundedUserAgentBuffer{limit: maxSafariOutputBytes}
	cmd.Stdout = output
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil || output.exceeded {
		return nil, false
	}
	return output.buffer.Bytes(), true
}

type boundedUserAgentBuffer struct {
	buffer   bytes.Buffer
	limit    int
	exceeded bool
}

func (b *boundedUserAgentBuffer) Write(value []byte) (int, error) {
	if b.exceeded {
		return 0, errors.New("output limit exceeded")
	}
	remaining := b.limit - b.buffer.Len()
	if len(value) > remaining {
		if remaining > 0 {
			_, _ = b.buffer.Write(value[:remaining])
		}
		b.exceeded = true
		return remaining, errors.New("output limit exceeded")
	}
	return b.buffer.Write(value)
}

func readUserAgentMetadata(filename string) ([]byte, bool) {
	if !userAgentMetadataFile(filename) {
		return nil, false
	}
	file, err := os.Open(filename)
	if err != nil {
		return nil, false
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxUserAgentMetadataBytes+1))
	if err != nil || len(data) > maxUserAgentMetadataBytes {
		return nil, false
	}
	return data, true
}

func userAgentMetadataFile(filename string) bool {
	info, err := os.Lstat(filename)
	return err == nil && info.Mode().IsRegular() && info.Size() >= 0 && info.Size() <= maxUserAgentMetadataBytes
}
