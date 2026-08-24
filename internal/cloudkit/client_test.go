package cloudkit

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/arkan/icloud-cli/internal/api"
	"github.com/arkan/icloud-cli/internal/config"
)

func TestUploadAssetUsesCloudKitThreeStepContract(t *testing.T) {
	t.Parallel()

	content := []byte(`{"enabled":true}`)
	var tokenRequest AssetUploadRequest
	var uploaded []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/database/1/com.apple.reminders/production/private/assets/upload":
			if err := json.NewDecoder(r.Body).Decode(&tokenRequest); err != nil {
				t.Errorf("decode token request: %v", err)
			}
			_ = json.NewEncoder(w).Encode(AssetUploadResponse{Tokens: []AssetUploadToken{{
				RecordName: "Reminder/R1", FieldName: "UrgentPresentationAlarmsAsData", URL: serverURL(r) + "/asset-data",
			}}})
		case "/asset-data":
			var err error
			uploaded, err = io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read asset: %v", err)
			}
			_, _ = io.WriteString(w, `{"singleFile":{"wrappingKey":"key","fileChecksum":"file","receipt":"receipt","referenceChecksum":"reference","size":16}}`)
		default:
			http.Error(w, "unexpected path: "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := NewClient(api.NewClient(&config.Session{Webservices: map[string]string{"ckdatabasews": server.URL}}))
	if err != nil {
		t.Fatal(err)
	}
	asset, err := client.UploadAsset("com.apple.reminders", "production", "private", ZoneID{ZoneName: "Reminders"}, "Reminder/R1", "Reminder", "UrgentPresentationAlarmsAsData", content)
	if err != nil {
		t.Fatalf("UploadAsset: %v", err)
	}
	if len(tokenRequest.Tokens) != 1 || tokenRequest.Tokens[0].RecordName != "Reminder/R1" || tokenRequest.Tokens[0].FieldName != "UrgentPresentationAlarmsAsData" {
		t.Fatalf("token request = %#v", tokenRequest)
	}
	if string(uploaded) != string(content) {
		t.Fatalf("uploaded = %q, want %q", uploaded, content)
	}
	if asset.Receipt != "receipt" || asset.Size != int64(len(content)) {
		t.Fatalf("asset = %#v", asset)
	}
}

func serverURL(r *http.Request) string {
	return "http://" + r.Host
}
