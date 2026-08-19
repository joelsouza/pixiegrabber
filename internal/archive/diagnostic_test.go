package archive

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pixiegrabber/internal/outputfs"
	"pixiegrabber/internal/pixieset"
)

func TestClassifyVideosNoVideosDoesNotCreateOutput(t *testing.T) {
	fs := openTestFS(t)
	root := filepath.Dir(mustDisplayPath(t, fs, diagnosticFilename))
	if err := ClassifyVideos(fs, []pixieset.Set{{ID: "1"}}, Options{Videos: false}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != ".pixiegrabber.lock" {
			t.Fatalf("no-video check created unexpected entry %q", entry.Name())
		}
	}
}

func TestClassifyVideosWritesDiagnosticAndReturnsTypedError(t *testing.T) {
	secret := "synthetic-video-token-7f3c"
	first := videoSet(t, fmt.Sprintf(`{"kind":"video","name":"synthetic-name","url":"https://user:%s@media.example/video.mp4?sig=%s#fragment","width":1920},{"kind":"video","name":"second-video-secret"}`, secret, secret))
	second := videoSet(t, `{"kind":"video","name":"third-video"}`)
	fs := openTestFS(t)
	err := ClassifyVideos(fs, []pixieset.Set{{ID: "no-video"}, first, second}, Options{Videos: false})
	if err == nil || !errors.Is(err, ErrUnsupportedVideo) {
		t.Fatalf("ClassifyVideos() error = %v", err)
	}
	var typed *UnsupportedVideoError
	if !errors.As(err, &typed) || typed == nil {
		t.Fatalf("error type = %T, %v", err, err)
	}
	path := mustDisplayPath(t, fs, diagnosticFilename)
	if typed.Path() != path || typed.DiagnosticPath != path {
		t.Fatalf("diagnostic path = %q/%q, want %q", typed.Path(), typed.DiagnosticPath, path)
	}
	if !strings.Contains(err.Error(), "video download is unsupported and images were not started") || !strings.Contains(err.Error(), path) || strings.Contains(err.Error(), secret) {
		t.Fatalf("unsupported video error is unsafe or not actionable: %v", err)
	}
	data, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if strings.Contains(string(data), secret) || strings.Contains(string(data), "synthetic-name") || strings.Contains(string(data), "second-video-secret") || strings.Contains(string(data), "media.example") {
		t.Fatalf("diagnostic contains source data: %s", data)
	}
	if !strings.Contains(string(data), `"kind": "video"`) || !strings.Contains(string(data), `"width": 1920`) {
		t.Fatalf("diagnostic lost safe facts: %s", data)
	}
	info, statErr := os.Stat(path)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("diagnostic permissions = %o, want 600", info.Mode().Perm())
	}
}

func TestUnsupportedVideoErrorQuotesPathAndWriteFailuresAreOrdinary(t *testing.T) {
	quoted := &UnsupportedVideoError{DiagnosticPath: "/tmp/line\nnext"}
	if strings.Contains(quoted.Error(), "line\nnext") || !strings.Contains(quoted.Error(), `"/tmp/line\nnext"`) {
		t.Fatalf("error path was not quoted: %q", quoted.Error())
	}
	if _, err := sanitizeVideoJSON([]byte(`{"kind":"video","kind":false}`)); err == nil || errors.Is(err, ErrUnsupportedVideo) {
		t.Fatalf("ordinary diagnostic failure has unsupported sentinel: %v", err)
	}
}

func TestSanitizeVideoJSONPreservesShapeAndAllowlist(t *testing.T) {
	raw := []byte(`{"type":"video","kind":"movie","mime_type":"video/mp4","ext":"mp4","extension":"m4v","format":"webm","codec":"h264","width":1920,"height":1080,"size":1234,"file_size":1234,"duration":4.5,"duration_seconds":4.5,"rank":2,"bitrate":128000,"null_value":null,"flag":true,"secret":"synthetic-secret","url":"https://user:password@example.test/private/video.mp4?token=synthetic#part","nested":{"name":"synthetic-name","items":[null,false,17,"/Users/synthetic/private"]}}`)
	original := append([]byte(nil), raw...)
	value, err := sanitizeVideoJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, original) {
		t.Fatal("sanitizer changed its input")
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, safe := range []string{`"type": "video"`, `"mime_type": "video/mp4"`, `"ext": "mp4"`, `"codec": "h264"`, `"width": 1920`, `"duration": 4.5`, `"null_value": null`} {
		if !strings.Contains(text, safe) {
			t.Fatalf("missing safe value %s in %s", safe, text)
		}
	}
	for _, secret := range []string{"synthetic-secret", "synthetic-name", "user:password", "example.test", "token=synthetic", "/Users/synthetic"} {
		if strings.Contains(text, secret) {
			t.Fatalf("secret %q survived: %s", secret, text)
		}
	}
	for _, placeholder := range []string{`"flag": false`, `"secret": "[redacted]"`, `"url": "[redacted]"`, `"items": [`} {
		if !strings.Contains(text, placeholder) {
			t.Fatalf("missing placeholder %s in %s", placeholder, text)
		}
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	nested, ok := decoded["nested"].(map[string]any)
	if !ok {
		t.Fatalf("nested type = %T", decoded["nested"])
	}
	if _, ok := nested["items"].([]any); !ok {
		t.Fatalf("nested items type = %T", nested["items"])
	}
}

func TestSanitizeVideoJSONRejectsMalformedAndBoundedInputs(t *testing.T) {
	deep := "{}"
	for i := 0; i < maxVideoDiagnosticDepth+1; i++ {
		deep = `{"nested":` + deep + `}`
	}
	wide := `{"values":[` + strings.TrimSuffix(strings.Repeat("0,", maxVideoDiagnosticArrayLength+1), ",") + `]}`
	fields := make([]string, maxVideoDiagnosticFields+1)
	for i := range fields {
		fields[i] = fmt.Sprintf(`"field-%d":0`, i)
	}
	wideObject := "{" + strings.Join(fields, ",") + "}"
	tooManyKeys := "{" + fmt.Sprintf(`"%s":0`, strings.Repeat("k", maxVideoDiagnosticObjectKeyBytes+1)) + "}"
	nodes := `{"values":[` + strings.TrimSuffix(strings.Repeat(`[0,0,0,0,0,0,0,0,0,0],`, 1001), ",") + `]}`
	keyParts := make([]string, 0, 1100)
	for index := 0; index < 1100; index++ {
		keyParts = append(keyParts, fmt.Sprintf(`"field_%04d_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx":0`, index))
	}
	keyBytes := `{"values":{` + strings.Join(keyParts, ",") + `}}`
	tests := []struct {
		name string
		raw  string
	}{
		{"malformed", `{"kind":"video"`},
		{"multiple", `{} {}`},
		{"non-object", `[]`},
		{"deep", deep},
		{"wide-array", wide},
		{"wide-object", wideObject},
		{"long-key", tooManyKeys},
		{"impossible-number", `{"size":1e999}`},
		{"too-many-nodes", nodes},
		{"too-many-key-bytes", keyBytes},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := sanitizeVideoJSON([]byte(test.raw))
			if err == nil || strings.Contains(err.Error(), test.raw) {
				t.Fatalf("sanitize error = %v", err)
			}
		})
	}
	if _, err := sanitizeVideoJSON([]byte(strings.Repeat("x", maxVideoDiagnosticInputBytes+1))); err == nil {
		t.Fatal("oversized input was accepted")
	}
}

func TestSanitizeVideoJSONOwnsInputAndUTF8Limits(t *testing.T) {
	base := `{"value":""}`
	exact := maxVideoDiagnosticInputBytes - len(base)
	if exact < 0 {
		t.Fatal("test base is larger than input limit")
	}
	atBound := []byte(`{"value":"` + strings.Repeat("x", exact) + `"}`)
	if len(atBound) != maxVideoDiagnosticInputBytes {
		t.Fatalf("exact input size = %d, want %d", len(atBound), maxVideoDiagnosticInputBytes)
	}
	if _, err := sanitizeVideoJSON(atBound); err != nil {
		t.Fatalf("exact-bound input rejected: %v", err)
	}
	tooLarge := append(append([]byte(nil), atBound...), ' ')
	if _, err := sanitizeVideoJSON(tooLarge); err == nil || !strings.Contains(err.Error(), "input is too large") {
		t.Fatalf("over-bound input error = %v", err)
	}
	invalid := append([]byte(`{"value":"`), 0xff)
	invalid = append(invalid, []byte(`"}`)...)
	if _, err := sanitizeVideoJSON(invalid); err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("invalid UTF-8 error = %v", err)
	}
}

func TestSanitizeVideoJSONRejectsDuplicateKeysBeforeReplacement(t *testing.T) {
	for _, raw := range []string{
		`{"kind":"video","kind":false}`,
		`{"nested":{"width":1,"width":"secret"}}`,
		`{"bad-key":1,"bad-key":[]}`,
	} {
		if _, err := sanitizeVideoJSON([]byte(raw)); err == nil || !strings.Contains(err.Error(), "duplicate") {
			t.Errorf("duplicate input %s error = %v", raw, err)
		}
	}
}

func TestSanitizeVideoJSONUsesSchemaKeysAndDecodedSecretProof(t *testing.T) {
	raw := []byte(`{"type":"VIDEO","Video_Name":"synthetic-name","user@example.com":"synthetic-email","/private/signed?token=synthetic":"synthetic-key-value","_redacted_field_1":"schema-collision","bad-key":"synthetic-bad","token":"synthetic-token","nested":{"kind":"VIDEO","width":1920}}`)
	value, err := sanitizeVideoJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	object, ok := value.(diagnosticObject)
	if !ok {
		t.Fatalf("sanitized type = %T", value)
	}
	if object["type"] != "video" {
		t.Fatalf("enum = %#v, want lowercase canonical enum", object["type"])
	}
	if _, ok := object["_redacted_field_1"]; !ok {
		t.Fatalf("valid collision key was not preserved: %#v", object)
	}
	if object["token"] != diagnosticPlaceholder {
		t.Fatalf("token value was not redacted: %#v", object["token"])
	}
	for key := range object {
		if strings.Contains(key, "synthetic") || strings.ContainsAny(key, ":/@.?-\\") {
			t.Fatalf("unsafe key survived: %q", key)
		}
	}
	assertNoSyntheticDiagnosticData(t, object)
	if _, ok := object["nested"].(diagnosticObject); !ok {
		t.Fatalf("nested value type = %T", object["nested"])
	}
	nested := object["nested"].(diagnosticObject)
	if nested["kind"] != diagnosticPlaceholder || nested["width"] != int64(0) {
		t.Fatalf("nested facts were preserved: %#v", nested)
	}
}

func assertNoSyntheticDiagnosticData(t *testing.T, value any) {
	t.Helper()
	switch item := value.(type) {
	case string:
		if strings.Contains(item, "synthetic") || strings.Contains(item, "VIDEO") {
			t.Fatalf("unsafe decoded value survived: %q", item)
		}
	case diagnosticObject:
		for key, nested := range item {
			if strings.Contains(key, "synthetic") {
				t.Fatalf("unsafe decoded key survived: %q", key)
			}
			assertNoSyntheticDiagnosticData(t, nested)
		}
	case []any:
		for _, nested := range item {
			assertNoSyntheticDiagnosticData(t, nested)
		}
	}
}

func TestSanitizeVideoJSONCanonicalizesTopLevelMediaFacts(t *testing.T) {
	raw := []byte(`{"type":"VIDEO","width":1920.0,"height":1080e0,"size":1e3,"file_size":1099511627776,"rank":1.0,"bitrate":128000.00,"duration":1e-3,"duration_seconds":2.5000,"nested":{"width":1920},"other_number":123.45}`)
	value, err := sanitizeVideoJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	object := value.(diagnosticObject)
	if object["type"] != "video" || object["width"] != int64(1920) || object["height"] != int64(1080) || object["size"] != int64(1000) || object["file_size"] != int64(1099511627776) || object["rank"] != int64(1) || object["bitrate"] != int64(128000) {
		t.Fatalf("canonical integer facts = %#v", object)
	}
	if object["duration"] != float64(0.001) || object["duration_seconds"] != float64(2.5) || object["other_number"] != int64(0) {
		t.Fatalf("canonical float facts = %#v", object)
	}
	for _, raw := range []string{
		`{"width":0}`, `{"width":-1}`, `{"width":1.5}`, `{"size":-1}`, `{"rank":1.5}`, `{"duration":-0.1}`, `{"duration":1e309}`,
	} {
		if _, err := sanitizeVideoJSON([]byte(raw)); err == nil {
			t.Errorf("impossible media number accepted: %s", raw)
		}
	}
}

func TestSanitizeVideoJSONMediaBoundsAndNegativeZero(t *testing.T) {
	valid := []struct {
		name string
		raw  string
	}{
		{"dimension-boundary", `{"width":65535,"height":65535}`},
		{"file-size-boundary", `{"size":1099511627776,"file_size":1099511627776}`},
		{"rank-boundary", `{"rank":1000000}`},
		{"bitrate-boundary", `{"bitrate":1000000000}`},
		{"duration-boundary", `{"duration":604800,"duration_seconds":604800}`},
		{"duration-negative-zero", `{"DURATION":-0}`},
	}
	for _, test := range valid {
		t.Run(test.name, func(t *testing.T) {
			value, err := sanitizeVideoJSON([]byte(test.raw))
			if err != nil {
				t.Fatal(err)
			}
			if test.name == "duration-negative-zero" && value.(diagnosticObject)["DURATION"] != float64(0) {
				t.Fatalf("negative zero was retained: %#v", value)
			}
		})
	}
	for _, raw := range []string{
		`{"width":65536}`, `{"height":65536}`, `{"size":1099511627777}`, `{"file_size":1000000000000000}`,
		`{"rank":1000001}`, `{"bitrate":1000000001}`, `{"duration":604801}`, `{"duration_seconds":604801}`,
	} {
		if _, err := sanitizeVideoJSON([]byte(raw)); err == nil {
			t.Errorf("out-of-range media fact was accepted: %s", raw)
		}
	}
}

func TestDiagnosticOutputRemainsWithinMaximum(t *testing.T) {
	fields := make([]string, 0, 2000)
	for index := 0; index < 2000; index++ {
		fields = append(fields, fmt.Sprintf(`"field_%d":"synthetic-secret"`, index))
	}
	raw := []byte("{" + strings.Join(fields, ",") + "}")
	fs := openTestFS(t)
	path, err := writeVideoDiagnostic(fs, raw)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > maxVideoDiagnosticOutputBytes {
		t.Fatalf("diagnostic size = %d, limit = %d", len(data), maxVideoDiagnosticOutputBytes)
	}
}

func TestDiagnosticOwnerModesAreAppliedOnUnix(t *testing.T) {
	fs := openTestFS(t)
	root := filepath.Dir(mustDisplayPath(t, fs, diagnosticFilename))
	target := mustDisplayPath(t, fs, diagnosticFilename)
	if _, err := writeVideoDiagnostic(fs, []byte(`{"kind":"video"}`)); err != nil {
		t.Fatal(err)
	}
	rootInfo, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	targetInfo, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if rootInfo.Mode().Perm() != 0700 || targetInfo.Mode().Perm() != 0600 {
		t.Fatalf("modes = root %o target %o", rootInfo.Mode().Perm(), targetInfo.Mode().Perm())
	}
}

func TestDiagnosticRejectsOutputLargerThanLimitWithinOtherBounds(t *testing.T) {
	// Each branch has one value, so array length, fields, key bytes, and node
	// count stay bounded while repeated deep indentation exceeds the limit.
	branch := "0"
	for index := 0; index < maxVideoDiagnosticDepth-3; index++ {
		branch = `[` + branch + `]`
	}
	branches := make([]string, 150)
	for index := range branches {
		branches[index] = branch
	}
	raw := []byte(`{"values":[` + strings.Join(branches, ",") + `]}`)
	if len(raw) > maxVideoDiagnosticInputBytes {
		t.Fatal("output-limit fixture exceeded input limit")
	}
	fs := openTestFS(t)
	if _, err := writeVideoDiagnostic(fs, raw); err == nil || !strings.Contains(err.Error(), "output is too large") {
		t.Fatalf("output-limit fixture error = %v", err)
	}
}

func TestSanitizeVideoJSONIsDeterministicAndReplacementIsAtomic(t *testing.T) {
	first, err := sanitizeVideoJSON([]byte(`{"z":"secret","a":"https://example.test/?token=x","kind":"video"}`))
	if err != nil {
		t.Fatal(err)
	}
	second, err := sanitizeVideoJSON([]byte(`{"kind":"video","a":"different","z":"another"}`))
	if err != nil {
		t.Fatal(err)
	}
	firstData, _ := json.MarshalIndent(first, "", "  ")
	secondData, _ := json.MarshalIndent(second, "", "  ")
	if string(firstData) != string(secondData) {
		t.Fatalf("sanitized output is not deterministic:\n%s\n%s", firstData, secondData)
	}

	fs := openTestFS(t)
	path := mustDisplayPath(t, fs, diagnosticFilename)
	if err := os.WriteFile(path, []byte(`{"old":true}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := ClassifyVideos(fs, []pixieset.Set{videoSet(t, `{"kind":"video","old":false}`)}, Options{Videos: false}); !errors.Is(err, ErrUnsupportedVideo) {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"old": true`) {
		t.Fatalf("diagnostic was not replaced: %s", data)
	}
	temporary, globErr := filepath.Glob(filepath.Join(filepath.Dir(path), ".pixiegrabber-tmp-*"))
	if globErr != nil || len(temporary) != 0 {
		t.Fatalf("temporary files remain: %v, %v", temporary, globErr)
	}
}

func TestDiagnosticReplacementFailureCleansTemporaryFile(t *testing.T) {
	fs := openTestFS(t)
	path := mustDisplayPath(t, fs, diagnosticFilename)
	existing := []byte("{\"existing\":true}\n")
	if err := os.WriteFile(path, existing, 0600); err != nil {
		t.Fatal(err)
	}
	callbackErr := errors.New("synthetic write failure")
	err := fs.AtomicReplace(diagnosticFilename, func(w io.Writer) error {
		_, _ = w.Write([]byte("partial"))
		return callbackErr
	})
	if err == nil || !errors.Is(err, callbackErr) {
		t.Fatalf("replacement error = %v", err)
	}
	temporary, globErr := filepath.Glob(filepath.Join(filepath.Dir(path), ".pixiegrabber-tmp-*"))
	if globErr != nil || len(temporary) != 0 {
		t.Fatalf("temporary files remain after failure: %v, %v", temporary, globErr)
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(got, existing) {
		t.Fatalf("existing diagnostic changed on replacement failure: %q", got)
	}
}

func TestDiagnosticRejectsWrongTypeRootAndTarget(t *testing.T) {
	rootFile := filepath.Join(t.TempDir(), "root-file")
	if err := os.WriteFile(rootFile, []byte("synthetic"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := outputfs.Open(rootFile); err == nil {
		t.Fatal("file output root was accepted")
	}
	fs := openTestFS(t)
	root := filepath.Dir(mustDisplayPath(t, fs, diagnosticFilename))
	if err := os.Mkdir(filepath.Join(root, diagnosticFilename), 0700); err != nil {
		t.Fatal(err)
	}
	if err := ClassifyVideos(fs, []pixieset.Set{videoSet(t, `{"kind":"video"}`)}, Options{Videos: false}); err == nil {
		t.Fatal("directory diagnostic target was accepted")
	}
}

func TestDiagnosticRejectsSymlinkRootAndTarget(t *testing.T) {
	targetRoot := t.TempDir()
	linkRoot := filepath.Join(t.TempDir(), "root-link")
	if err := os.Symlink(targetRoot, linkRoot); err != nil {
		t.Skipf("symbolic links are not supported: %v", err)
	}
	if _, err := outputfs.Open(linkRoot); err == nil {
		t.Fatal("symbolic-link output root was accepted")
	}

	fs := openTestFS(t)
	root := filepath.Dir(mustDisplayPath(t, fs, diagnosticFilename))
	target := filepath.Join(root, diagnosticFilename)
	linkTarget := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(linkTarget, []byte("synthetic"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(linkTarget, target); err != nil {
		t.Skipf("symbolic links are not supported: %v", err)
	}
	if err := ClassifyVideos(fs, []pixieset.Set{videoSet(t, `{"kind":"video"}`)}, Options{Videos: false}); err == nil {
		t.Fatal("symbolic-link diagnostic target was accepted")
	}
}

func openTestFS(t *testing.T) *outputfs.FS {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks() error = %v", err)
	}
	f, err := outputfs.Open(filepath.Join(base, "output"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := f.Close(); err != nil {
			t.Errorf("cleanup Close() error = %v", err)
		}
	})
	return f
}

func mustDisplayPath(t *testing.T, fs *outputfs.FS, rel string) string {
	t.Helper()
	path, err := fs.DisplayPath(rel)
	if err != nil {
		t.Fatalf("DisplayPath(%q) error = %v", rel, err)
	}
	return path
}

func videoSet(t *testing.T, raw string) pixieset.Set {
	t.Helper()
	videoCount := strings.Count(raw, `},{`) + 1
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/api/v1/galleries/2" {
			t.Errorf("request path = %q", r.URL.Path)
			return
		}
		_, _ = io.WriteString(w, fmt.Sprintf(`{"data":{"id":"2","collection_id":"1","name":"Synthetic Set","description":"","photo_count":0,"video_count":%d,"rank":1,"photos":[],"videos":[%s]}}`, videoCount, raw))
	}))
	t.Cleanup(server.Close)
	client, err := pixieset.NewClient(server.URL, server.Client(), pixieset.WithUserAgent("PixiegrabberDiagnosticTest/1"))
	if err != nil {
		t.Fatal(err)
	}
	set, err := client.GetSet(context.Background(), "1", "2")
	if err != nil {
		t.Fatal(err)
	}
	return set
}
