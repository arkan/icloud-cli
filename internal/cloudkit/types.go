// Package cloudkit provides access to iCloud CloudKit services
package cloudkit

// ZoneID identifies a CloudKit zone
type ZoneID struct {
	ZoneName        string `json:"zoneName"`
	OwnerRecordName string `json:"ownerRecordName,omitempty"`
}

// Zone represents a CloudKit zone
type Zone struct {
	ZoneID    ZoneID `json:"zoneID"`
	SyncToken string `json:"syncToken,omitempty"`
	Atomic    bool   `json:"atomic,omitempty"`
}

// ZonesResponse is the response from zones/list
type ZonesResponse struct {
	Zones []Zone `json:"zones"`
}

// FieldValue represents a CloudKit field value
type FieldValue struct {
	Value interface{} `json:"value"`
	Type  string      `json:"type,omitempty"`
}

// RecordReference represents a reference to another record
type RecordReference struct {
	RecordName string `json:"recordName"`
	Action     string `json:"action,omitempty"`
	ZoneID     *ZoneID `json:"zoneID,omitempty"`
}

// Record represents a CloudKit record
type Record struct {
	RecordName       string                `json:"recordName"`
	RecordType       string                `json:"recordType"`
	RecordChangeTag  string                `json:"recordChangeTag,omitempty"`
	Fields           map[string]FieldValue `json:"fields,omitempty"`
	PluginFields     map[string]FieldValue `json:"pluginFields,omitempty"`
	Created          *Timestamp            `json:"created,omitempty"`
	Modified         *Timestamp            `json:"modified,omitempty"`
	Deleted          bool                  `json:"deleted,omitempty"`
	Parent           *RecordReference      `json:"parent,omitempty"`
}

// Timestamp represents a CloudKit timestamp
type Timestamp struct {
	Timestamp   int64  `json:"timestamp"`
	UserID      string `json:"userRecordName,omitempty"`
	DeviceID    string `json:"deviceID,omitempty"`
}

// Filter represents a query filter
type Filter struct {
	FieldName       string     `json:"fieldName,omitempty"`
	SystemFieldName string     `json:"systemFieldName,omitempty"`
	Comparator      string     `json:"comparator"`
	FieldValue      FieldValue `json:"fieldValue"`
}

// Sort represents a query sort order
type Sort struct {
	FieldName       string `json:"fieldName,omitempty"`
	SystemFieldName string `json:"systemFieldName,omitempty"`
	Ascending       bool   `json:"ascending"`
}

// Query represents a CloudKit query
type Query struct {
	RecordType string   `json:"recordType"`
	FilterBy   []Filter `json:"filterBy,omitempty"`
	SortBy     []Sort   `json:"sortBy,omitempty"`
}

// QueryRequest is the request body for records/query
type QueryRequest struct {
	ZoneID             ZoneID `json:"zoneID"`
	Query              Query  `json:"query"`
	ResultsLimit       int    `json:"resultsLimit,omitempty"`
	DesiredKeys        []string `json:"desiredKeys,omitempty"`
	ContinuationMarker string `json:"continuationMarker,omitempty"`
}

// RecordsResponse is the response from records/query
type RecordsResponse struct {
	Records            []Record `json:"records"`
	ContinuationMarker string   `json:"continuationMarker,omitempty"`
}

// LookupRequest is the request body for records/lookup
type LookupRequest struct {
	Records []RecordRef `json:"records"`
	ZoneID  ZoneID      `json:"zoneID"`
}

// RecordRef references a record by name
type RecordRef struct {
	RecordName string `json:"recordName"`
}

// ModifyRequest is the request body for records/modify
type ModifyRequest struct {
	ZoneID     ZoneID           `json:"zoneID"`
	Operations []RecordOperation `json:"operations"`
}

// RecordOperation represents a create/update/delete operation
type RecordOperation struct {
	OperationType string  `json:"operationType"` // create, update, forceUpdate, replace, forceReplace, delete, forceDelete
	Record        Record  `json:"record"`
}

// ErrorResponse represents a CloudKit error
type ErrorResponse struct {
	UUID           string `json:"uuid"`
	ServerErrorCode string `json:"serverErrorCode"`
	Reason         string `json:"reason"`
}
