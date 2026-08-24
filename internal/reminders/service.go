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

// Sharee is a participant who can receive assignments in a shared list.
type Sharee struct {
	ParticipantID  string
	UserRecordName string
	DisplayName    string
	Email          string
	Phone          string
	CurrentUser    bool
}

// ParsedReminder is a user-friendly reminder representation
type ParsedReminder struct {
	GUID             string
	RecordName       string
	ParentRecordName string
	ListGUID         string
	ListName         string
	Title            string
	Description      string
	DueDate          *time.Time
	Completed        bool
	CompletionDate   *time.Time
	Priority         int
	Flagged          bool
	CreatedDate      time.Time
	ModifiedDate     time.Time
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
			GUID:             guid,
			RecordName:       item.ID,
			ParentRecordName: item.ParentID,
			ListGUID:         item.ListID,
			Title:            item.Title,
			Description:      item.Notes,
			Completed:        item.Completed,
			CompletionDate:   item.CompletionDate,
			Priority:         item.Priority,
			Flagged:          item.Flagged,
			CreatedDate:      item.CreatedDate,
			ModifiedDate:     item.ModifiedDate,
			DueDate:          item.DueDate,
		}
		result = append(result, parsed)
	}

	return result, nil
}

// GetRawReminder returns the underlying CloudKit record without transformation.
func (s *Service) GetRawReminder(recordName string) (cloudkit.Record, error) {
	record, err := s.cloudkitSvc.GetReminderRecord(recordName)
	if err != nil {
		return cloudkit.Record{}, fmt.Errorf("get raw reminder: %w", err)
	}
	return record, nil
}

// GetReminderProperties resolves native properties stored on linked records.
func (s *Service) GetReminderProperties(recordName string) (cloudkit.ReminderProperties, error) {
	properties, err := s.cloudkitSvc.GetReminderProperties(recordName)
	if err != nil {
		return cloudkit.ReminderProperties{}, fmt.Errorf("get reminder properties: %w", err)
	}
	return properties, nil
}

// Add creates a new reminder via CloudKit
func (s *Service) Add(title, description, listGUID string, dueDate *cloudkit.DueDateChange, priority int, parentGUID string, earlyReminder *cloudkit.EarlyReminder, urgent bool) error {
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
	if earlyReminder != nil {
		if err := s.cloudkitSvc.ValidateEarlyReminderSupport(); err != nil {
			return fmt.Errorf("validate reminder early alert: %w", err)
		}
	}
	if urgent {
		if err := s.cloudkitSvc.ValidateUrgentReminderSupport(); err != nil {
			return fmt.Errorf("validate reminder Urgent alarm: %w", err)
		}
	}

	created, err := s.cloudkitSvc.AddReminderWithParent(title, description, listGUID, priority, nil, parentGUID)
	if err != nil {
		return fmt.Errorf("add reminder: %w", err)
	}
	rollback := func(step string, cause error) error {
		if rollbackErr := s.cloudkitSvc.DeleteReminder(created.ID); rollbackErr != nil {
			return fmt.Errorf("%s: %w; rollback of new reminder %s failed: %v", step, cause, created.ID, rollbackErr)
		}
		return fmt.Errorf("%s: %w", step, cause)
	}
	if dueDate != nil {
		if err := s.cloudkitSvc.UpdateDueDate(created.ID, dueDate); err != nil {
			return rollback("set reminder due date", err)
		}
	}
	if earlyReminder != nil {
		if err := s.cloudkitSvc.UpdateEarlyReminder(created.ID, earlyReminder); err != nil {
			return rollback("set reminder early alert", err)
		}
	}
	if urgent {
		if err := s.cloudkitSvc.UpdateUrgentReminder(created.ID, true); err != nil {
			return rollback("set reminder Urgent alarm", err)
		}
	}

	return nil
}

// UpdateDueDate replaces or clears the due date and its native date alarm.
func (s *Service) UpdateDueDate(reminderGUID string, due *cloudkit.DueDateChange) error {
	if err := s.cloudkitSvc.UpdateDueDate(reminderGUID, due); err != nil {
		return fmt.Errorf("update due date: %w", err)
	}
	return nil
}

// UpdateRecurrence replaces or clears the reminder's native recurrence rule.
func (s *Service) UpdateRecurrence(reminderGUID string, recurrence *cloudkit.RecurrenceRule) error {
	if err := s.cloudkitSvc.UpdateRecurrence(reminderGUID, recurrence); err != nil {
		return fmt.Errorf("update recurrence: %w", err)
	}
	return nil
}

// UpdateEarlyReminder replaces or clears the reminder's early alert.
func (s *Service) UpdateEarlyReminder(reminderGUID string, alert *cloudkit.EarlyReminder) error {
	if err := s.cloudkitSvc.UpdateEarlyReminder(reminderGUID, alert); err != nil {
		return fmt.Errorf("update early reminder: %w", err)
	}
	return nil
}

// UpdateUrgent enables or disables the reminder's Urgent alarm.
func (s *Service) UpdateUrgent(reminderGUID string, enabled bool) error {
	if err := s.cloudkitSvc.UpdateUrgentReminder(reminderGUID, enabled); err != nil {
		return fmt.Errorf("update Urgent alarm: %w", err)
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

// GetSharees returns accepted participants for a shared list.
func (s *Service) GetSharees(listGUID string) ([]Sharee, error) {
	participants, err := s.cloudkitSvc.GetSharees(listGUID)
	if err != nil {
		return nil, fmt.Errorf("get sharees: %w", err)
	}
	result := make([]Sharee, 0, len(participants))
	for _, participant := range participants {
		result = append(result, Sharee{
			ParticipantID:  participant.ParticipantID,
			UserRecordName: participant.UserRecordName,
			DisplayName:    participant.DisplayName,
			Email:          participant.Email,
			Phone:          participant.Phone,
			CurrentUser:    participant.CurrentUser,
		})
	}
	return result, nil
}

// UpdateAssignment assigns a shared reminder or clears its current assignment.
func (s *Service) UpdateAssignment(reminderGUID, assignee string, clear bool) error {
	if err := s.cloudkitSvc.UpdateAssignment(reminderGUID, assignee, clear); err != nil {
		return fmt.Errorf("update assignment: %w", err)
	}
	return nil
}

// UpdateLocationAlarm replaces or clears the reminder's location alarm.
func (s *Service) UpdateLocationAlarm(reminderGUID string, location *cloudkit.LocationAlarm) error {
	if err := s.cloudkitSvc.UpdateLocationAlarm(reminderGUID, location); err != nil {
		return fmt.Errorf("update location alarm: %w", err)
	}
	return nil
}

// UpdateURLAttachment replaces or clears the reminder's native URL attachment.
func (s *Service) UpdateURLAttachment(reminderGUID, rawURL string) error {
	if err := s.cloudkitSvc.UpdateURLAttachment(reminderGUID, rawURL); err != nil {
		return fmt.Errorf("update URL attachment: %w", err)
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
