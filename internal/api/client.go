// Package api provides the base HTTP client for iCloud API
package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"time"

	"github.com/florian/icloud-cli/internal/config"
)

const (
	AuthEndpoint  = "https://idmsa.apple.com/appleauth/auth"
	HomeEndpoint  = "https://www.icloud.com"
	SetupEndpoint = "https://setup.icloud.com/setup/ws/1"

	WidgetKey = "d39ba9916b7251055b22c7f910e2ea796ee65e98b2ddecea8f5dde8d9d1a815d"
)

// Client is the iCloud API client
type Client struct {
	http    *http.Client
	session *config.Session
}

// NewClient creates a new API client
func NewClient(session *config.Session) *Client {
	jar, _ := cookiejar.New(nil)
	return &Client{
		http: &http.Client{
			Jar:     jar,
			Timeout: 30 * time.Second,
		},
		session: session,
	}
}

// Session returns the current session
func (c *Client) Session() *config.Session {
	return c.session
}

// SetSession updates the session
func (c *Client) SetSession(s *config.Session) {
	c.session = s
}

// Request makes an HTTP request and handles common iCloud response patterns
func (c *Client) Request(method, url string, body interface{}, headers map[string]string) (*http.Response, []byte, error) {
	var reqBody io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, nil, fmt.Errorf("marshal body: %w", err)
		}
		reqBody = bytes.NewReader(data)
	}

	req, err := http.NewRequest(method, url, reqBody)
	if err != nil {
		return nil, nil, fmt.Errorf("create request: %w", err)
	}

	// Default headers
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", HomeEndpoint)
	req.Header.Set("Referer", HomeEndpoint+"/")

	// Custom headers
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp, nil, fmt.Errorf("read response: %w", err)
	}

	// Capture session headers
	c.captureSessionHeaders(resp)

	return resp, respBody, nil
}

// captureSessionHeaders extracts session data from response headers
func (c *Client) captureSessionHeaders(resp *http.Response) {
	if c.session == nil {
		c.session = &config.Session{}
	}

	headerMap := map[string]*string{
		"X-Apple-ID-Account-Country":  &c.session.AccountCountry,
		"X-Apple-ID-Session-Id":       &c.session.SessionID,
		"X-Apple-Session-Token":       &c.session.SessionToken,
		"X-Apple-TwoSV-Trust-Token":   &c.session.TrustToken,
		"scnt":                        &c.session.Scnt,
	}

	for header, target := range headerMap {
		if val := resp.Header.Get(header); val != "" {
			*target = val
		}
	}
}

// AuthHeaders returns headers required for authentication requests
func (c *Client) AuthHeaders() map[string]string {
	headers := map[string]string{
		"X-Apple-OAuth-Client-Id":          WidgetKey,
		"X-Apple-OAuth-Client-Type":        "firstPartyAuth",
		"X-Apple-OAuth-Redirect-URI":       HomeEndpoint,
		"X-Apple-OAuth-Require-Grant-Code": "true",
		"X-Apple-OAuth-Response-Mode":      "web_message",
		"X-Apple-OAuth-Response-Type":      "code",
		"X-Apple-Widget-Key":               WidgetKey,
	}

	if c.session != nil {
		if c.session.ClientID != "" {
			headers["X-Apple-OAuth-State"] = c.session.ClientID
		}
		if c.session.Scnt != "" {
			headers["scnt"] = c.session.Scnt
		}
		if c.session.SessionID != "" {
			headers["X-Apple-ID-Session-Id"] = c.session.SessionID
		}
	}

	return headers
}

// GetWebserviceURL returns the URL for a given webservice
func (c *Client) GetWebserviceURL(service string) (string, error) {
	if c.session == nil || c.session.Webservices == nil {
		return "", fmt.Errorf("not authenticated")
	}

	url, ok := c.session.Webservices[service]
	if !ok {
		return "", fmt.Errorf("service %q not available", service)
	}

	return url, nil
}
