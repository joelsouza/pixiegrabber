package pixieset

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const (
	defaultMaxResponseBodyBytes int64 = 64 << 20
	defaultMaxPages                   = 10000
)

var (
	// ErrHTTPStatus identifies an HTTP response outside the success range.
	ErrHTTPStatus = errors.New("pixieset: unexpected HTTP status")
	// ErrResponseTooLarge identifies a response rejected before JSON decoding.
	ErrResponseTooLarge  = errors.New("pixieset: response body exceeds limit")
	errRedirectsDisabled = errors.New("pixieset: redirects are disabled")
)

// HTTPError contains only safe response facts. It never includes a response
// body or request headers.
type HTTPError struct {
	Operation string
	ID        string
	Status    int
}

func (e *HTTPError) Error() string {
	if e.ID == "" {
		return fmt.Sprintf("%s: HTTP status %d", e.Operation, e.Status)
	}
	return fmt.Sprintf("%s for %s: HTTP status %d", e.Operation, e.ID, e.Status)
}

func (e *HTTPError) Unwrap() error { return ErrHTTPStatus }

// Client is the isolated Pixieset JSON API client.
type Client struct {
	baseURL              *url.URL
	httpClient           *http.Client
	userAgent            string
	referer              string
	maxResponseBodyBytes int64
	maxPages             int
}

// ClientOption changes a Client's local transport boundary.
type ClientOption func(*Client)

// WithUserAgent sets the browser-compatible User-Agent sent on every request.
func WithUserAgent(value string) ClientOption {
	return func(c *Client) { c.userAgent = value }
}

// WithMaxResponseBodyBytes bounds every JSON response body before decoding.
func WithMaxResponseBodyBytes(limit int64) ClientOption {
	return func(c *Client) { c.maxResponseBodyBytes = limit }
}

// WithMaxPages limits dashboard pages fetched by ListCollections.
func WithMaxPages(limit int) ClientOption {
	return func(c *Client) { c.maxPages = limit }
}

// WithMaxPageCount is an explicit synonym for WithMaxPages.
func WithMaxPageCount(limit int) ClientOption { return WithMaxPages(limit) }

// NewClient creates a client with the supplied API origin and HTTP client.
// The supplied HTTP client is shallow-cloned and is never modified.
func NewClient(baseURL string, supplied *http.Client, options ...ClientOption) (*Client, error) {
	if supplied == nil {
		return nil, errors.New("create client: HTTP client is nil")
	}
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return nil, fmt.Errorf("create client: invalid base URL: %w", err)
	}
	if err := validateBaseOrigin(parsed); err != nil {
		return nil, fmt.Errorf("create client: %w", err)
	}
	clone := *supplied
	clone.CheckRedirect = func(*http.Request, []*http.Request) error {
		return errRedirectsDisabled
	}
	client := &Client{
		baseURL:              parsed,
		httpClient:           &clone,
		maxResponseBodyBytes: defaultMaxResponseBodyBytes,
		maxPages:             defaultMaxPages,
	}
	for _, option := range options {
		if option != nil {
			option(client)
		}
	}
	if strings.TrimSpace(client.userAgent) == "" {
		return nil, errors.New("create client: User-Agent is required")
	}
	if len(client.userAgent) > maxStringBytes || strings.ContainsAny(client.userAgent, "\r\n") {
		return nil, errors.New("create client: User-Agent is invalid")
	}
	if client.maxResponseBodyBytes <= 0 || client.maxResponseBodyBytes >= int64(^uint64(0)>>1) {
		return nil, errors.New("create client: response body limit is invalid")
	}
	if client.maxPages <= 0 {
		return nil, errors.New("create client: page limit must be positive")
	}
	client.referer = parsed.Scheme + "://" + parsed.Host + "/collections"
	return client, nil
}

func validateBaseOrigin(value *url.URL) error {
	if value == nil || !value.IsAbs() || value.Host == "" || value.User != nil || value.RawQuery != "" || value.Fragment != "" {
		return errors.New("base URL must be an origin")
	}
	if isLoopbackHost(value.Hostname()) {
		if value.Scheme != "http" && value.Scheme != "https" {
			return errors.New("base URL has an invalid scheme")
		}
		if value.Path != "" && value.Path != "/" {
			return errors.New("base URL must not contain a path")
		}
		value.Path = ""
		return nil
	}
	if value.Scheme != "https" || value.Hostname() != "galleries.pixieset.com" || value.Port() != "" || (value.Path != "" && value.Path != "/") {
		return errors.New("base URL must be exactly https://galleries.pixieset.com")
	}
	value.Path = ""
	return nil
}

func isLoopbackHost(hostname string) bool {
	return hostname == "127.0.0.1" || hostname == "::1" || strings.EqualFold(hostname, "localhost")
}

// ListCollections discovers and validates every dashboard listing page.
func (c *Client) ListCollections(ctx context.Context) ([]Collection, error) {
	const operation = "list collections"
	collections := make([]Collection, 0)
	seen := make(map[string]struct{})
	for page := 1; ; page++ {
		if page > c.maxPages {
			return nil, operationError(operation, strconv.Itoa(page), errors.New("page limit exceeded"))
		}
		body, err := c.requestJSON(ctx, operation, strconv.Itoa(page), fmt.Sprintf("/api/v1/dashboard_listings?page=%d", page))
		if err != nil {
			return nil, operationError(operation, strconv.Itoa(page), err)
		}
		response, err := decodeDashboard(body)
		if err != nil {
			return nil, operationError(operation, strconv.Itoa(page), err)
		}
		current, last, err := validatePagination(response.Meta, page, c.maxPages)
		if err != nil {
			return nil, operationError(operation, strconv.Itoa(page), err)
		}
		if current != page {
			return nil, operationError(operation, strconv.Itoa(page), fmt.Errorf("response page is %d", current))
		}
		for index, item := range response.Data.Collections {
			collection, err := normalizeCollection(item)
			if err != nil {
				return nil, operationError(operation, strconv.Itoa(page), fmt.Errorf("collection %d: %w", index, err))
			}
			if _, exists := seen[collection.ID]; exists {
				return nil, operationError(operation, collection.ID, errors.New("duplicate Collection ID"))
			}
			seen[collection.ID] = struct{}{}
			collections = append(collections, collection)
		}
		if page == last {
			return collections, nil
		}
	}
}

// ListSets loads the Set summaries for one Collection.
func (c *Client) ListSets(ctx context.Context, collectionID string) ([]Set, error) {
	const operation = "list sets"
	id, err := normalizeID(collectionID)
	if err != nil {
		return nil, operationError(operation, collectionID, err)
	}
	body, err := c.requestJSON(ctx, operation, id, "/api/v1/collections/"+id+"/galleries")
	if err != nil {
		return nil, operationError(operation, id, err)
	}
	response, err := decodeSetList(body)
	if err != nil {
		return nil, operationError(operation, id, err)
	}
	sets := make([]Set, 0, len(response.Data))
	seen := make(map[string]struct{})
	for index, item := range response.Data {
		set, err := normalizeSet(item, id, "", false, c.baseURL)
		if err != nil {
			return nil, operationError(operation, id, fmt.Errorf("set %d: %w", index, err))
		}
		if _, exists := seen[set.ID]; exists {
			return nil, operationError(operation, set.ID, errors.New("duplicate Set ID"))
		}
		seen[set.ID] = struct{}{}
		sets = append(sets, set)
	}
	return sets, nil
}

// GetSet loads one Set and validates both the requested Collection and Set
// relationships before returning its photos and video presence information.
func (c *Client) GetSet(ctx context.Context, collectionID, setID string) (Set, error) {
	const operation = "get set"
	collection, err := normalizeID(collectionID)
	if err != nil {
		return Set{}, operationError(operation, collectionID, err)
	}
	setID, err = normalizeID(setID)
	if err != nil {
		return Set{}, operationError(operation, setID, err)
	}
	body, err := c.requestJSON(ctx, operation, setID, "/api/v1/galleries/"+setID+"?expand=photos.starred%2Cvideos")
	if err != nil {
		return Set{}, operationError(operation, setID, err)
	}
	response, err := decodeSet(body)
	if err != nil {
		return Set{}, operationError(operation, setID, err)
	}
	set, err := normalizeSet(response.Data, collection, setID, true, c.baseURL)
	if err != nil {
		return Set{}, operationError(operation, setID, err)
	}
	return set, nil
}

func operationError(operation, id string, err error) error {
	if id == "" {
		return fmt.Errorf("%s: %w", operation, err)
	}
	if _, normalizeErr := normalizeID(id); normalizeErr != nil {
		return fmt.Errorf("%s invalid ID: %w", operation, err)
	}
	return fmt.Errorf("%s %q: %w", operation, id, err)
}

func validatePagination(meta wireMeta, requested, maximum int) (int, int, error) {
	if !meta.CurrentPage.present || !meta.LastPage.present || meta.CurrentPage.null || meta.LastPage.null {
		return 0, 0, errors.New("pagination is incomplete")
	}
	current := meta.CurrentPage.value
	last := meta.LastPage.value
	if current <= 0 || last <= 0 || last < current || current != int64(requested) {
		return 0, 0, errors.New("pagination values are invalid")
	}
	if last > int64(maximum) {
		return 0, 0, errors.New("page limit exceeded")
	}
	if last > int64(^uint(0)>>1) {
		return 0, 0, errors.New("pagination value is too large")
	}
	return int(current), int(last), nil
}
