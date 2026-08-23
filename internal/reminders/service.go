// Package reminders provides access to iCloud Reminders via CloudKit
package reminders

import (
	"fmt"
	"strings"
	"time"

	"github.com/arkan/icloud-cli/internal/api"
	"github.com/arkan/icloud-cli/internal/cloudkit"
)

// Service provides access to Reminders via CloudKit
type Service struct {
	client      *api.Client
	cloudkitSvc *cloudkit.RemindersService
}

// Collection represents a reminders list
type Collection struct {
	GUID  string `json:"guid"`
	Title string `json:"title"`
	Order int    `json:"order"`
}

// ParsedReminder is a user-friendly reminder representation
type ParsedReminder struct {
	GUID        string
	RecordName  string
	ListGUID    string
	ListName    string
	Title       string
	Description string
	DueDate     *time.Time
	Completed   bool
	Priority    int
}

// NewService creates a new Reminders service using CloudKit
func NewService(client *api.Client) (*Service, error) {
	ckClient, err := cloudkit.NewClient(client)
	if err != nil {
		return nil, fmt.Errorf("cloudkit not available: %w", err)
	}

	return &Service{
		client:      client,
		cloudkitSvc: cloudkit.NewRemindersService(ckClient),
	}, nil
}

// GetLists returns all reminder lists via CloudKit
func (s *Service) GetLists() ([]Collection, error) {
	lists, err := s.cloudkitSvc.GetLists()
	if err != nil {
		return nil, fmt.Errorf("get lists: %w", err)
	}

	var result []Collection
	for i, l := range lists {
		result = append(result, Collection{
			GUID:  l.ID,
			Title: l.Title,
			Order: i,
		})
	}

	return result, nil
}

// GetReminders returns reminders for a specific list via CloudKit. Completed
// reminders are excluded unless includeCompleted is true.
func (s *Service) GetReminders(listGUID string, includeCompleted ...bool) ([]ParsedReminder, error) {
	includeDone := len(includeCompleted) > 0 && includeCompleted[0]
	items, err := s.cloudkitSvc.GetReminders(includeDone)
	if err != nil {
		return nil, fmt.Errorf("get reminders: %w", err)
	}

	var result []ParsedReminder
	for _, item := range items {
		// Filter by list if specified
		if listGUID != "" && item.ListID != listGUID {
			continue
		}

		// Extract UUID from record name (format: "Reminder/UUID")
		guid := item.ID
		if parts := strings.Split(item.ID, "/"); len(parts) == 2 {
			guid = parts[1]
		}

		parsed := ParsedReminder{
			GUID:        guid,
			RecordName:  item.ID,
			ListGUID:    item.ListID,
			Title:       item.Title,
			Description: item.Notes,
			Completed:   item.Completed,
			Priority:    item.Priority,
			DueDate:     item.DueDate,
		}
		result = append(result, parsed)
	}

	return result, nil
}

// Add creates a new reminder via CloudKit
func (s *Service) Add(title, description, listGUID string, dueDate *time.Time, priority int, parentGUID string) error {
	// If no list specified, use the first available list
	if listGUID == "" && parentGUID == "" {
		lists, err := s.cloudkitSvc.GetLists()
		if err != nil {
			return fmt.Errorf("get lists: %w", err)
		}
		if len(lists) == 0 {
			return fmt.Errorf("no lists available")
		}
		listGUID = lists[0].ID
	}

	_, err := s.cloudkitSvc.AddReminderWithParent(title, description, listGUID, priority, dueDate, parentGUID)
	if err != nil {
		return fmt.Errorf("add reminder: %w", err)
	}

	return nil
}

// Update applies selected changes to an existing reminder.
func (s *Service) Update(reminderGUID string, changes cloudkit.ReminderChanges) error {
	if err := s.cloudkitSvc.UpdateReminder(reminderGUID, changes); err != nil {
		return fmt.Errorf("update reminder: %w", err)
	}
	return nil
}

// UpdateTags adds and removes native Reminders tags.
func (s *Service) UpdateTags(reminderGUID string, add, remove []string) error {
	if err := s.cloudkitSvc.UpdateTags(reminderGUID, add, remove); err != nil {
		return fmt.Errorf("update tags: %w", err)
	}
	return nil
}

// Complete submits a completion mutation via CloudKit.
func (s *Service) Complete(reminderGUID string) error {
	if err := s.cloudkitSvc.CompleteReminder(reminderGUID); err != nil {
		return fmt.Errorf("complete reminder: %w", err)
	}
	return nil
}

// Delete removes a reminder via CloudKit.
func (s *Service) Delete(reminderGUID string) error {
	if err := s.cloudkitSvc.DeleteReminder(reminderGUID); err != nil {
		return fmt.Errorf("delete reminder: %w", err)
	}
	return nil
}
