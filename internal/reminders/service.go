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

// GetReminders returns all reminders for a specific list via CloudKit
func (s *Service) GetReminders(listGUID string) ([]ParsedReminder, error) {
	items, err := s.cloudkitSvc.GetReminders(false) // exclude completed
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

// Add creates a new reminder (not yet implemented for CloudKit)
func (s *Service) Add(title, description, listGUID string, dueDate *time.Time) error {
	return fmt.Errorf("add reminder via CloudKit not yet implemented")
}

// Complete marks a reminder as done (not yet implemented for CloudKit)
func (s *Service) Complete(reminderGUID string) error {
	return fmt.Errorf("complete reminder via CloudKit not yet implemented")
}

// Delete removes a reminder (not yet implemented for CloudKit)
func (s *Service) Delete(reminderGUID string) error {
	return fmt.Errorf("delete reminder via CloudKit not yet implemented")
}
