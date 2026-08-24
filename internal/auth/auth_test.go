package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/arkan/icloud-cli/internal/api"
	"github.com/arkan/icloud-cli/internal/config"
)

func TestRequestCodeTriggersCurrentApple2FAPushEndpoint(t *testing.T) {
	t.Parallel()

	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPut && r.URL.Path == "/verify/trusteddevice/securitycode":
			if r.Header.Get("scnt") != "scnt-value" || r.Header.Get("X-Apple-ID-Session-Id") != "session-value" {
				t.Errorf("2FA push request is missing session headers: %#v", r.Header)
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected request", http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()

	authenticator := NewAuthenticator(api.NewClient(&config.Session{
		Scnt: "scnt-value", SessionID: "session-value",
	}))
	authenticator.authEndpoint = server.URL
	if err := authenticator.RequestCode(); err != nil {
		t.Fatalf("RequestCode: %v", err)
	}

	want := []string{
		"GET /",
		"PUT /verify/trusteddevice/securitycode",
	}
	if len(requests) != len(want) {
		t.Fatalf("requests = %#v, want %#v", requests, want)
	}
	for i := range want {
		if requests[i] != want[i] {
			t.Errorf("request %d = %q, want %q", i, requests[i], want[i])
		}
	}
}
