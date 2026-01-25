// Package api provides the base HTTP client for iCloud API
package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"strings"
	"time"

	"github.com/arkan/icloud-cli/internal/config"
)

var debug = os.Getenv("ICLOUD_DEBUG") == "1"

const (
	AuthEndpoint     = "https://idmsa.apple.com/appleauth/auth"
	AuthRootEndpoint = "https://idmsa.apple.com"
	HomeEndpoint     = "https://www.icloud.com"
	SetupEndpoint    = "https://setup.icloud.com/setup/ws/1"

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

	// Use custom transport to disable HTTP/2 (can cause 421 errors)
	transport := &http.Transport{
		ForceAttemptHTTP2: false,
	}

	return &Client{
		http: &http.Client{
			Jar:       jar,
			Timeout:   30 * time.Second,
			Transport: transport,
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

	if debug {
		fmt.Fprintf(os.Stderr, "DEBUG: %s %s\n", method, url)
		for k, v := range req.Header {
			fmt.Fprintf(os.Stderr, "  %s: %s\n", k, v)
		}
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

	// Capture cookies for webservice authentication
	if c.session.Cookies == nil {
		c.session.Cookies = make(map[string]string)
	}
	for _, cookie := range resp.Cookies() {
		// Store important Apple auth cookies
		if strings.HasPrefix(cookie.Name, "X-APPLE-") ||
			cookie.Name == "aasp" ||
			strings.HasPrefix(cookie.Name, "DES") {
			c.session.Cookies[cookie.Name] = cookie.Value
		}
	}
}

// AuthHeaders returns headers required for authentication requests
func (c *Client) AuthHeaders() map[string]string {
	headers := map[string]string{
		"Accept":                            "application/json, text/javascript",
		"Content-Type":                      "application/json",
		"User-Agent":                        "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
		"X-Apple-OAuth-Client-Id":           WidgetKey,
		"X-Apple-OAuth-Client-Type":         "firstPartyAuth",
		"X-Apple-OAuth-Redirect-URI":        HomeEndpoint,
		"X-Apple-OAuth-Require-Grant-Code":  "true",
		"X-Apple-OAuth-Response-Mode":       "web_message",
		"X-Apple-OAuth-Response-Type":       "code",
		"X-Apple-Widget-Key":                WidgetKey,
		// Auth requests must use idmsa.apple.com as origin
		"Origin":  AuthRootEndpoint,
		"Referer": AuthRootEndpoint + "/",
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

	// Remove explicit :443 port as it can cause 421 Misdirected Request
	url = strings.TrimSuffix(url, ":443")

	return url, nil
}

// WebserviceParams returns common query parameters for webservice requests
func (c *Client) WebserviceParams() string {
	params := "clientBuildNumber=2552Build21&clientMasteringNumber=2552Build21"
	if c.session != nil {
		if c.session.ClientID != "" {
			params += "&clientId=" + c.session.ClientID
		}
		if c.session.Dsid != "" {
			params += "&dsid=" + c.session.Dsid
		}
	}
	return params
}

// WebserviceHeaders returns headers needed for webservice requests
func (c *Client) WebserviceHeaders() map[string]string {
	headers := map[string]string{
		"Accept":       "application/json",
		"Content-Type": "application/json",
		"Origin":       HomeEndpoint,
		"Referer":      HomeEndpoint + "/",
	}

	// Add cookies as Cookie header
	if c.session != nil && c.session.Cookies != nil {
		var cookieParts []string
		for name, value := range c.session.Cookies {
			cookieParts = append(cookieParts, name+"="+value)
		}
		if len(cookieParts) > 0 {
			headers["Cookie"] = strings.Join(cookieParts, "; ")
		}
	}

	return headers
}
