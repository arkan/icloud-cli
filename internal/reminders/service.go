// Package reminders provides access to iCloud Reminders
package reminders

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/florian/icloud-cli/internal/api"
)

// Service provides access to Reminders
type Service struct {
	client     *api.Client
	serviceURL string
	timezone   string
}

// Collection represents a reminders list
type Collection struct {
	GUID  string `json:"guid"`
	Title string `json:"title"`
	Ctag  string `json:"ctag"`
	Order int    `json:"order"`
}

// Reminder represents a single reminder
type Reminder struct {
	GUID                string    `json:"guid"`
	PGUID               string    `json:"pGuid"`
	Title               string    `json:"title"`
	Description         string    `json:"description,omitempty"`
	Priority            int       `json:"priority"`
	DueDate             []int     `json:"dueDate,omitempty"`
	CompletedDate       []int     `json:"completedDate,omitempty"`
	CreatedDateExtended int64     `json:"createdDateExtended,omitempty"`
	Etag                string    `json:"etag,omitempty"`
	Order               *int      `json:"order,omitempty"`
	Recurrence          *string   `json:"recurrence,omitempty"`
	Alarms              []any     `json:"alarms,omitempty"`
	StartDate           *[]int    `json:"startDate,omitempty"`
	StartDateTz         *string   `json:"startDateTz,omitempty"`
	StartDateIsAllDay   bool      `json:"startDateIsAllDay"`
	DueDateIsAllDay     bool      `json:"dueDateIsAllDay"`
	LastModifiedDate    *[]int    `json:"lastModifiedDate,omitempty"`
	CreatedDate         *[]int    `json:"createdDate,omitempty"`
	IsFamily            *bool     `json:"isFamily,omitempty"`
	Deleted             bool      `json:"deleted,omitempty"`
}

// StartupResponse is the response from /rd/startup
type StartupResponse struct {
	Collections []Collection `json:"Collections"`
	Reminders   []Reminder   `json:"Reminders"`
}

// TaskRequest is the request for creating/updating reminders
type TaskRequest struct {
	Reminders   Reminder    `json:"Reminders"`
	ClientState ClientState `json:"ClientState"`
}

// ClientState contains the current state of collections
type ClientState struct {
	Collections []CollectionRef `json:"Collections"`
}

// CollectionRef is a reference to a collection with ctag
type CollectionRef struct {
	GUID string `json:"guid"`
	Ctag string `json:"ctag"`
}

// ParsedReminder is a user-friendly reminder representation
type ParsedReminder struct {
	GUID        string
	ListGUID    string
	ListName    string
	Title       string
	Description string
	DueDate     *time.Time
	Completed   bool
	Priority    int
}

// NewService creates a new Reminders service
func NewService(client *api.Client) (*Service, error) {
	url, err := client.GetWebserviceURL("reminders")
	if err != nil {
		return nil, err
	}

	return &Service{
		client:     client,
		serviceURL: url,
		timezone:   "Europe/Paris",
	}, nil
}

// SetTimezone sets the timezone for date operations
func (s *Service) SetTimezone(tz string) {
	s.timezone = tz
}

// GetAll fetches all reminders and collections
func (s *Service) GetAll() (*StartupResponse, error) {
	url := s.serviceURL + "/rd/startup?clientVersion=4.0&lang=en-us&usertz=" + s.timezone
	
	resp, body, err := s.client.Request("GET", url, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("fetch reminders: %w", err)
	}

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("fetch reminders failed: %s - %s", resp.Status, string(body))
	}

	var result StartupResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("parse reminders: %w", err)
	}

	return &result, nil
}

// GetLists returns all reminder lists
func (s *Service) GetLists() ([]Collection, error) {
	data, err := s.GetAll()
	if err != nil {
		return nil, err
	}
	return data.Collections, nil
}

// GetReminders returns all reminders for a specific list
func (s *Service) GetReminders(listGUID string) ([]ParsedReminder, error) {
	data, err := s.GetAll()
	if err != nil {
		return nil, err
	}

	// Build list name map
	listNames := make(map[string]string)
	for _, c := range data.Collections {
		listNames[c.GUID] = c.Title
	}

	var result []ParsedReminder
	for _, r := range data.Reminders {
		// Filter by list if specified
		if listGUID != "" && r.PGUID != listGUID {
			continue
		}

		// Skip completed
		if r.CompletedDate != nil && len(r.CompletedDate) > 0 {
			continue
		}

		parsed := ParsedReminder{
			GUID:        r.GUID,
			ListGUID:    r.PGUID,
			ListName:    listNames[r.PGUID],
			Title:       r.Title,
			Description: r.Description,
			Completed:   r.CompletedDate != nil && len(r.CompletedDate) > 0,
			Priority:    r.Priority,
		}

		if r.DueDate != nil && len(r.DueDate) >= 6 {
			due := time.Date(r.DueDate[1], time.Month(r.DueDate[2]), r.DueDate[3],
				r.DueDate[4], r.DueDate[5], 0, 0, time.Local)
			parsed.DueDate = &due
		}

		result = append(result, parsed)
	}

	return result, nil
}

// Add creates a new reminder
func (s *Service) Add(title, description, listGUID string, dueDate *time.Time) error {
	// Get current state for ClientState
	data, err := s.GetAll()
	if err != nil {
		return err
	}

	// Build collection refs
	var collections []CollectionRef
	for _, c := range data.Collections {
		collections = append(collections, CollectionRef{
			GUID: c.GUID,
			Ctag: c.Ctag,
		})
	}

	// Default to "tasks" list
	if listGUID == "" {
		listGUID = "tasks"
	}

	// Build due date array
	var dueDateArr []int
	if dueDate != nil {
		dueDateArr = []int{
			dueDate.Year()*10000 + int(dueDate.Month())*100 + dueDate.Day(),
			dueDate.Year(),
			int(dueDate.Month()),
			dueDate.Day(),
			dueDate.Hour(),
			dueDate.Minute(),
		}
	}

	reminder := Reminder{
		GUID:                generateUUID(),
		PGUID:               listGUID,
		Title:               title,
		Description:         description,
		Priority:            0,
		DueDate:             dueDateArr,
		Alarms:              []any{},
		StartDateIsAllDay:   false,
		DueDateIsAllDay:     false,
		CreatedDateExtended: time.Now().UnixMilli(),
	}

	req := TaskRequest{
		Reminders:   reminder,
		ClientState: ClientState{Collections: collections},
	}

	url := s.serviceURL + "/rd/reminders/tasks?clientVersion=4.0&lang=en-us&usertz=" + s.timezone
	
	resp, body, err := s.client.Request("POST", url, req, nil)
	if err != nil {
		return fmt.Errorf("add reminder: %w", err)
	}

	if resp.StatusCode != 200 {
		return fmt.Errorf("add reminder failed: %s - %s", resp.Status, string(body))
	}

	return nil
}

// Complete marks a reminder as done
func (s *Service) Complete(reminderGUID string) error {
	data, err := s.GetAll()
	if err != nil {
		return err
	}

	// Find the reminder
	var reminder *Reminder
	for _, r := range data.Reminders {
		if r.GUID == reminderGUID {
			rCopy := r
			reminder = &rCopy
			break
		}
	}

	if reminder == nil {
		return fmt.Errorf("reminder not found: %s", reminderGUID)
	}

	// Build collection refs
	var collections []CollectionRef
	for _, c := range data.Collections {
		collections = append(collections, CollectionRef{
			GUID: c.GUID,
			Ctag: c.Ctag,
		})
	}

	// Set completed date
	now := time.Now()
	reminder.CompletedDate = []int{
		now.Year()*10000 + int(now.Month())*100 + now.Day(),
		now.Year(),
		int(now.Month()),
		now.Day(),
		now.Hour(),
		now.Minute(),
	}

	req := TaskRequest{
		Reminders:   *reminder,
		ClientState: ClientState{Collections: collections},
	}

	url := s.serviceURL + "/rd/reminders/tasks?clientVersion=4.0&lang=en-us&usertz=" + s.timezone
	
	resp, body, err := s.client.Request("POST", url, req, nil)
	if err != nil {
		return fmt.Errorf("complete reminder: %w", err)
	}

	if resp.StatusCode != 200 {
		return fmt.Errorf("complete reminder failed: %s - %s", resp.Status, string(body))
	}

	return nil
}

// Delete removes a reminder
func (s *Service) Delete(reminderGUID string) error {
	data, err := s.GetAll()
	if err != nil {
		return err
	}

	// Find the reminder
	var reminder *Reminder
	for _, r := range data.Reminders {
		if r.GUID == reminderGUID {
			rCopy := r
			reminder = &rCopy
			break
		}
	}

	if reminder == nil {
		return fmt.Errorf("reminder not found: %s", reminderGUID)
	}

	// Build collection refs
	var collections []CollectionRef
	for _, c := range data.Collections {
		collections = append(collections, CollectionRef{
			GUID: c.GUID,
			Ctag: c.Ctag,
		})
	}

	// Mark as deleted
	reminder.Deleted = true

	req := TaskRequest{
		Reminders:   *reminder,
		ClientState: ClientState{Collections: collections},
	}

	url := s.serviceURL + "/rd/reminders/tasks?clientVersion=4.0&lang=en-us&usertz=" + s.timezone
	
	resp, body, err := s.client.Request("POST", url, req, nil)
	if err != nil {
		return fmt.Errorf("delete reminder: %w", err)
	}

	if resp.StatusCode != 200 {
		return fmt.Errorf("delete reminder failed: %s - %s", resp.Status, string(body))
	}

	return nil
}

// generateUUID generates a UUID for new reminders
func generateUUID() string {
	// Simple UUID generation - in production use a proper UUID library
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		time.Now().UnixNano()&0xFFFFFFFF,
		time.Now().UnixNano()>>32&0xFFFF,
		0x4000|(time.Now().UnixNano()>>48&0x0FFF),
		0x8000|(time.Now().UnixNano()>>60&0x3FFF),
		time.Now().UnixNano()&0xFFFFFFFFFFFF)
}
