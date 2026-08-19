package pixieset

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"pixiegrabber/internal/throttle"
)

// maxRequestAttempts bounds retries after transient 429/5xx responses.
const maxRequestAttempts = 5

// reportWaitFrom is the shortest retry wait that the person sees. A short
// backoff stays in the log only, so the terminal does not fill with noise.
const reportWaitFrom = 2 * time.Second

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

	for attempt := 0; attempt < maxRequestAttempts; attempt++ {
		if c.lim != nil {
			if err := c.lim.Wait(ctx); err != nil {
				return nil, err
			}
		}
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
		status := response.StatusCode
		if status >= http.StatusOK && status < http.StatusMultipleChoices {
			contentType, _, parseErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
			if parseErr != nil || contentType != "application/json" {
				response.Body.Close()
				return nil, errors.New("response content type is not JSON")
			}
			body, readErr := io.ReadAll(io.LimitReader(response.Body, c.maxResponseBodyBytes+1))
			response.Body.Close()
			if readErr != nil {
				return nil, fmt.Errorf("read response body: %w", readErr)
			}
			if int64(len(body)) > c.maxResponseBodyBytes {
				return nil, ErrResponseTooLarge
			}
			return body, nil
		}
		response.Body.Close()
		if !isRetryableStatus(status) {
			return nil, &HTTPError{Operation: operation, ID: subject, Status: status}
		}
		if attempt == maxRequestAttempts-1 {
			return nil, &HTTPError{Operation: operation, ID: subject, Status: status}
		}
		header := response.Header.Get("Retry-After")
		delay := retryDelay(header, attempt)
		reason := "backoff"
		if strings.TrimSpace(header) != "" {
			reason = "retry_after"
		}
		// Pixieset can ask for a long wait. Without this line the tool looks
		// dead while it obeys.
		line := ""
		if delay >= reportWaitFrom {
			line = fmt.Sprintf("Pixieset asked to wait %s (HTTP %d). Attempt %d of %d.",
				delay.Round(time.Second), status, attempt+1, maxRequestAttempts)
		}
		c.reportProgress(line, "retry_wait", map[string]any{
			"status":  status,
			"attempt": attempt + 1,
			"of":      maxRequestAttempts,
			"seconds": delay.Seconds(),
			"reason":  reason,
		})
		if err := c.sleep(ctx, delay); err != nil {
			return nil, err
		}
	}
	return nil, &HTTPError{Operation: operation, ID: subject, Status: 0}
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

// isRetryableStatus reports whether a response status should be retried with
// backoff. It covers 429 and the transient 5xx statuses.
func isRetryableStatus(status int) bool {
	switch status {
	case http.StatusRequestTimeout, 425, http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

// retryDelay returns the wait before a retry. It prefers the Retry-After
// header seconds when present, otherwise falls back to exponential backoff.
func retryDelay(value string, attempt int) time.Duration {
	value = strings.TrimSpace(value)
	if value != "" {
		if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds >= 0 {
			return time.Duration(seconds) * time.Second
		}
	}
	return throttle.Backoff(attempt)
}

// sleepContext sleeps for delay or returns ctx.Err() if the context is
// cancelled first.
func sleepContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
