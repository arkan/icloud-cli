package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/alecthomas/kong"
	"github.com/fatih/color"
	"golang.org/x/term"

	"github.com/arkan/icloud-cli/internal/api"
	"github.com/arkan/icloud-cli/internal/auth"
	"github.com/arkan/icloud-cli/internal/cloudkit"
	"github.com/arkan/icloud-cli/internal/config"
	"github.com/arkan/icloud-cli/internal/reminders"
)

var version = "0.1.0"

// CLI represents the command-line interface
type CLI struct {
	Version VersionCmd `cmd:"" help:"Show version"`
	Login   LoginCmd   `cmd:"" help:"Login to iCloud"`
	Logout  LogoutCmd  `cmd:"" help:"Logout from iCloud"`
	Status  StatusCmd  `cmd:"" help:"Show current session status"`

	// Reminders commands
	Reminders RemindersCmd `cmd:"" aliases:"r" help:"Manage reminders"`

	// CloudKit debug commands
	Cloudkit CloudkitCmd `cmd:"" aliases:"ck" help:"CloudKit debug commands"`
}

// VersionCmd shows the version
type VersionCmd struct{}

func (c *VersionCmd) Run() error {
	fmt.Printf("icloud-cli v%s\n", version)
	return nil
}

// LoginCmd handles authentication
type LoginCmd struct {
	AppleID  string `arg:"" optional:"" help:"Apple ID (email)"`
	Password string `short:"p" help:"Password (for debugging, prefer interactive input)"`
}

func (c *LoginCmd) Run() error {
	appleID := c.AppleID
	if appleID == "" {
		fmt.Print("Apple ID: ")
		fmt.Scanln(&appleID)
	}

	var password string
	if c.Password != "" {
		password = c.Password
	} else {
		fmt.Print("Password: ")
		passwordBytes, err := term.ReadPassword(int(syscall.Stdin))
		fmt.Println()
		if err != nil {
			return fmt.Errorf("read password: %w", err)
		}
		password = string(passwordBytes)
	}

	// Load existing session or create new
	session, _ := config.LoadSession()
	if session == nil {
		session = &config.Session{}
	}

	client := api.NewClient(session)
	authenticator := auth.NewAuthenticator(client)

	color.Yellow("→ Signing in to iCloud...")
	if err := authenticator.SignIn(appleID, password); err != nil {
		// Check if 2FA is needed
		if strings.Contains(err.Error(), "2FA required") {
			// Request code to be sent
			color.Yellow("→ Requesting verification code...")
			if reqErr := authenticator.RequestCode(); reqErr != nil {
				color.Yellow("  (code request: %v)", reqErr)
			}
			return handle2FA(authenticator)
		}
		return fmt.Errorf("sign in failed: %w", err)
	}

	// Check if 2FA is required
	loginResp, err := authenticator.Validate()
	if err != nil {
		return fmt.Errorf("validate session: %w", err)
	}

	if authenticator.Requires2FA(loginResp) {
		return handle2FA(authenticator)
	}

	color.Green("✓ Successfully logged in!")
	return nil
}

func handle2FA(authenticator *auth.Authenticator) error {
	color.Yellow("→ Two-factor authentication required")
	fmt.Println("A verification code has been sent to your trusted devices.")
	fmt.Print("Enter code: ")

	var code string
	fmt.Scanln(&code)
	code = strings.TrimSpace(code)

	if err := authenticator.Verify2FA(code); err != nil {
		return fmt.Errorf("2FA verification failed: %w", err)
	}

	color.Green("✓ Successfully logged in!")
	return nil
}

// LogoutCmd clears the session
type LogoutCmd struct{}

func (c *LogoutCmd) Run() error {
	if err := config.Clear(); err != nil {
		return fmt.Errorf("logout failed: %w", err)
	}
	color.Green("✓ Logged out")
	return nil
}

// StatusCmd shows session status
type StatusCmd struct{}

func (c *StatusCmd) Run() error {
	session, err := config.LoadSession()
	if err != nil {
		return fmt.Errorf("load session: %w", err)
	}

	if session == nil || session.SessionToken == "" {
		color.Red("✗ Not logged in")
		return nil
	}

	// Just check if we have valid session data without calling accountLogin
	// This avoids triggering Apple's "sign in" notification emails
	if len(session.Webservices) == 0 {
		color.Red("✗ Session incomplete - run 'icloud login' to refresh")
		return nil
	}

	color.Green("✓ Logged in as %s", session.AppleID)
	return nil
}

// RemindersCmd is the parent command for reminders
type RemindersCmd struct {
	Lists ListsCmd `cmd:"" aliases:"ll" help:"List all reminder lists"`
	Ls    LsCmd    `cmd:"" help:"List reminders"`
	Add   AddCmd   `cmd:"" help:"Add a new reminder"`
	Edit  EditCmd  `cmd:"" help:"Edit a reminder"`
	Done  DoneCmd  `cmd:"" help:"Mark a reminder as done (experimental)"`
	Rm    RmCmd    `cmd:"" help:"Delete a reminder"`
}

// EditCmd updates selected reminder fields.
type EditCmd struct {
	ID          string `arg:"" help:"Reminder ID (first 8 chars or full GUID)"`
	Title       string `help:"Replacement title (experimental)"`
	Description string `short:"d" help:"Replacement description (experimental)"`
	Due         string `help:"Replacement due date"`
	Priority    string `short:"p" help:"Priority: high, medium, low, none"`
}

func (c *EditCmd) Run() error {
	svc, err := getRemindersService()
	if err != nil {
		return err
	}
	guid, err := resolveReminderID(svc, c.ID)
	if err != nil {
		return err
	}

	var title, description *string
	if c.Title != "" {
		title = &c.Title
	}
	if c.Description != "" {
		description = &c.Description
	}
	var dueDate *time.Time
	if c.Due != "" {
		parsed, err := parseDueDate(c.Due)
		if err != nil {
			return fmt.Errorf("invalid due date: %w", err)
		}
		dueDate = &parsed
	}
	var priority *int
	if c.Priority != "" {
		value, err := parsePriority(c.Priority)
		if err != nil {
			return err
		}
		priority = &value
	}
	if title == nil && description == nil && dueDate == nil && priority == nil {
		return fmt.Errorf("no changes specified; use --title, --description, --due, or --priority")
	}
	changes := cloudkit.ReminderChanges{
		Title: title, Notes: description, DueDate: dueDate, Priority: priority,
	}
	if err := svc.Update(guid, changes); err != nil {
		return fmt.Errorf("edit reminder: %w", err)
	}
	color.Green("✓ Update submitted")
	return nil
}

func parsePriority(value string) (int, error) {
	switch strings.ToLower(value) {
	case "high", "h", "1":
		return 1, nil
	case "medium", "med", "m", "5":
		return 5, nil
	case "low", "l", "9":
		return 9, nil
	case "none", "0":
		return 0, nil
	default:
		return 0, fmt.Errorf("invalid priority %q; use high, medium, low, or none", value)
	}
}

func resolveReminderID(svc *reminders.Service, id string) (string, error) {
	if len(id) >= 36 {
		return id, nil
	}
	items, err := svc.GetReminders("", true)
	if err != nil {
		return "", err
	}
	for _, item := range items {
		if strings.HasPrefix(strings.ToLower(item.GUID), strings.ToLower(id)) {
			return item.GUID, nil
		}
	}
	return "", fmt.Errorf("reminder not found: %s", id)
}

// ListsCmd lists all reminder lists
type ListsCmd struct{}

func (c *ListsCmd) Run() error {
	svc, err := getRemindersService()
	if err != nil {
		return err
	}

	lists, err := svc.GetLists()
	if err != nil {
		return fmt.Errorf("get lists: %w", err)
	}

	if len(lists) == 0 {
		fmt.Println("No lists found")
		return nil
	}

	fmt.Println()
	for _, list := range lists {
		fmt.Printf("  📋 %s\n", color.CyanString(list.Title))
		fmt.Printf("     GUID: %s\n", color.HiBlackString(list.GUID))
	}
	fmt.Println()

	return nil
}

// LsCmd lists reminders
type LsCmd struct {
	List string `arg:"" optional:"" help:"List name or GUID (default: all)"`
	All  bool   `short:"a" help:"Include completed reminders"`
}

func (c *LsCmd) Run() error {
	svc, err := getRemindersService()
	if err != nil {
		return err
	}

	// Resolve list name to GUID if provided
	var listGUID string
	if c.List != "" {
		lists, err := svc.GetLists()
		if err != nil {
			return err
		}
		for _, l := range lists {
			if strings.EqualFold(l.Title, c.List) || l.GUID == c.List {
				listGUID = l.GUID
				break
			}
		}
		if listGUID == "" && c.List != "" {
			return fmt.Errorf("list not found: %s", c.List)
		}
	}

	reminders, err := svc.GetReminders(listGUID, c.All)
	if err != nil {
		return fmt.Errorf("get reminders: %w", err)
	}

	if len(reminders) == 0 {
		fmt.Println("No reminders")
		return nil
	}

	fmt.Println()
	for _, r := range reminders {
		bullet := "○"
		titleColor := color.New(color.FgWhite)
		if r.Completed {
			bullet = "✓"
			titleColor = color.New(color.FgHiBlack, color.CrossedOut)
		}

		// Priority indicator
		priority := ""
		switch r.Priority {
		case 1:
			priority = color.RedString(" !!!")
		case 5:
			priority = color.YellowString(" !!")
		case 9:
			priority = color.BlueString(" !")
		}

		fmt.Printf("  %s %s%s\n", bullet, titleColor.Sprint(r.Title), priority)

		// Due date
		if r.DueDate != nil {
			dueStr := formatDueDate(*r.DueDate)
			if r.DueDate.Before(time.Now()) && !r.Completed {
				fmt.Printf("    %s\n", color.RedString("⏰ %s (overdue)", dueStr))
			} else {
				fmt.Printf("    %s\n", color.HiBlackString("⏰ %s", dueStr))
			}
		}

		// Description
		if r.Description != "" {
			fmt.Printf("    %s\n", color.HiBlackString(r.Description))
		}

		// GUID for reference
		fmt.Printf("    %s\n", color.HiBlackString("ID: %s", r.GUID[:8]))
	}
	fmt.Println()

	return nil
}

func formatDueDate(t time.Time) string {
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	tomorrow := today.AddDate(0, 0, 1)
	dueDay := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())

	switch {
	case dueDay.Equal(today):
		return fmt.Sprintf("Today %s", t.Format("15:04"))
	case dueDay.Equal(tomorrow):
		return fmt.Sprintf("Tomorrow %s", t.Format("15:04"))
	case dueDay.Before(today.AddDate(0, 0, 7)):
		return t.Format("Mon 15:04")
	default:
		return t.Format("Jan 2, 15:04")
	}
}

// AddCmd adds a new reminder
type AddCmd struct {
	Title       string `arg:"" help:"Reminder title"`
	List        string `short:"l" help:"List name or GUID"`
	Description string `short:"d" help:"Description"`
	Due         string `help:"Due date (e.g., 'tomorrow 14:00', '2024-01-20')"`
	Priority    string `short:"p" help:"Priority: high, medium, low (default: none)"`
}

func (c *AddCmd) Run() error {
	svc, err := getRemindersService()
	if err != nil {
		return err
	}

	// Resolve list
	var listGUID string
	if c.List != "" {
		lists, err := svc.GetLists()
		if err != nil {
			return err
		}
		for _, l := range lists {
			if strings.EqualFold(l.Title, c.List) || l.GUID == c.List {
				listGUID = l.GUID
				break
			}
		}
	}

	// Parse due date
	var dueDate *time.Time
	if c.Due != "" {
		parsed, err := parseDueDate(c.Due)
		if err != nil {
			return fmt.Errorf("invalid due date: %w", err)
		}
		dueDate = &parsed
	}

	// Parse priority
	priority := 0
	if c.Priority != "" {
		priority, err = parsePriority(c.Priority)
		if err != nil {
			return err
		}
	}

	if err := svc.Add(c.Title, c.Description, listGUID, dueDate, priority); err != nil {
		return fmt.Errorf("add reminder: %w", err)
	}

	color.Green("✓ Added: %s", c.Title)
	return nil
}

func parseDueDate(s string) (time.Time, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())

	// Handle relative dates
	switch {
	case s == "today":
		return today.Add(18 * time.Hour), nil // Default to 6 PM
	case s == "tomorrow":
		return today.AddDate(0, 0, 1).Add(9 * time.Hour), nil // Default to 9 AM
	case strings.HasPrefix(s, "tomorrow "):
		timeStr := strings.TrimPrefix(s, "tomorrow ")
		t, err := time.Parse("15:04", timeStr)
		if err != nil {
			return time.Time{}, err
		}
		return today.AddDate(0, 0, 1).Add(time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute), nil
	default:
		// Try various formats
		formats := []string{
			"2006-01-02 15:04",
			"2006-01-02",
			"Jan 2 15:04",
			"Jan 2",
		}
		for _, format := range formats {
			if t, err := time.Parse(format, s); err == nil {
				if t.Year() == 0 {
					t = time.Date(now.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), 0, 0, now.Location())
				}
				return t, nil
			}
		}
		return time.Time{}, fmt.Errorf("unrecognized format: %s", s)
	}
}

// DoneCmd submits an experimental completion mutation.
type DoneCmd struct {
	ID string `arg:"" help:"Reminder ID (first 8 chars or full GUID)"`
}

func (c *DoneCmd) Run() error {
	svc, err := getRemindersService()
	if err != nil {
		return err
	}
	guid, err := resolveReminderID(svc, c.ID)
	if err != nil {
		return err
	}
	if err := svc.Complete(guid); err != nil {
		return fmt.Errorf("complete reminder: %w", err)
	}
	color.Green("✓ Completion submitted")
	return nil
}

// RmCmd deletes a reminder
type RmCmd struct {
	ID string `arg:"" help:"Reminder ID (first 8 chars or full GUID)"`
}

func (c *RmCmd) Run() error {
	svc, err := getRemindersService()
	if err != nil {
		return err
	}

	guid, err := resolveReminderID(svc, c.ID)
	if err != nil {
		return err
	}

	if err := svc.Delete(guid); err != nil {
		return fmt.Errorf("delete reminder: %w", err)
	}

	color.Green("✓ Deleted")
	return nil
}

// CloudkitCmd is the parent command for CloudKit debug operations
type CloudkitCmd struct {
	Zones     CKZonesCmd     `cmd:"" help:"List CloudKit zones"`
	Records   CKRecordsCmd   `cmd:"" help:"Query CloudKit records"`
	Dump      CKDumpCmd      `cmd:"" help:"Dump all records from a zone"`
	Lookup    CKLookupCmd    `cmd:"" help:"Lookup specific records by name"`
	Reminders CKRemindersCmd `cmd:"" aliases:"r" help:"List reminders via CloudKit"`
}

// CKZonesCmd lists CloudKit zones
type CKZonesCmd struct {
	Container string `short:"c" default:"com.apple.reminders" help:"Container ID"`
	Env       string `short:"e" default:"production" help:"Environment (production/development)"`
	Database  string `short:"d" default:"private" help:"Database (private/public/shared)"`
}

func (c *CKZonesCmd) Run() error {
	ckClient, err := getCloudKitClient()
	if err != nil {
		return err
	}

	fmt.Printf("Listing zones for %s/%s/%s...\n\n", c.Container, c.Env, c.Database)

	zones, err := ckClient.ListZones(c.Container, c.Env, c.Database)
	if err != nil {
		return fmt.Errorf("list zones: %w", err)
	}

	if len(zones.Zones) == 0 {
		fmt.Println("No zones found")
		return nil
	}

	for _, z := range zones.Zones {
		fmt.Printf("  📁 %s\n", color.CyanString(z.ZoneID.ZoneName))
		if z.ZoneID.OwnerRecordName != "" {
			fmt.Printf("     Owner: %s\n", z.ZoneID.OwnerRecordName)
		}
		if z.SyncToken != "" {
			fmt.Printf("     SyncToken: %s...\n", z.SyncToken[:min(20, len(z.SyncToken))])
		}
	}

	return nil
}

// CKRecordsCmd queries CloudKit records
type CKRecordsCmd struct {
	Container  string `short:"c" default:"com.apple.reminders" help:"Container ID"`
	Env        string `short:"e" default:"production" help:"Environment"`
	Database   string `short:"d" default:"private" help:"Database"`
	Zone       string `short:"z" default:"com.apple.coredata.cloudkit.zone" help:"Zone name"`
	RecordType string `arg:"" optional:"" help:"Record type to query"`
	Limit      int    `short:"l" default:"10" help:"Max records to return"`
	Raw        bool   `short:"r" help:"Show raw JSON output"`
}

func (c *CKRecordsCmd) Run() error {
	ckClient, err := getCloudKitClient()
	if err != nil {
		return err
	}

	zoneID := cloudkit.ZoneID{ZoneName: c.Zone}

	// If no record type specified, try to discover them
	if c.RecordType == "" {
		fmt.Printf("Discovering record types in zone %s...\n\n", c.Zone)
		types, err := ckClient.GetRecordTypes(c.Container, c.Env, c.Database, zoneID)
		if err != nil {
			return err
		}
		if len(types) == 0 {
			fmt.Println("No record types found (try specifying one with -t)")
		} else {
			fmt.Println("Found record types:")
			for _, t := range types {
				fmt.Printf("  - %s\n", color.GreenString(t))
			}
		}
		return nil
	}

	// Query specific record type
	fmt.Printf("Querying %s records in %s/%s/%s zone=%s...\n\n",
		c.RecordType, c.Container, c.Env, c.Database, c.Zone)

	req := cloudkit.QueryRequest{
		ZoneID: zoneID,
		Query: cloudkit.Query{
			RecordType: c.RecordType,
		},
		ResultsLimit: c.Limit,
	}

	resp, err := ckClient.QueryRecords(c.Container, c.Env, c.Database, req)
	if err != nil {
		return fmt.Errorf("query records: %w", err)
	}

	if len(resp.Records) == 0 {
		fmt.Println("No records found")
		return nil
	}

	fmt.Printf("Found %d records:\n\n", len(resp.Records))

	if c.Raw {
		raw, _ := json.MarshalIndent(resp.Records, "", "  ")
		fmt.Println(string(raw))
		return nil
	}

	for i, r := range resp.Records {
		fmt.Printf("%d. %s (%s)\n", i+1, color.CyanString(r.RecordName[:min(16, len(r.RecordName))]), r.RecordType)
		if len(r.Fields) > 0 {
			for k, v := range r.Fields {
				valStr := fmt.Sprintf("%v", v.Value)
				if len(valStr) > 50 {
					valStr = valStr[:50] + "..."
				}
				fmt.Printf("   %s: %s\n", color.HiBlackString(k), valStr)
			}
		}
		fmt.Println()
	}

	if resp.ContinuationMarker != "" {
		fmt.Println(color.YellowString("(more records available)"))
	}

	return nil
}

// CKLookupCmd looks up specific records by name
type CKLookupCmd struct {
	Container string   `short:"c" default:"com.apple.reminders" help:"Container ID"`
	Env       string   `short:"e" default:"production" help:"Environment"`
	Database  string   `short:"d" default:"private" help:"Database"`
	Zone      string   `short:"z" default:"Reminders" help:"Zone name"`
	Names     []string `arg:"" help:"Record names to lookup"`
}

func (c *CKLookupCmd) Run() error {
	ckClient, err := getCloudKitClient()
	if err != nil {
		return err
	}

	zoneID := cloudkit.ZoneID{ZoneName: c.Zone}

	fmt.Printf("Looking up %d records in %s/%s/%s zone=%s...\n\n",
		len(c.Names), c.Container, c.Env, c.Database, c.Zone)

	resp, err := ckClient.LookupRecords(c.Container, c.Env, c.Database, zoneID, c.Names)
	if err != nil {
		return fmt.Errorf("lookup records: %w", err)
	}

	if len(resp.Records) == 0 {
		fmt.Println("No records found")
		return nil
	}

	raw, _ := json.MarshalIndent(resp.Records, "", "  ")
	fmt.Println(string(raw))

	return nil
}

// CKDumpCmd dumps all records from a zone using changes API
type CKDumpCmd struct {
	Container string `short:"c" default:"com.apple.reminders" help:"Container ID"`
	Env       string `short:"e" default:"production" help:"Environment"`
	Database  string `short:"d" default:"private" help:"Database"`
	Zone      string `short:"z" default:"Reminders" help:"Zone name"`
	Raw       bool   `short:"r" help:"Show raw JSON output"`
}

func (c *CKDumpCmd) Run() error {
	ckClient, err := getCloudKitClient()
	if err != nil {
		return err
	}

	zoneID := cloudkit.ZoneID{ZoneName: c.Zone}

	fmt.Printf("Fetching all records from %s/%s/%s zone=%s...\n\n",
		c.Container, c.Env, c.Database, c.Zone)

	resp, err := ckClient.FetchChanges(c.Container, c.Env, c.Database, zoneID, "")
	if err != nil {
		return fmt.Errorf("fetch changes: %w", err)
	}

	if len(resp.Records) == 0 {
		fmt.Println("No records found")
		return nil
	}

	fmt.Printf("Found %d records:\n\n", len(resp.Records))

	if c.Raw {
		raw, _ := json.MarshalIndent(resp.Records, "", "  ")
		fmt.Println(string(raw))
		return nil
	}

	// Group by record type
	byType := make(map[string][]cloudkit.Record)
	for _, r := range resp.Records {
		byType[r.RecordType] = append(byType[r.RecordType], r)
	}

	for recType, records := range byType {
		fmt.Printf("=== %s (%d) ===\n", color.CyanString(recType), len(records))
		for i, r := range records {
			if i >= 5 {
				fmt.Printf("  ... and %d more\n", len(records)-5)
				break
			}
			name := r.RecordName
			if len(name) > 20 {
				name = name[:20] + "..."
			}
			fmt.Printf("  %s", name)

			// Show a few key fields
			if title, ok := r.Fields["CD_title"]; ok {
				fmt.Printf(" - %v", title.Value)
			}
			if title, ok := r.Fields["title"]; ok {
				fmt.Printf(" - %v", title.Value)
			}
			fmt.Println()
		}
		fmt.Println()
	}

	if resp.MoreComing {
		fmt.Println(color.YellowString("(more records available - sync token saved)"))
	}

	return nil
}

// CKRemindersCmd lists reminders via CloudKit
type CKRemindersCmd struct {
	All bool `short:"a" help:"Include completed reminders"`
}

func (c *CKRemindersCmd) Run() error {
	ckClient, err := getCloudKitClient()
	if err != nil {
		return err
	}

	svc := cloudkit.NewRemindersService(ckClient)

	fmt.Println("Fetching reminders from CloudKit...")

	reminders, err := svc.GetReminders(!c.All)
	if err != nil {
		return fmt.Errorf("get reminders: %w", err)
	}

	if len(reminders) == 0 {
		fmt.Println("No reminders found")
		return nil
	}

	fmt.Printf("\nFound %d reminders:\n\n", len(reminders))

	for _, r := range reminders {
		bullet := "○"
		if r.Completed {
			bullet = "✓"
		}

		title := r.Title
		if title == "" {
			title = "(no title)"
		}

		fmt.Printf("  %s %s\n", bullet, title)

		if r.DueDate != nil {
			fmt.Printf("    Due: %s\n", color.HiBlackString(r.DueDate.Format("2006-01-02 15:04")))
		}

		if r.Notes != "" {
			notes := r.Notes
			if len(notes) > 50 {
				notes = notes[:50] + "..."
			}
			fmt.Printf("    %s\n", color.HiBlackString(notes))
		}

		fmt.Printf("    %s\n", color.HiBlackString("ID: "+r.ID[:min(20, len(r.ID))]))
	}

	return nil
}

// getCloudKitClient creates an authenticated CloudKit client
func getCloudKitClient() (*cloudkit.Client, error) {
	session, err := config.LoadSession()
	if err != nil {
		return nil, fmt.Errorf("load session: %w", err)
	}

	if session == nil || session.SessionToken == "" {
		return nil, fmt.Errorf("not logged in - run 'icloud login' first")
	}

	if len(session.Webservices) == 0 {
		return nil, fmt.Errorf("session incomplete - run 'icloud login' to refresh")
	}

	client := api.NewClient(session)
	return cloudkit.NewClient(client)
}

// getRemindersService creates an authenticated reminders service
func getRemindersService() (*reminders.Service, error) {
	session, err := config.LoadSession()
	if err != nil {
		return nil, fmt.Errorf("load session: %w", err)
	}

	if session == nil || session.SessionToken == "" {
		return nil, fmt.Errorf("not logged in - run 'icloud login' first")
	}

	// Check if session has required data without calling accountLogin
	// This avoids triggering Apple's "sign in" notification emails
	if len(session.Webservices) == 0 {
		return nil, fmt.Errorf("session incomplete - run 'icloud login' to refresh")
	}

	client := api.NewClient(session)
	return reminders.NewService(client)
}

func main() {
	cli := &CLI{}
	ctx := kong.Parse(cli,
		kong.Name("icloud"),
		kong.Description("iCloud CLI - Access your iCloud data from the command line"),
		kong.UsageOnError(),
		kong.ConfigureHelp(kong.HelpOptions{
			Compact: true,
		}),
	)

	if err := ctx.Run(); err != nil {
		color.Red("Error: %v", err)
		os.Exit(1)
	}
}
