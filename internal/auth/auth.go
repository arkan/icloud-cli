// Package auth handles iCloud authentication
package auth

import (
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/arkan/icloud-cli/internal/api"
	"github.com/arkan/icloud-cli/internal/config"
	"github.com/arkan/icloud-cli/internal/srp"
)

// Authenticator handles iCloud authentication flow
type Authenticator struct {
	client       *api.Client
	authEndpoint string
}

// NewAuthenticator creates a new authenticator
func NewAuthenticator(client *api.Client) *Authenticator {
	return &Authenticator{client: client, authEndpoint: api.AuthEndpoint}
}

// SRPInitRequest is the request body for SRP init
type SRPInitRequest struct {
	AccountName string   `json:"accountName"`
	A           string   `json:"a"`
	Protocols   []string `json:"protocols"`
}

// SRPInitResponse is the response from SRP init
type SRPInitResponse struct {
	Salt      string `json:"salt"`
	B         string `json:"b"`
	C         string `json:"c"`
	Iteration int    `json:"iteration"`
	Protocol  string `json:"protocol"`
}

// SRPCompleteRequest is the request body for SRP complete
type SRPCompleteRequest struct {
	AccountName string   `json:"accountName"`
	C           string   `json:"c"`
	M1          string   `json:"m1"`
	M2          string   `json:"m2"`
	RememberMe  bool     `json:"rememberMe"`
	TrustTokens []string `json:"trustTokens"`
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
		HsaVersion int    `json:"hsaVersion"`
		Dsid       string `json:"dsid"`
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

// SignIn performs initial authentication using SRP
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

	// Create SRP password handler
	srpPassword := srp.NewApplePassword(password)

	// Create SRP client
	srpClient, err := srp.NewClient(appleID, srpPassword)
	if err != nil {
		return fmt.Errorf("create SRP client: %w", err)
	}

	// Step 1: SRP Init
	initReq := SRPInitRequest{
		AccountName: appleID,
		A:           srpClient.GetPublicKey(),
		Protocols:   []string{"s2k", "s2k_fo"},
	}

	headers := a.client.AuthHeaders()
	url := a.authEndpoint + "/signin/init"

	resp, body, err := a.client.Request("POST", url, initReq, headers)
	if err != nil {
		return fmt.Errorf("SRP init request: %w", err)
	}

	if resp.StatusCode != 200 {
		return fmt.Errorf("SRP init failed: %s - %s", resp.Status, string(body))
	}

	var initResp SRPInitResponse
	if err := json.Unmarshal(body, &initResp); err != nil {
		return fmt.Errorf("parse SRP init response: %w", err)
	}

	// Step 2: Configure password with server parameters
	salt, err := decodeBase64(initResp.Salt)
	if err != nil {
		return fmt.Errorf("decode salt: %w", err)
	}
	srpPassword.SetEncryptInfo(initResp.Protocol, salt, initResp.Iteration)

	// Step 3: Process challenge and get M1, M2
	m1, m2, err := srpClient.ProcessChallenge(initResp.Salt, initResp.B)
	if err != nil {
		return fmt.Errorf("process SRP challenge: %w", err)
	}

	// Step 4: SRP Complete
	completeReq := SRPCompleteRequest{
		AccountName: appleID,
		C:           initResp.C,
		M1:          m1,
		M2:          m2,
		RememberMe:  true,
		TrustTokens: []string{},
	}
	if session.TrustToken != "" {
		completeReq.TrustTokens = []string{session.TrustToken}
	}

	url = a.authEndpoint + "/signin/complete?isRememberMeEnabled=true"
	resp, body, err = a.client.Request("POST", url, completeReq, headers)
	if err != nil {
		return fmt.Errorf("SRP complete request: %w", err)
	}

	if resp.StatusCode == 409 {
		// 2FA required - fetch auth state to trigger code sending
		if err := a.fetchAuthState(); err != nil {
			return fmt.Errorf("fetch auth state: %w", err)
		}
		// Return special error to indicate 2FA is needed
		return fmt.Errorf("2FA required")
	}

	if resp.StatusCode == 412 {
		// Non-2FA account, call repair endpoint
		url = a.authEndpoint + "/repair/complete"
		resp, body, err = a.client.Request("POST", url, map[string]interface{}{}, headers)
		if err != nil {
			return fmt.Errorf("repair complete request: %w", err)
		}
		if resp.StatusCode != 200 && resp.StatusCode != 204 {
			return fmt.Errorf("repair complete failed: %s - %s", resp.Status, string(body))
		}
		return a.authenticateWithToken()
	}

	if resp.StatusCode != 200 {
		return fmt.Errorf("SRP complete failed: %s - %s", resp.Status, string(body))
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

	// Extract dsid
	if loginResp.DsInfo.Dsid != "" {
		session.Dsid = loginResp.DsInfo.Dsid
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
	session := a.client.Session()
	if session == nil || session.SessionToken == "" {
		return nil, fmt.Errorf("no session token")
	}

	// Use accountLogin with stored token to validate and refresh session
	req := AccountLoginRequest{
		AccountCountryCode: session.AccountCountry,
		DsWebAuthToken:     session.SessionToken,
		ExtendedLogin:      true,
		TrustToken:         session.TrustToken,
	}

	url := api.SetupEndpoint + "/accountLogin"
	resp, body, err := a.client.Request("POST", url, req, nil)
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

	// Update dsid
	if loginResp.DsInfo.Dsid != "" {
		session.Dsid = loginResp.DsInfo.Dsid
	}

	// Update webservices
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

	url := a.authEndpoint + "/verify/trusteddevice/securitycode"
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
	url := a.authEndpoint + "/2sv/trust"
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

// fetchAuthState retrieves the current auth state (triggers 2FA code sending)
func (a *Authenticator) fetchAuthState() error {
	url := a.authEndpoint
	headers := a.client.AuthHeaders()

	resp, _, err := a.client.Request("GET", url, nil, headers)
	if err != nil {
		return fmt.Errorf("fetch auth state: %w", err)
	}

	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		return fmt.Errorf("fetch auth state failed: %s", resp.Status)
	}

	return nil
}

// RequestCode explicitly requests a 2FA code to be sent to trusted devices
func (a *Authenticator) RequestCode() error {
	// First try to get auth state which should trigger code push
	if err := a.fetchAuthState(); err != nil {
		return err
	}

	// Request code resend if needed
	url := a.authEndpoint + "/verify/trusteddevice/securitycode"
	headers := a.client.AuthHeaders()

	resp, _, err := a.client.Request("PUT", url, nil, headers)
	if err != nil {
		return fmt.Errorf("request code: %w", err)
	}

	// Apple currently returns 204 for a successful push trigger. Older
	// deployments have also returned 200 or 202.
	if resp.StatusCode != 200 && resp.StatusCode != 202 && resp.StatusCode != 204 {
		return fmt.Errorf("request code failed: %s", resp.Status)
	}

	return nil
}

// decodeBase64 decodes a base64 string
func decodeBase64(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(s)
}
