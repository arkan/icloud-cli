package cloudkit

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/arkan/icloud-cli/internal/api"
)

// Client provides access to CloudKit services
type Client struct {
	apiClient *api.Client
	baseURL   string
}

// NewClient creates a new CloudKit client
func NewClient(apiClient *api.Client) (*Client, error) {
	url, err := apiClient.GetWebserviceURL("ckdatabasews")
	if err != nil {
		return nil, fmt.Errorf("cloudkit service not available: %w", err)
	}

	return &Client{
		apiClient: apiClient,
		baseURL:   url,
	}, nil
}

// buildPath constructs a CloudKit API path
func (c *Client) buildPath(container, env, database, operation string) string {
	return fmt.Sprintf("/database/1/%s/%s/%s/%s", container, env, database, operation)
}

// request makes a CloudKit API request
func (c *Client) request(method, path string, body interface{}) ([]byte, error) {
	url := c.baseURL + path

	// Add query params
	if !strings.Contains(url, "?") {
		url += "?"
	} else {
		url += "&"
	}
	url += c.apiClient.WebserviceParams()

	resp, respBody, err := c.apiClient.Request(method, url, body, c.apiClient.WebserviceHeaders())
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("cloudkit request failed: %s - %s", resp.Status, string(respBody))
	}

	// Debug: print response if ICLOUD_DEBUG_RESPONSE is set
	if os.Getenv("ICLOUD_DEBUG_RESPONSE") == "1" {
		fmt.Fprintf(os.Stderr, "RESPONSE: %s\n", string(respBody))
	}

	return respBody, nil
}

// ListZones lists all zones in a database
func (c *Client) ListZones(container, env, database string) (*ZonesResponse, error) {
	path := c.buildPath(container, env, database, "zones/list")

	body, err := c.request("POST", path, map[string]interface{}{})
	if err != nil {
		return nil, fmt.Errorf("list zones: %w", err)
	}

	var result ZonesResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("parse zones response: %w", err)
	}

	return &result, nil
}

// QueryRecords queries records in a zone
func (c *Client) QueryRecords(container, env, database string, req QueryRequest) (*RecordsResponse, error) {
	path := c.buildPath(container, env, database, "records/query")

	body, err := c.request("POST", path, req)
	if err != nil {
		return nil, fmt.Errorf("query records: %w", err)
	}

	var result RecordsResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("parse records response: %w", err)
	}

	return &result, nil
}

// GetRecordTypes attempts to discover record types by querying the zone
// This is a helper for discovery - CloudKit doesn't have a direct "list types" API
func (c *Client) GetRecordTypes(container, env, database string, zoneID ZoneID) ([]string, error) {
	// Query with empty record type to see what's available
	// This might not work - CloudKit requires a recordType
	// We'll try common patterns
	commonTypes := []string{
		"CD_REMCDReminder",
		"CD_REMCDList",
		"CD_REMCDAccount",
		"Reminder",
		"ReminderList",
		"Tasks",
		"List",
	}

	var foundTypes []string
	for _, rt := range commonTypes {
		req := QueryRequest{
			ZoneID: zoneID,
			Query: Query{
				RecordType: rt,
			},
			ResultsLimit: 1,
		}

		_, err := c.QueryRecords(container, env, database, req)
		if err == nil {
			foundTypes = append(foundTypes, rt)
		}
	}

	return foundTypes, nil
}

// ChangesResponse is the response from zone changes
type ChangesResponse struct {
	Records   []Record `json:"records"`
	SyncToken string   `json:"syncToken,omitempty"`
	MoreComing bool    `json:"moreComing,omitempty"`
}

// FetchChanges fetches all records in a zone (initial sync)
func (c *Client) FetchChanges(container, env, database string, zoneID ZoneID, syncToken string) (*ChangesResponse, error) {
	path := c.buildPath(container, env, database, "records/changes")

	reqBody := map[string]interface{}{
		"zoneID":       zoneID,
		"resultsLimit": 200,
	}
	if syncToken != "" {
		reqBody["syncToken"] = syncToken
	}

	body, err := c.request("POST", path, reqBody)
	if err != nil {
		return nil, fmt.Errorf("fetch changes: %w", err)
	}

	// Parse the response
	var resp ChangesResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parse changes response: %w", err)
	}

	return &resp, nil
}
