package pixieset

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
)

func (c *Client) requestJSON(ctx context.Context, operation, subject, path string) ([]byte, error) {
	requestURL := *c.baseURL
	parsedPath, err := url.Parse(path)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	requestURL.Path = parsedPath.Path
	requestURL.RawPath = parsedPath.RawPath
	requestURL.RawQuery = parsedPath.RawQuery
	requestURL.Fragment = ""
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Referer", c.referer)
	if err := c.setXSRFHeader(req, &requestURL); err != nil {
		return nil, err
	}
	response, err := c.httpClient.Do(req)
	if err != nil {
		if errors.Is(err, errRedirectsDisabled) {
			return nil, errRedirectsDisabled
		}
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, &HTTPError{Operation: operation, ID: subject, Status: response.StatusCode}
	}
	contentType, _, parseErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if parseErr != nil || contentType != "application/json" {
		return nil, errors.New("response content type is not JSON")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, c.maxResponseBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}
	if int64(len(body)) > c.maxResponseBodyBytes {
		return nil, ErrResponseTooLarge
	}
	return body, nil
}

func (c *Client) setXSRFHeader(req *http.Request, requestURL *url.URL) error {
	if c.httpClient.Jar == nil {
		return nil
	}
	for _, cookie := range c.httpClient.Jar.Cookies(requestURL) {
		if cookie.Name != "GD-XSRF-TOKEN" {
			continue
		}
		decoded, err := url.PathUnescape(cookie.Value)
		if err != nil {
			return fmt.Errorf("decode GD-XSRF-TOKEN cookie: %w", err)
		}
		req.Header.Set("X-XSRF-TOKEN", decoded)
		break
	}
	return nil
}
