package browsercookies

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDetectUserAgentChromiumLayoutsAndTemplates(t *testing.T) {
	tests := []struct {
		browser string
		suffix  string
		want    string
	}{
		{browser: "brave", suffix: "", want: "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/123.45.6.7 Safari/537.36"},
		{browser: "chrome", suffix: "/Network", want: "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/123.45.6.7 Safari/537.36"},
		{browser: "chromium", suffix: "", want: "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/123.45.6.7 Safari/537.36"},
		{browser: "edge", suffix: "/Network", want: "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/123.45.6.7 Safari/537.36 Edg/123.45.6.7"},
	}
	for _, tt := range tests {
		t.Run(tt.browser+tt.suffix, func(t *testing.T) {
			root := t.TempDir()
			profile := filepath.Join(root, "User Data", "Default")
			cookiePath := filepath.Join(profile, "Cookies")
			if tt.suffix != "" {
				cookiePath = filepath.Join(profile, tt.suffix[1:], "Cookies")
			}
			writeUserAgentFile(t, cookiePath, "cookies")
			writeUserAgentFile(t, filepath.Join(root, "User Data", "Last Version"), " 123.45.6.7\n")
			store := &fakeStore{browser: tt.browser, filePath: cookiePath}
			if got := detectUserAgentWith(context.Background(), store, "linux", nil, nil); got != tt.want {
				t.Fatalf("detectUserAgentWith() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDetectUserAgentPlatformTemplates(t *testing.T) {
	root := t.TempDir()
	cookiePath := filepath.Join(root, "User Data", "Default", "Cookies")
	writeUserAgentFile(t, cookiePath, "cookies")
	writeUserAgentFile(t, filepath.Join(root, "User Data", "Last Version"), "123.45.6.7")
	store := &fakeStore{browser: "chrome", filePath: cookiePath}
	for _, tt := range []struct {
		goos   string
		prefix string
	}{
		{goos: "darwin", prefix: "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7)"},
		{goos: "linux", prefix: "Mozilla/5.0 (X11; Linux x86_64)"},
	} {
		t.Run(tt.goos, func(t *testing.T) {
			got := detectUserAgentWith(context.Background(), store, tt.goos, nil, nil)
			if !strings.HasPrefix(got, tt.prefix+" ") {
				t.Fatalf("User-Agent = %q, want prefix %q", got, tt.prefix)
			}
		})
	}
	if got := detectUserAgentWith(context.Background(), store, "freebsd", nil, nil); got != "" {
		t.Fatalf("unsupported OS User-Agent = %q, want empty", got)
	}
}

func TestDetectUserAgentRejectsChromiumMetadata(t *testing.T) {
	tests := []struct {
		name string
		data string
		link bool
	}{
		{name: "missing", data: "", link: false},
		{name: "too few components", data: "123.45"},
		{name: "non numeric", data: "123.45.x.7"},
		{name: "control", data: "123.45.6.7\x00"},
		{name: "oversized", data: strings.Repeat("1", 65)},
		{name: "symlink", data: "123.45.6.7", link: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			cookiePath := filepath.Join(root, "User Data", "Default", "Cookies")
			writeUserAgentFile(t, cookiePath, "cookies")
			versionPath := filepath.Join(root, "User Data", "Last Version")
			if tt.link {
				target := filepath.Join(root, "version-target")
				writeUserAgentFile(t, target, tt.data)
				if err := os.Symlink(target, versionPath); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			} else if tt.name != "missing" {
				writeUserAgentFile(t, versionPath, tt.data)
			}
			store := &fakeStore{browser: "chrome", filePath: cookiePath}
			if got := detectUserAgentWith(context.Background(), store, "linux", nil, nil); got != "" {
				t.Fatalf("User-Agent = %q, want empty", got)
			}
		})
	}
}

func TestDetectUserAgentFirefoxCompatibilityParsing(t *testing.T) {
	tests := []struct {
		name string
		data string
		want string
	}{
		{
			name: "section and build suffix",
			data: "[Other]\nLastVersion=1.2.3\n[Compatibility]\nLastVersion=128.0_20240101120000\n",
			want: "Mozilla/5.0 (X11; Linux x86_64; rv:128.0) Gecko/20100101 Firefox/128.0",
		},
		{name: "missing section", data: "LastVersion=128.0\n"},
		{name: "missing version", data: "[Compatibility]\nLastAppDir=/tmp\n"},
		{name: "malformed section", data: "[Compatibility]\n[Other\nLastVersion=128.0\n"},
		{name: "malformed version", data: "[Compatibility]\nLastVersion=128\n"},
		{name: "control", data: "[Compatibility]\nLastVersion=128.0\x00\n"},
		{name: "oversized", data: "[Compatibility]\nLastVersion=128.0\n" + strings.Repeat("x", 65536)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			cookiePath := filepath.Join(root, "profile", "cookies.sqlite")
			writeUserAgentFile(t, cookiePath, "cookies")
			if tt.name != "missing section" && tt.name != "missing version" {
				writeUserAgentFile(t, filepath.Join(root, "profile", "compatibility.ini"), tt.data)
			} else if tt.name == "missing section" {
				writeUserAgentFile(t, filepath.Join(root, "profile", "compatibility.ini"), tt.data)
			}
			store := &fakeStore{browser: "firefox", filePath: cookiePath}
			got := detectUserAgentWith(context.Background(), store, "linux", nil, nil)
			if got != tt.want {
				t.Fatalf("User-Agent = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDetectUserAgentFirefoxRejectsSymlinkAndMissingMetadata(t *testing.T) {
	root := t.TempDir()
	cookiePath := filepath.Join(root, "profile", "cookies.sqlite")
	writeUserAgentFile(t, cookiePath, "cookies")
	compatibilityPath := filepath.Join(root, "profile", "compatibility.ini")
	target := filepath.Join(root, "compatibility-target")
	writeUserAgentFile(t, target, "[Compatibility]\nLastVersion=128.0\n")
	if err := os.Symlink(target, compatibilityPath); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	store := &fakeStore{browser: "firefox", filePath: cookiePath}
	if got := detectUserAgentWith(context.Background(), store, "linux", nil, nil); got != "" {
		t.Fatalf("symlink User-Agent = %q, want empty", got)
	}
}

func TestDetectUserAgentSafariPathPrecedenceAndVersionFallback(t *testing.T) {
	tests := []struct {
		name       string
		short      string
		build      string
		want       string
		wantCalls  []string
		commandErr bool
	}{
		{name: "path precedence", short: "17.4", build: "17.4.1", want: "17.4", wantCalls: []string{"first.plist"}},
		{name: "bundle fallback", short: "", build: "17.4.1", want: "17.4.1", wantCalls: []string{"first.plist", "first.plist"}},
		{name: "command failure", commandErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			first := filepath.Join(root, "first.plist")
			second := filepath.Join(root, "second.plist")
			writeUserAgentFile(t, first, "plist")
			writeUserAgentFile(t, second, "plist")
			var calls []string
			runner := func(ctx context.Context, _ string, args ...string) *exec.Cmd {
				path := args[len(args)-1]
				calls = append(calls, filepath.Base(path))
				key := args[1]
				output := ""
				if filepath.Base(path) == "first.plist" {
					if key == "CFBundleShortVersionString" {
						output = tt.short
					} else {
						output = tt.build
					}
				}
				if filepath.Base(path) == "second.plist" && key == "CFBundleShortVersionString" {
					output = "18.0"
				}
				if tt.commandErr {
					return safariHelperCommand(ctx, "", 1, false)
				}
				return safariHelperCommand(ctx, output, 0, false)
			}
			store := &fakeStore{browser: "safari", filePath: filepath.Join(root, "Cookies")}
			writeUserAgentFile(t, store.filePath, "cookies")
			got := detectUserAgentWith(context.Background(), store, "darwin", runner, []string{first, second})
			if tt.want == "" {
				if got != "" {
					t.Fatalf("User-Agent = %q, want empty", got)
				}
				return
			}
			want := "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/" + tt.want + " Safari/605.1.15"
			if got != want {
				t.Fatalf("User-Agent = %q, want %q", got, want)
			}
			if strings.Join(calls, ",") != strings.Join(tt.wantCalls, ",") {
				t.Fatalf("plutil paths = %v, want %v", calls, tt.wantCalls)
			}
		})
	}
}

func TestDetectUserAgentSafariFailureTimeoutAndNonDarwin(t *testing.T) {
	root := t.TempDir()
	plist := filepath.Join(root, "Safari.plist")
	writeUserAgentFile(t, plist, "plist")
	store := &fakeStore{browser: "safari", filePath: filepath.Join(root, "Cookies")}
	writeUserAgentFile(t, store.filePath, "cookies")

	t.Run("oversized output", func(t *testing.T) {
		runner := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
			return safariHelperCommand(ctx, strings.Repeat("1", 4097), 0, false)
		}
		if got := detectUserAgentWith(context.Background(), store, "darwin", runner, []string{plist}); got != "" {
			t.Fatalf("oversized output User-Agent = %q, want empty", got)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		runner := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
			return safariHelperCommand(ctx, "", 0, true)
		}
		started := time.Now()
		if got := detectUserAgentWith(context.Background(), store, "darwin", runner, []string{plist}); got != "" {
			t.Fatalf("timeout User-Agent = %q, want empty", got)
		}
		if elapsed := time.Since(started); elapsed > 2*time.Second {
			t.Fatalf("Safari command timeout took %s", elapsed)
		}
	})
	t.Run("non darwin does not run command", func(t *testing.T) {
		run := false
		runner := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
			run = true
			return safariHelperCommand(ctx, "17.0", 0, false)
		}
		if got := detectUserAgentWith(context.Background(), store, "linux", runner, []string{plist}); got != "" || run {
			t.Fatalf("non-darwin Safari detection = %q, command run=%v", got, run)
		}
	})
}

func TestDetectUserAgentDoesNotExposeMetadata(t *testing.T) {
	root := t.TempDir()
	secret := "metadata-secret-content"
	cookiePath := filepath.Join(root, "User Data", "Default", "Cookies")
	writeUserAgentFile(t, cookiePath, "cookies")
	writeUserAgentFile(t, filepath.Join(root, "User Data", "Last Version"), secret)
	if got := detectUserAgentWith(context.Background(), &fakeStore{browser: "chrome", filePath: cookiePath}, "linux", nil, nil); got != "" {
		t.Fatalf("invalid metadata User-Agent = %q", got)
	}
}

func TestSafariPlutilHelper(t *testing.T) {
	if os.Getenv("PIXIEGRABBER_SAFARI_HELPER") != "1" {
		return
	}
	if os.Getenv("PIXIEGRABBER_SAFARI_SLEEP") == "1" {
		time.Sleep(2 * time.Second)
		return
	}
	_, _ = io.WriteString(os.Stdout, os.Getenv("PIXIEGRABBER_SAFARI_OUTPUT"))
	if value := os.Getenv("PIXIEGRABBER_SAFARI_EXIT"); value != "" {
		if value == "1" {
			os.Exit(1)
		}
	}
	os.Exit(0)
}

func safariHelperCommand(ctx context.Context, output string, exit int, sleep bool) *exec.Cmd {
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestSafariPlutilHelper", "--")
	cmd.Env = append(os.Environ(), "GORACE=atexit_sleep_ms=0", "PIXIEGRABBER_SAFARI_HELPER=1", "PIXIEGRABBER_SAFARI_OUTPUT="+output, fmt.Sprintf("PIXIEGRABBER_SAFARI_EXIT=%d", exit))
	if sleep {
		cmd.Env = append(cmd.Env, "PIXIEGRABBER_SAFARI_SLEEP=1")
	}
	return cmd
}

func TestDetectUserAgentContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	root := t.TempDir()
	cookiePath := filepath.Join(root, "User Data", "Default", "Cookies")
	writeUserAgentFile(t, cookiePath, "cookies")
	writeUserAgentFile(t, filepath.Join(root, "User Data", "Last Version"), "123.45.6.7")
	if got := detectUserAgentWith(ctx, &fakeStore{browser: "chrome", filePath: cookiePath}, "linux", nil, nil); got != "" {
		t.Fatalf("canceled User-Agent = %q, want empty", got)
	}
}

func writeUserAgentFile(t *testing.T, filename, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filename), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
}
