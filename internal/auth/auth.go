// Package auth handles iCloud authentication
package auth

import (
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/arkan/icloud-cli/internal/api"
	"github.com/arkan/icloud-cli/internal/config"
)

// Authenticator handles iCloud authentication flow
type Authenticator struct {
	client *api.Client
}

// NewAuthenticator creates a new authenticator
func NewAuthenticator(client *api.Client) *Authenticator {
	return &Authenticator{client: client}
}

// SignInRequest is the request body for signing in
type SignInRequest struct {
	AccountName   string   `json:"accountName"`
	Password      string   `json:"password"`
	RememberMe    bool     `json:"rememberMe"`
	TrustTokens   []string `json:"trustTokens"`
}

// AccountLoginRequest is the request body for account login
type AccountLoginRequest struct {
	AccountCountryCode string `json:"accountCountryCode"`
	DsWebAuthToken     string `json:"dsWebAuthToken"`
	ExtendedLogin      bool   `json:"extended_login"`
	TrustToken         string `json:"trustToken"`
}

// AccountLoginResponse contains the response from account login
type AccountLoginResponse struct {
	DsInfo struct {
		HsaVersion int  `json:"hsaVersion"`
	} `json:"dsInfo"`
	HsaChallengeRequired bool `json:"hsaChallengeRequired"`
	HsaTrustedBrowser    bool `json:"hsaTrustedBrowser"`
	Webservices          map[string]struct {
		URL    string `json:"url"`
		Status string `json:"status"`
	} `json:"webservices"`
}

// SecurityCodeRequest is the request for 2FA verification
type SecurityCodeRequest struct {
	SecurityCode struct {
		Code string `json:"code"`
	} `json:"securityCode"`
}

// SignIn performs initial authentication
func (a *Authenticator) SignIn(appleID, password string) error {
	// Initialize session with new client ID
	session := a.client.Session()
	if session == nil {
		session = &config.Session{}
		a.client.SetSession(session)
	}
	if session.ClientID == "" {
		session.ClientID = "auth-" + uuid.New().String()
	}
	session.AppleID = appleID

	// Build request
	req := SignInRequest{
		AccountName: appleID,
		Password:    password,
		RememberMe:  true,
		TrustTokens: []string{},
	}
	if session.TrustToken != "" {
		req.TrustTokens = []string{session.TrustToken}
	}

	url := api.AuthEndpoint + "/signin?isRememberMeEnabled=true"
	headers := a.client.AuthHeaders()

	resp, body, err := a.client.Request("POST", url, req, headers)
	if err != nil {
		return fmt.Errorf("sign in request: %w", err)
	}

	if resp.StatusCode == 409 {
		// 2FA required - this is expected
		return a.authenticateWithToken()
	}

	if resp.StatusCode != 200 {
		return fmt.Errorf("sign in failed: %s - %s", resp.Status, string(body))
	}

	return a.authenticateWithToken()
}

// authenticateWithToken completes authentication using session token
func (a *Authenticator) authenticateWithToken() error {
	session := a.client.Session()
	if session == nil || session.SessionToken == "" {
		return fmt.Errorf("no session token available")
	}

	req := AccountLoginRequest{
		AccountCountryCode: session.AccountCountry,
		DsWebAuthToken:     session.SessionToken,
		ExtendedLogin:      true,
		TrustToken:         session.TrustToken,
	}

	url := api.SetupEndpoint + "/accountLogin"
	resp, body, err := a.client.Request("POST", url, req, nil)
	if err != nil {
		return fmt.Errorf("account login request: %w", err)
	}

	if resp.StatusCode != 200 {
		return fmt.Errorf("account login failed: %s - %s", resp.Status, string(body))
	}

	var loginResp AccountLoginResponse
	if err := json.Unmarshal(body, &loginResp); err != nil {
		return fmt.Errorf("parse login response: %w", err)
	}

	// Extract webservice URLs
	session.Webservices = make(map[string]string)
	for name, svc := range loginResp.Webservices {
		if svc.URL != "" {
			session.Webservices[name] = svc.URL
		}
	}

	return session.Save()
}

// Validate checks if the current session is still valid
func (a *Authenticator) Validate() (*AccountLoginResponse, error) {
	url := api.SetupEndpoint + "/validate"
	resp, body, err := a.client.Request("POST", url, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("validate request: %w", err)
	}

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("session invalid: %s", resp.Status)
	}

	var loginResp AccountLoginResponse
	if err := json.Unmarshal(body, &loginResp); err != nil {
		return nil, fmt.Errorf("parse validate response: %w", err)
	}

	// Update webservices
	session := a.client.Session()
	if session == nil {
		session = &config.Session{}
		a.client.SetSession(session)
	}
	session.Webservices = make(map[string]string)
	for name, svc := range loginResp.Webservices {
		if svc.URL != "" {
			session.Webservices[name] = svc.URL
		}
	}
	_ = session.Save()

	return &loginResp, nil
}

// Requires2FA checks if 2FA is required
func (a *Authenticator) Requires2FA(loginResp *AccountLoginResponse) bool {
	return loginResp.DsInfo.HsaVersion >= 1 && 
		(loginResp.HsaChallengeRequired || !loginResp.HsaTrustedBrowser)
}

// Verify2FA verifies a 2FA code
func (a *Authenticator) Verify2FA(code string) error {
	var req SecurityCodeRequest
	req.SecurityCode.Code = code

	url := api.AuthEndpoint + "/verify/trusteddevice/securitycode"
	headers := a.client.AuthHeaders()
	headers["Accept"] = "application/json"

	resp, body, err := a.client.Request("POST", url, req, headers)
	if err != nil {
		return fmt.Errorf("verify 2fa request: %w", err)
	}

	if resp.StatusCode != 204 && resp.StatusCode != 200 {
		return fmt.Errorf("2fa verification failed: %s - %s", resp.Status, string(body))
	}

	// Trust the session
	return a.TrustSession()
}

// TrustSession marks the session as trusted
func (a *Authenticator) TrustSession() error {
	url := api.AuthEndpoint + "/2sv/trust"
	headers := a.client.AuthHeaders()

	resp, body, err := a.client.Request("GET", url, nil, headers)
	if err != nil {
		return fmt.Errorf("trust session request: %w", err)
	}

	if resp.StatusCode != 204 && resp.StatusCode != 200 {
		return fmt.Errorf("trust session failed: %s - %s", resp.Status, string(body))
	}

	// Complete authentication
	return a.authenticateWithToken()
}
