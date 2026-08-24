// Package cloudkit provides access to iCloud CloudKit services
package cloudkit

import "fmt"

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

// ZoneChangesRequest is the CloudKit changes/zone payload.
type ZoneChangesRequest struct {
	Zones []ZoneChangesSpec `json:"zones"`
}

// ZoneChangesSpec selects one custom zone and an optional delta token.
type ZoneChangesSpec struct {
	ZoneID      ZoneID   `json:"zoneID"`
	DesiredKeys []string `json:"desiredKeys,omitempty"`
	SyncToken   string   `json:"syncToken,omitempty"`
}

// ZoneChangesResponse wraps each requested zone's changes.
type ZoneChangesResponse struct {
	Zones []ChangesResponse `json:"zones"`
}

// FieldValue represents a CloudKit field value
type FieldValue struct {
	Value       interface{} `json:"value"`
	Type        string      `json:"type,omitempty"`
	IsEncrypted bool        `json:"isEncrypted,omitempty"`
}

// RecordReference represents a reference to another record
type RecordReference struct {
	RecordName string  `json:"recordName"`
	Action     string  `json:"action,omitempty"`
	ZoneID     *ZoneID `json:"zoneID,omitempty"`
}

// Record represents a CloudKit record
type Record struct {
	RecordName      string                `json:"recordName"`
	RecordType      string                `json:"recordType"`
	RecordChangeTag string                `json:"recordChangeTag,omitempty"`
	Fields          map[string]FieldValue `json:"fields,omitempty"`
	PluginFields    map[string]FieldValue `json:"pluginFields,omitempty"`
	Created         *Timestamp            `json:"created,omitempty"`
	Modified        *Timestamp            `json:"modified,omitempty"`
	Deleted         bool                  `json:"deleted,omitempty"`
	Parent          *RecordReference      `json:"parent,omitempty"`
	Owner           *ShareParticipant     `json:"owner,omitempty"`
	Participants    []ShareParticipant    `json:"participants,omitempty"`
	CurrentUser     *ShareParticipant     `json:"currentUserParticipant,omitempty"`
	ServerErrorCode string                `json:"serverErrorCode,omitempty"`
	Reason          string                `json:"reason,omitempty"`
}

// ShareParticipant describes a user included in a CloudKit share.
type ShareParticipant struct {
	ParticipantID    string       `json:"participantId"`
	AcceptanceStatus string       `json:"acceptanceStatus"`
	Permission       string       `json:"permission"`
	Type             string       `json:"type"`
	UserIdentity     UserIdentity `json:"userIdentity"`
}

// UserIdentity contains the identity information CloudKit exposes for a share participant.
type UserIdentity struct {
	UserRecordName string         `json:"userRecordName"`
	LookupInfo     UserLookupInfo `json:"lookupInfo"`
	NameComponents NameComponents `json:"nameComponents"`
}

type UserLookupInfo struct {
	EmailAddress string `json:"emailAddress,omitempty"`
	PhoneNumber  string `json:"phoneNumber,omitempty"`
}

type NameComponents struct {
	GivenName  string `json:"givenName,omitempty"`
	FamilyName string `json:"familyName,omitempty"`
}

func recordError(record Record) error {
	if record.ServerErrorCode == "" {
		return nil
	}
	return fmt.Errorf("CloudKit error %s: %s", record.ServerErrorCode, record.Reason)
}

// Timestamp represents a CloudKit timestamp
type Timestamp struct {
	Timestamp int64  `json:"timestamp"`
	UserID    string `json:"userRecordName,omitempty"`
	DeviceID  string `json:"deviceID,omitempty"`
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
	ZoneID             ZoneID   `json:"zoneID"`
	Query              Query    `json:"query"`
	ResultsLimit       int      `json:"resultsLimit,omitempty"`
	DesiredKeys        []string `json:"desiredKeys,omitempty"`
	ContinuationMarker string   `json:"continuationMarker,omitempty"`
}

// RecordsResponse is the response from records/query
type RecordsResponse struct {
	Records            []Record `json:"records"`
	ContinuationMarker string   `json:"continuationMarker,omitempty"`
}

type AssetUploadRequest struct {
	ZoneID ZoneID             `json:"zoneID"`
	Tokens []AssetUploadToken `json:"tokens"`
}

type AssetUploadResponse struct {
	Tokens []AssetUploadToken `json:"tokens"`
}

type AssetUploadToken struct {
	RecordName string `json:"recordName"`
	RecordType string `json:"recordType,omitempty"`
	FieldName  string `json:"fieldName"`
	URL        string `json:"url,omitempty"`
}

type AssetValue struct {
	WrappingKey       string `json:"wrappingKey"`
	FileChecksum      string `json:"fileChecksum"`
	Receipt           string `json:"receipt"`
	ReferenceChecksum string `json:"referenceChecksum"`
	Size              int64  `json:"size"`
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
	ZoneID     ZoneID            `json:"zoneID"`
	Operations []RecordOperation `json:"operations"`
	Atomic     bool              `json:"atomic,omitempty"`
}

type OperationType string

const (
	OperationCreate OperationType = "create"
	OperationUpdate OperationType = "update"
	OperationDelete OperationType = "delete"
)

// RecordOperation represents a create/update/delete operation
type RecordOperation struct {
	OperationType OperationType `json:"operationType"`
	Record        Record        `json:"record"`
}

// ErrorResponse represents a CloudKit error
type ErrorResponse struct {
	UUID            string `json:"uuid"`
	ServerErrorCode string `json:"serverErrorCode"`
	Reason          string `json:"reason"`
}
