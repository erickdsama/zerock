package client

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/erickdsama/zerock/internal/version"
)

// API calls a zerock server's management API using a profile's token.
type API struct {
	profile Profile
	http    *http.Client
}

// NewAPI builds a client for the given profile.
func NewAPI(p Profile) *API {
	return &API{
		profile: p,
		http: &http.Client{
			Timeout: 20 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					MinVersion:         tls.VersionTLS12,
					InsecureSkipVerify: p.Insecure,
				},
			},
		},
	}
}

// APIError is a non-2xx response from the server.
type APIError struct {
	Status  int    `json:"-"`
	Kind    string `json:"error"`
	Message string `json:"message"`
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	if e.Kind != "" {
		return e.Kind
	}
	return fmt.Sprintf("server returned HTTP %d", e.Status)
}

// Do performs a request against path and decodes a JSON response into out.
// Both body and out may be nil.
func (a *API) Do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}

	url := a.profile.APIURL(path)
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.profile.Token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", version.UserAgent())
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := a.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, url, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		apiErr := &APIError{Status: resp.StatusCode}
		if err := json.Unmarshal(raw, apiErr); err != nil || apiErr.Message == "" {
			// A non-JSON error usually means something other than zerock
			// answered, so show a trimmed body rather than a bare status.
			snippet := strings.TrimSpace(string(raw))
			if len(snippet) > 200 {
				snippet = snippet[:200] + "..."
			}
			apiErr.Message = fmt.Sprintf("HTTP %d from %s: %s", resp.StatusCode, url, snippet)
		}
		return apiErr
	}

	if out == nil || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode response from %s: %w", url, err)
	}
	return nil
}
