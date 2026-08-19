package pixieset

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"pixiegrabber/internal/throttle"
)

func TestClientCapturedContractsHeadersDatesVariantsAndVideos(t *testing.T) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	var gotHeaders http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/dashboard_listings":
			_, _ = io.WriteString(w, `{"data":{"data":{"collections":[{"id":1,"name":"Collection","description":"Description","photo_count":1,"video_count":0,"rank":4,"event_date":"2024-01-02T03:04:05-05:00","create_at":"2024-01-03 04:05:06"}]},"meta":{"current_page":1,"last_page":1}}}`)
		case "/api/v1/galleries/2":
			if r.URL.Query().Get("expand") != "photos.starred,videos" {
				t.Errorf("expand query = %q", r.URL.RawQuery)
			}
			_, _ = io.WriteString(w, `{"data":{"id":"2","collection_id":"1","name":"Set","description":"Set description","photo_count":1,"video_count":1,"rank":2,"photos":[{"id":"3","collection_id":"1","gallery_id":"2","name":"Photo","description":"Photo description","mime_type":"image/jpeg","ext":"jpeg","size":12,"width":20,"height":30,"rank":1,"capture_date":"2024-02-03T04:05:06-05:00","path_xxlarge":"//images.pixieset.com/xx.jpg","path_medium":"https://images.pixieset.com/medium.jpg"}],"videos":[{"kind":"video","safe":true}]}}`)
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	base, _ := url.Parse(server.URL)
	jar.SetCookies(base, []*http.Cookie{{Name: "GD-XSRF-TOKEN", Value: "encoded%2Ftoken%3D"}})
	client, err := NewClient(server.URL, &http.Client{Jar: jar}, WithUserAgent("PixiegrabberTest/1"))
	if err != nil {
		t.Fatal(err)
	}
	collections, err := client.ListCollections(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(collections) != 1 || collections[0].ID != "1" || collections[0].EventDate == nil || collections[0].CreatedAt == nil || collections[0].EventDate.Location() != time.UTC || collections[0].CreatedAt.Location() != time.UTC {
		t.Fatalf("collections = %#v", collections)
	}
	if gotHeaders.Get("User-Agent") != "PixiegrabberTest/1" || gotHeaders.Get("Accept") != "application/json" || gotHeaders.Get("X-Requested-With") != "XMLHttpRequest" || gotHeaders.Get("Referer") != server.URL+"/collections" || gotHeaders.Get("X-XSRF-TOKEN") != "encoded/token=" {
		t.Fatalf("request headers = %#v", gotHeaders)
	}

	set, err := client.GetSet(context.Background(), "1", "2")
	if err != nil {
		t.Fatal(err)
	}
	photo := set.Photos[0]
	if photo.Extension != "jpeg" || len(photo.ImageVariants) != 2 || photo.ImageVariants[0].Quality != "xxlarge" || photo.ImageVariants[1].Quality != "medium" || photo.ImageVariants[0].URL != "https://images.pixieset.com/xx.jpg" {
		t.Fatalf("photo variants = %#v", photo.ImageVariants)
	}
	if !set.HasVideos() {
		t.Fatal("set did not report videos")
	}
	video, ok := set.FirstVideo()
	if !ok || !strings.Contains(string(video), `"kind":"video"`) {
		t.Fatalf("first video = %q, %v", video, ok)
	}
	video[0] = 'x'
	videoAgain, _ := set.FirstVideo()
	if videoAgain[0] == 'x' {
		t.Fatal("FirstVideo did not return a copy")
	}
	serialized, _ := json.Marshal(set)
	if strings.Contains(string(serialized), `"kind":"video"`) {
		t.Fatalf("video was serialized: %s", serialized)
	}
}

func TestClientEndpointPathsAndMultiPageAccumulation(t *testing.T) {
	dashboardRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/dashboard_listings":
			dashboardRequests++
			wantURI := "/api/v1/dashboard_listings?page=" + stringInt(dashboardRequests)
			if r.URL.RequestURI() != wantURI {
				t.Errorf("dashboard request URI = %q, want %q", r.URL.RequestURI(), wantURI)
			}
			if dashboardRequests == 1 {
				_, _ = io.WriteString(w, dashboardPage(1, 2, `[{"id":"1","name":"One","description":"","photo_count":0,"video_count":0,"rank":0}]`))
				return
			}
			_, _ = io.WriteString(w, dashboardPage(2, 2, `[{"id":"2","name":"Two","description":"","photo_count":0,"video_count":0,"rank":0}]`))
		case "/api/v1/collections/1/galleries":
			if r.URL.RequestURI() != "/api/v1/collections/1/galleries" {
				t.Errorf("Set-list request URI = %q", r.URL.RequestURI())
			}
			_, _ = io.WriteString(w, `{"data":[{"id":"2","collection_id":"1","name":"Set","description":"","photo_count":0,"video_count":0,"rank":0}]}`)
		case "/api/v1/galleries/2":
			if r.URL.RequestURI() != "/api/v1/galleries/2?expand=photos.starred%2Cvideos" {
				t.Errorf("Set-content request URI = %q", r.URL.RequestURI())
			}
			_, _ = io.WriteString(w, setResponse("0", "", "[]", 0))
		default:
			t.Errorf("unexpected request URI %q", r.URL.RequestURI())
		}
	}))
	defer server.Close()

	client, err := NewClient(server.URL, server.Client(), WithUserAgent("test"))
	if err != nil {
		t.Fatal(err)
	}
	collections, err := client.ListCollections(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(collections) != 2 || collections[0].ID != "1" || collections[1].ID != "2" {
		t.Fatalf("collections = %#v", collections)
	}
	if _, err := client.ListSets(context.Background(), "1"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.GetSet(context.Background(), "1", "2"); err != nil {
		t.Fatal(err)
	}
}

func TestClientRejectsDuplicateCollectionIDsAcrossPages(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		if r.URL.RequestURI() != "/api/v1/dashboard_listings?page="+stringInt(requests) {
			t.Errorf("dashboard request URI = %q", r.URL.RequestURI())
		}
		_, _ = io.WriteString(w, dashboardPage(requests, 2, `[{"id":"1","name":"Collection","description":"","photo_count":0,"video_count":0,"rank":0}]`))
	}))
	defer server.Close()

	client, err := NewClient(server.URL, server.Client(), WithUserAgent("test"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.ListCollections(context.Background())
	if err == nil || !strings.Contains(err.Error(), `duplicate Collection ID`) || !strings.Contains(err.Error(), `"1"`) {
		t.Fatalf("duplicate Collection result = %v", err)
	}
	if requests != 2 {
		t.Fatalf("requests = %d, want 2", requests)
	}
}

func TestClientRejectsInvalidDashboardPagination(t *testing.T) {
	tests := map[string]struct {
		meta string
		want string
	}{
		"missing current page":  {meta: `{"last_page":1}`, want: "pagination is incomplete"},
		"missing last page":     {meta: `{"current_page":1}`, want: "pagination is incomplete"},
		"null current page":     {meta: `{"current_page":null,"last_page":1}`, want: "pagination is incomplete"},
		"null last page":        {meta: `{"current_page":1,"last_page":null}`, want: "pagination is incomplete"},
		"current page mismatch": {meta: `{"current_page":2,"last_page":2}`, want: "pagination values are invalid"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, dashboardResponseWithMeta(test.meta))
			}))
			defer server.Close()
			client, err := NewClient(server.URL, server.Client(), WithUserAgent("test"))
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.ListCollections(context.Background())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("pagination error = %v, want %q", err, test.want)
			}
		})
	}

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		if requests == 1 {
			_, _ = io.WriteString(w, dashboardPage(1, 2, "[]"))
			return
		}
		_, _ = io.WriteString(w, dashboardPage(2, 1, "[]"))
	}))
	defer server.Close()
	client, err := NewClient(server.URL, server.Client(), WithUserAgent("test"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.ListCollections(context.Background())
	if err == nil || !strings.Contains(err.Error(), "pagination values are invalid") {
		t.Fatalf("last-page regression error = %v", err)
	}
	if requests != 2 {
		t.Fatalf("last-page regression requests = %d, want 2", requests)
	}
}

func TestClientConfigurationClonesAndDeniesRedirects(t *testing.T) {
	if _, err := NewClient("http://127.0.0.1:1", nil, WithUserAgent("test")); err == nil {
		t.Fatal("nil HTTP client was accepted")
	}
	if _, err := NewClient("http://127.0.0.1:1", &http.Client{}); err == nil {
		t.Fatal("missing User-Agent was accepted")
	}
	for _, base := range []string{"http://galleries.pixieset.com", "https://galleries.pixieset.com:443", "https://galleries.pixieset.com/path", "https://not-galleries.pixieset.com"} {
		if _, err := NewClient(base, &http.Client{}, WithUserAgent("test")); err == nil {
			t.Fatalf("invalid production origin accepted: %s", base)
		}
	}

	redirects := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/dashboard_listings" {
			redirects++
			http.Redirect(w, r, "/redirect-target", http.StatusFound)
			return
		}
		redirects++
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, dashboardPage(1, 1, "[]"))
	}))
	defer server.Close()
	supplied := &http.Client{}
	client, err := NewClient(server.URL, supplied, WithUserAgent("test"))
	if err != nil {
		t.Fatal(err)
	}
	if client.httpClient == supplied || client.httpClient.CheckRedirect == nil {
		t.Fatal("supplied HTTP client was not cloned and configured")
	}
	_, err = client.ListCollections(context.Background())
	if err == nil || redirects != 1 || strings.Contains(err.Error(), "redirect-target") {
		t.Fatalf("redirect result = %v, redirects = %d", err, redirects)
	}
}

func TestClientDashboardPageCapAndExactResponseShape(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, dashboardPage(1, 2, "[]"))
	}))
	defer server.Close()
	client, err := NewClient(server.URL, server.Client(), WithUserAgent("test"), WithMaxPages(1))
	if err != nil {
		t.Fatal(err)
	}
	if client.maxResponseBodyBytes != 64<<20 || client.maxPages != 1 {
		t.Fatalf("defaults/options = %d/%d", client.maxResponseBodyBytes, client.maxPages)
	}
	_, err = client.ListCollections(context.Background())
	if err == nil || requests != 1 || !strings.Contains(err.Error(), "page limit") {
		t.Fatalf("page cap result = %v, requests = %d", err, requests)
	}

	legacy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[],"meta":{"current_page":1,"last_page":1}}`)
	}))
	defer legacy.Close()
	client, _ = NewClient(legacy.URL, legacy.Client(), WithUserAgent("test"))
	if _, err = client.ListCollections(context.Background()); err == nil {
		t.Fatal("legacy dashboard response was accepted")
	}
}

func TestClientValidatesIDsDuplicatesAndPhotoFields(t *testing.T) {
	for _, id := range []string{"", "abc", "123456789012345678901"} {
		client, err := NewClient("http://127.0.0.1:1", &http.Client{}, WithUserAgent("test"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = client.ListSets(context.Background(), id); err == nil {
			t.Fatalf("invalid input ID accepted: %q", id)
		}
	}

	duplicateCollection := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, dashboardPage(1, 1, `[{"id":"1","name":"A","photo_count":0,"video_count":0,"rank":0},{"id":"1","name":"B","photo_count":0,"video_count":0,"rank":0}]`))
	}))
	defer duplicateCollection.Close()
	client, _ := NewClient(duplicateCollection.URL, duplicateCollection.Client(), WithUserAgent("test"))
	if _, err := client.ListCollections(context.Background()); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate Collection result = %v", err)
	}

	for name, test := range map[string]struct {
		photo string
		want  string
	}{
		"missing name":   {photo: `{"id":"3","collection_id":"1","gallery_id":"2","mime_type":"image/jpeg","ext":"jpg","size":1,"width":1,"height":1,"path_medium":"//images.pixieset.com/a.jpg"}`, want: "required"},
		"bad MIME":       {photo: `{"id":"3","collection_id":"1","gallery_id":"2","name":"Photo","mime_type":"video/mp4","ext":"mp4","size":1,"width":1,"height":1,"path_medium":"//images.pixieset.com/a.jpg"}`, want: "image type"},
		"bad extension":  {photo: `{"id":"3","collection_id":"1","gallery_id":"2","name":"Photo","mime_type":"image/jpeg","ext":"","size":1,"width":1,"height":1,"path_medium":"//images.pixieset.com/a.jpg"}`, want: "extension"},
		"bad size":       {photo: `{"id":"3","collection_id":"1","gallery_id":"2","name":"Photo","mime_type":"image/jpeg","ext":"jpg","size":0,"width":1,"height":1,"path_medium":"//images.pixieset.com/a.jpg"}`, want: "positive"},
		"bad dimensions": {photo: `{"id":"3","collection_id":"1","gallery_id":"2","name":"Photo","mime_type":"image/jpeg","ext":"jpg","size":1,"width":0,"height":1,"path_medium":"//images.pixieset.com/a.jpg"}`, want: "positive"},
		"bad height":     {photo: `{"id":"3","collection_id":"1","gallery_id":"2","name":"Photo","mime_type":"image/jpeg","ext":"jpg","size":1,"width":1,"height":0,"path_medium":"//images.pixieset.com/a.jpg"}`, want: "positive"},
		"count mismatch": {photo: `{"id":"3","collection_id":"1","gallery_id":"2","name":"Photo","mime_type":"image/jpeg","ext":"jpg","size":1,"width":1,"height":1,"path_medium":"//images.pixieset.com/a.jpg"}`, want: "does not match"},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			count := "1"
			if name == "count mismatch" {
				count = "2"
			}
			_, _ = io.WriteString(w, setResponse(count, test.photo, "[]", 0))
		}))
		client, _ := NewClient(server.URL, server.Client(), WithUserAgent("test"))
		_, err := client.GetSet(context.Background(), "1", "2")
		server.Close()
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: error = %v", name, err)
		}
	}
}

func TestClientRejectsMissingRequiredNumericFields(t *testing.T) {
	collectionFields := []struct {
		name  string
		field string
		want  string
	}{
		{name: "photo count", field: "photo_count", want: "Collection photo count is missing"},
		{name: "video count", field: "video_count", want: "Collection video count is missing"},
	}
	for _, test := range collectionFields {
		t.Run("Collection "+test.name, func(t *testing.T) {
			collection := map[string]any{
				"id":          "1",
				"name":        "Collection",
				"description": "",
				"photo_count": 0,
				"video_count": 0,
				"rank":        0,
			}
			delete(collection, test.field)
			encoded, err := json.Marshal(collection)
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, dashboardPage(1, 1, "["+string(encoded)+"]"))
			}))
			defer server.Close()
			client, err := NewClient(server.URL, server.Client(), WithUserAgent("test"))
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.ListCollections(context.Background())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("missing %s error = %v, want %q", test.name, err, test.want)
			}
		})
	}

	setFields := []struct {
		name  string
		field string
		want  string
	}{
		{name: "photo count", field: "photo_count", want: "Set photo count is missing"},
	}
	for _, test := range setFields {
		t.Run("Set "+test.name, func(t *testing.T) {
			set := map[string]any{
				"id":            "2",
				"collection_id": "1",
				"name":          "Set",
				"description":   "",
				"photo_count":   0,
				"video_count":   0,
				"rank":          0,
			}
			delete(set, test.field)
			encoded, err := json.Marshal(set)
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"data":[`+string(encoded)+`]}`)
			}))
			defer server.Close()
			client, err := NewClient(server.URL, server.Client(), WithUserAgent("test"))
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.ListSets(context.Background(), "1")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("missing %s error = %v, want %q", test.name, err, test.want)
			}
		})
	}

	photoFields := []struct {
		name  string
		field string
		want  string
	}{
		{name: "size", field: "size", want: "photo size is missing"},
		{name: "width", field: "width", want: "photo width is missing"},
		{name: "height", field: "height", want: "photo height is missing"},
		{name: "rank", field: "rank", want: "photo rank is missing"},
	}
	for _, test := range photoFields {
		t.Run("Photo "+test.name, func(t *testing.T) {
			photo := map[string]any{}
			if err := json.Unmarshal([]byte(validPhoto("3", "2")), &photo); err != nil {
				t.Fatal(err)
			}
			delete(photo, test.field)
			encoded, err := json.Marshal(photo)
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, setResponse("1", string(encoded), "[]", 0))
			}))
			defer server.Close()
			client, err := NewClient(server.URL, server.Client(), WithUserAgent("test"))
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.GetSet(context.Background(), "1", "2")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("missing %s error = %v, want %q", test.name, err, test.want)
			}
		})
	}
}

func TestNormalizeRejectsMissingIDsAndRelationships(t *testing.T) {
	collection := wireCollection{
		ID:         wireID{value: "1", present: true},
		Name:       "Collection",
		PhotoCount: wireInt{value: 0, present: true},
		VideoCount: wireInt{value: 0, present: true},
	}
	collection.ID = wireID{}
	if _, err := normalizeCollection(collection); err == nil || err.Error() != "collection ID is missing" {
		t.Fatalf("missing Collection ID error = %v", err)
	}

	set := wireSet{
		ID:           wireID{value: "2", present: true},
		CollectionID: wireID{value: "1", present: true},
		Name:         "Set",
		PhotoCount:   wireInt{value: 0, present: true},
		VideoCount:   wireInt{value: 0, present: true},
		Rank:         wireInt{value: 0, present: true},
	}
	missingSetID := set
	missingSetID.ID = wireID{}
	if _, err := normalizeSet(missingSetID, "1", "", false, nil); err == nil || err.Error() != "Set ID is missing" {
		t.Fatalf("missing Set ID error = %v", err)
	}
	missingCollectionID := set
	missingCollectionID.CollectionID = wireID{}
	if _, err := normalizeSet(missingCollectionID, "1", "", false, nil); err == nil || err.Error() != "Collection ID is missing" {
		t.Fatalf("missing Set Collection ID error = %v", err)
	}
	wrongCollectionID := set
	wrongCollectionID.CollectionID = wireID{value: "9", present: true}
	if _, err := normalizeSet(wrongCollectionID, "1", "", false, nil); err == nil || !strings.Contains(err.Error(), "wrong Collection relationship") {
		t.Fatalf("wrong Set Collection relationship error = %v", err)
	}

	photo := wirePhoto{
		ID:           wireID{value: "3", present: true},
		CollectionID: wireID{value: "1", present: true},
		GalleryID:    wireID{value: "2", present: true},
		Name:         "Photo",
		MIMEType:     "image/jpeg",
		Extension:    "jpg",
		Size:         wireInt{value: 1, present: true},
		Width:        wireInt{value: 1, present: true},
		Height:       wireInt{value: 1, present: true},
		Rank:         wireInt{value: 0, present: true},
		PathMedium:   "//images.pixieset.com/photo.jpg",
	}
	if normalized, err := normalizePhoto(photo, "1", "2", nil); err != nil || normalized.Rank != 0 {
		t.Fatalf("zero photo rank result = %#v, error = %v", normalized, err)
	}
	missingPhotoID := photo
	missingPhotoID.ID = wireID{}
	if _, err := normalizePhoto(missingPhotoID, "1", "2", nil); err == nil || err.Error() != "photo ID is missing" {
		t.Fatalf("missing photo ID error = %v", err)
	}
	missingPhotoCollectionID := photo
	missingPhotoCollectionID.CollectionID = wireID{}
	if _, err := normalizePhoto(missingPhotoCollectionID, "1", "2", nil); err == nil || err.Error() != "photo Collection ID is missing" {
		t.Fatalf("missing photo Collection ID error = %v", err)
	}
	missingPhotoSetID := photo
	missingPhotoSetID.GalleryID = wireID{}
	if _, err := normalizePhoto(missingPhotoSetID, "1", "2", nil); err == nil || err.Error() != "Set ID is missing" {
		t.Fatalf("missing photo Set ID error = %v", err)
	}
	wrongPhotoCollectionID := photo
	wrongPhotoCollectionID.CollectionID = wireID{value: "9", present: true}
	if _, err := normalizePhoto(wrongPhotoCollectionID, "1", "2", nil); err == nil || !strings.Contains(err.Error(), "invalid relationship") {
		t.Fatalf("wrong photo Collection relationship error = %v", err)
	}
	wrongPhotoSetID := photo
	wrongPhotoSetID.GalleryID = wireID{value: "9", present: true}
	if _, err := normalizePhoto(wrongPhotoSetID, "1", "2", nil); err == nil || !strings.Contains(err.Error(), "invalid relationship") {
		t.Fatalf("wrong photo Set relationship error = %v", err)
	}
}

func TestClientRejectsDuplicateSetsPhotosAndBadRelationships(t *testing.T) {
	set := `{"id":"2","collection_id":"1","name":"Set","photo_count":0,"video_count":0,"rank":1,"photos":[]}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/collections/1/galleries" {
			_, _ = io.WriteString(w, `{"data":[`+set+`,`+set+`]}`)
			return
		}
		_, _ = io.WriteString(w, setResponse("2", validPhoto("3", "2")+","+validPhoto("3", "2"), "[]", 0))
	}))
	defer server.Close()
	client, _ := NewClient(server.URL, server.Client(), WithUserAgent("test"))
	if _, err := client.ListSets(context.Background(), "1"); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate Set result = %v", err)
	}
	if _, err := client.GetSet(context.Background(), "1", "2"); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate photo result = %v", err)
	}
}

func TestClientHTTPBoundsContentTypeAndCauseDoNotLeak(t *testing.T) {
	secret := "source-name-secret-media-url-token"
	statusServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, `{"error":"`+secret+`"}`)
	}))
	defer statusServer.Close()
	client, _ := NewClient(statusServer.URL, statusServer.Client(), WithUserAgent("test"))
	client.sleep = func(context.Context, time.Duration) error { return nil }
	_, err := client.ListCollections(context.Background())
	var httpErr *HTTPError
	if err == nil || !errors.As(err, &httpErr) || httpErr.Status != http.StatusBadGateway || strings.Contains(err.Error(), secret) {
		t.Fatalf("status error = %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("X-Secret", secret)
		_, _ = io.WriteString(w, secret)
	}))
	defer server.Close()
	client, _ = NewClient(server.URL, server.Client(), WithUserAgent("test"))
	_, err = client.ListCollections(context.Background())
	if err == nil || !strings.Contains(err.Error(), "content type") || strings.Contains(err.Error(), secret) {
		t.Fatalf("content-type error = %v", err)
	}

	large := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":{"data":{"collections":`+strings.Repeat(" ", 1024)+`[]},"meta":{"current_page":1,"last_page":1}}}`)
	}))
	defer large.Close()
	client, _ = NewClient(large.URL, large.Client(), WithUserAgent("test"), WithMaxResponseBodyBytes(100))
	_, err = client.ListCollections(context.Background())
	if !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("bounded-body error = %v", err)
	}

	transportErr := errors.New("synthetic transport failure")
	client, _ = NewClient("http://127.0.0.1:1", &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, transportErr
	})}, WithUserAgent("test"))
	_, err = client.ListCollections(context.Background())
	if !errors.Is(err, transportErr) {
		t.Fatalf("transport cause was lost: %v", err)
	}
}

func TestClientThrottleSpacesSequentialRequests(t *testing.T) {
	const interval = 30 * time.Millisecond
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, setResponse("0", "", "[]", 0))
	}))
	defer server.Close()
	client, err := NewClient(server.URL, server.Client(), WithUserAgent("test"), WithThrottle(throttle.New(interval)))
	if err != nil {
		t.Fatal(err)
	}
	const n = 5
	start := time.Now()
	for i := 0; i < n; i++ {
		if _, err := client.GetSet(context.Background(), "1", "2"); err != nil {
			t.Fatal(err)
		}
	}
	elapsed := time.Since(start)
	minExpected := time.Duration(n-1) * interval
	if elapsed < minExpected {
		t.Fatalf("elapsed = %v, want at least %v", elapsed, minExpected)
	}
}

func TestClientThrottleZeroIntervalDoesNotSlow(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, setResponse("0", "", "[]", 0))
	}))
	defer server.Close()
	client, err := NewClient(server.URL, server.Client(), WithUserAgent("test"), WithThrottle(throttle.New(0)))
	if err != nil {
		t.Fatal(err)
	}
	const n = 20
	start := time.Now()
	for i := 0; i < n; i++ {
		if _, err := client.GetSet(context.Background(), "1", "2"); err != nil {
			t.Fatal(err)
		}
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("zero interval slowed requests: %v", elapsed)
	}
}

func TestClientRetries429HonoringRetryAfter(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, setResponse("0", "", "[]", 0))
	}))
	defer server.Close()
	client, err := NewClient(server.URL, server.Client(), WithUserAgent("test"))
	if err != nil {
		t.Fatal(err)
	}
	var gotDelay time.Duration
	client.sleep = func(ctx context.Context, delay time.Duration) error {
		gotDelay = delay
		return nil
	}
	if _, err := client.GetSet(context.Background(), "1", "2"); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2", calls.Load())
	}
	if gotDelay != 2*time.Second {
		t.Fatalf("retry delay = %v, want 2s", gotDelay)
	}
}

// Pixieset sends no rank for a Collection or a Set. Only an image record has
// one. These tests hold the real response shape.
func TestListCollectionsAcceptsAResponseWithoutARank(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, dashboardPage(1, 1,
			`[{"id":"117855994","name":"Collection","photo_count":131,"video_count":0}]`))
	}))
	defer server.Close()
	client, err := NewClient(server.URL, server.Client(), WithUserAgent("test"))
	if err != nil {
		t.Fatal(err)
	}
	collections, err := client.ListCollections(context.Background())
	if err != nil {
		t.Fatalf("ListCollections() error = %v", err)
	}
	if len(collections) != 1 || collections[0].ID != "117855994" || collections[0].PhotoCount != 131 {
		t.Fatalf("ListCollections() = %#v", collections)
	}
}

func TestListSetsOrdersSetsByPositionWhenTheResponseHasNoRank(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[`+
			`{"id":"169355728","collection_id":"1","name":"First","photo_count":122,"video_count":0},`+
			`{"id":"169736640","collection_id":"1","name":"Second","photo_count":9,"video_count":0}]}`)
	}))
	defer server.Close()
	client, err := NewClient(server.URL, server.Client(), WithUserAgent("test"))
	if err != nil {
		t.Fatal(err)
	}
	sets, err := client.ListSets(context.Background(), "1")
	if err != nil {
		t.Fatalf("ListSets() error = %v", err)
	}
	if len(sets) != 2 {
		t.Fatalf("ListSets() returned %d Sets", len(sets))
	}
	// The response order gives the display order, and ranks start at 1.
	if sets[0].Rank != 1 || sets[1].Rank != 2 {
		t.Fatalf("Set ranks = %d and %d, want 1 and 2", sets[0].Rank, sets[1].Rank)
	}
	if sets[0].Name != "First" || sets[1].Name != "Second" {
		t.Fatalf("Set order = %q then %q", sets[0].Name, sets[1].Name)
	}
}

// The single-Set response is thinner than the Set list. It sends no video
// count and no rank, and it can send a null description.
func TestGetSetAcceptsTheThinSingleSetResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":{"id":"169355728","collection_id":"117855994",`+
			`"name":"Destaques","description":null,"photo_count":1,"private":false,"download":true,`+
			`"photos":[`+validPhotoInCollection("3", "169355728", "117855994")+`],"videos":[]}}`)
	}))
	defer server.Close()
	client, err := NewClient(server.URL, server.Client(), WithUserAgent("test"))
	if err != nil {
		t.Fatal(err)
	}
	set, err := client.GetSet(context.Background(), "117855994", "169355728")
	if err != nil {
		t.Fatalf("GetSet() error = %v", err)
	}
	if set.Name != "Destaques" || set.Description != "" || len(set.Photos) != 1 {
		t.Fatalf("GetSet() = %#v", set)
	}
	if set.VideoCount != 0 || set.HasVideos() {
		t.Fatalf("video count = %d, HasVideos = %v", set.VideoCount, set.HasVideos())
	}
}

func TestGetSetCountsTheVideosItReceives(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":{"id":"2","collection_id":"1","name":"Set",`+
			`"photo_count":0,"photos":[],"videos":[{"kind":"video"},{"kind":"video"}]}}`)
	}))
	defer server.Close()
	client, err := NewClient(server.URL, server.Client(), WithUserAgent("test"))
	if err != nil {
		t.Fatal(err)
	}
	set, err := client.GetSet(context.Background(), "1", "2")
	if err != nil {
		t.Fatalf("GetSet() error = %v", err)
	}
	// No video count arrives, so the videos themselves give it.
	if set.VideoCount != 2 || !set.HasVideos() {
		t.Fatalf("video count = %d, want 2", set.VideoCount)
	}
}

func TestGetSetNormalizesAVideo(t *testing.T) {
	video := `{"id":"133871182","provider_id":3,"name":"Clip","width":1080,"height":1620,"mux_status":2,"metadata":"{\"status\":\"ready\",\"duration\":8.8,\"mp4_support\":\"standard\",\"static_renditions\":{\"status\":\"ready\",\"files\":[{\"name\":\"high.mp4\",\"ext\":\"mp4\",\"width\":1080,\"height\":1620,\"filesize\":2152769},{\"name\":\"medium.mp4\",\"ext\":\"mp4\",\"width\":480,\"height\":720,\"filesize\":243234},{\"name\":\"low.mp4\",\"ext\":\"mp4\",\"width\":270,\"height\":404,\"filesize\":106595}]}}","video_source":"https://stream.mux.com/PLAYBACKID.m3u8?token=synthetic-video-token"}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, setResponse("0", "", "["+video+"]", 1))
	}))
	defer server.Close()
	client, err := NewClient(server.URL, server.Client(), WithUserAgent("test"))
	if err != nil {
		t.Fatal(err)
	}
	set, err := client.GetSet(context.Background(), "1", "2")
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Videos) != 1 {
		t.Fatalf("videos = %d, want 1", len(set.Videos))
	}
	v := set.Videos[0]
	if v.ID != "133871182" || v.Name != "Clip" || v.Width != 1080 || v.Height != 1620 || v.MIMEType != "video/mp4" || v.Extension != "mp4" || v.Size != 2152769 || v.DurationSeconds != 8.8 || v.MuxStatus != 2 {
		t.Fatalf("video = %#v", v)
	}
	if len(v.Variants) != 3 || v.Variants[0].Quality != "high" || v.Variants[1].Quality != "medium" || v.Variants[2].Quality != "low" {
		t.Fatalf("variants = %#v", v.Variants)
	}
	serialized, _ := json.Marshal(set)
	if strings.Contains(string(serialized), "synthetic-video-token") || strings.Contains(string(serialized), "stream.mux.com") {
		t.Fatalf("URL leaked into serialized Set: %s", serialized)
	}
}

func TestGetSetRejectsUnrecognizedVideos(t *testing.T) {
	tests := map[string]struct {
		video string
	}{
		"metadata over byte bound": {video: `{"id":"1","provider_id":3,"name":"Clip","mux_status":2,"metadata":"` + strings.Repeat("a", maxVideoMetadataBytes+1) + `","video_source":"https://stream.mux.com/PLAYBACKID.m3u8?token=synthetic-video-token"}`},
		"unsafe rendition name":    {video: `{"id":"1","provider_id":3,"name":"Clip","mux_status":2,"metadata":"{\"status\":\"ready\",\"static_renditions\":{\"files\":[{\"name\":\"../../evil.mp4\"}]}}","video_source":"https://stream.mux.com/PLAYBACKID.m3u8?token=synthetic-video-token"}`},
		"wrong video host":         {video: `{"id":"1","provider_id":3,"name":"Clip","mux_status":2,"metadata":"{\"status\":\"ready\",\"static_renditions\":{\"files\":[{\"name\":\"high.mp4\"}]}}","video_source":"https://images.pixieset.com/PLAYBACKID.m3u8?token=synthetic-video-token"}`},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, setResponse("0", "", "["+test.video+"]", 1))
			}))
			defer server.Close()
			client, err := NewClient(server.URL, server.Client(), WithUserAgent("test"))
			if err != nil {
				t.Fatal(err)
			}
			set, err := client.GetSet(context.Background(), "1", "2")
			if err != nil {
				t.Fatal(err)
			}
			if len(set.Videos) != 0 {
				t.Fatalf("videos = %d, want 0", len(set.Videos))
			}
			raw, ok := set.FirstUnrecognizedVideo()
			if !ok || !strings.Contains(string(raw), `"id":"1"`) {
				t.Fatalf("unrecognized video = %q, %v", raw, ok)
			}
		})
	}
}

func TestListSetsKeepsARankThatPixiesetSends(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[`+
			`{"id":"2","collection_id":"1","name":"Only","photo_count":0,"video_count":0,"rank":7}]}`)
	}))
	defer server.Close()
	client, err := NewClient(server.URL, server.Client(), WithUserAgent("test"))
	if err != nil {
		t.Fatal(err)
	}
	sets, err := client.ListSets(context.Background(), "1")
	if err != nil {
		t.Fatalf("ListSets() error = %v", err)
	}
	if len(sets) != 1 || sets[0].Rank != 7 {
		t.Fatalf("Set rank = %#v, want the rank from the response", sets)
	}
}

func dashboardPage(current, last int, collections string) string {
	return `{"data":{"data":{"collections":` + collections + `},"meta":{"current_page":` + stringInt(current) + `,"last_page":` + stringInt(last) + `}}}`
}

func dashboardResponseWithMeta(meta string) string {
	return `{"data":{"data":{"collections":[]},"meta":` + meta + `}}`
}

func setResponse(count, photos, videos string, videoCount int) string {
	return `{"data":{"id":"2","collection_id":"1","name":"Set","description":"","photo_count":` + count + `,"video_count":` + stringInt(videoCount) + `,"rank":1,"photos":[` + photos + `],"videos":` + videos + `}}`
}

func validPhoto(id, setID string) string {
	return `{"id":"` + id + `","collection_id":"1","gallery_id":"` + setID + `","name":"Photo","description":"","mime_type":"image/jpeg","ext":"jpg","size":1,"width":1,"height":1,"rank":1,"path_medium":"//images.pixieset.com/photo.jpg"}`
}

func stringInt(value int) string {
	return fmt.Sprintf("%d", value)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func validPhotoInCollection(id, setID, collectionID string) string {
	return `{"id":"` + id + `","collection_id":"` + collectionID + `","gallery_id":"` + setID + `","name":"Photo","description":"","mime_type":"image/jpeg","ext":"jpg","size":1,"width":1,"height":1,"rank":1,"path_medium":"//images.pixieset.com/photo.jpg"}`
}
