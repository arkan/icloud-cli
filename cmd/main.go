package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
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
	Lists   ListsCmd   `cmd:"" aliases:"ll" help:"List all reminder lists"`
	Ls      LsCmd      `cmd:"" help:"List reminders"`
	Sharees ShareesCmd `cmd:"" help:"List accepted participants of a shared list"`
	Add     AddCmd     `cmd:"" help:"Add a new reminder"`
	Edit    EditCmd    `cmd:"" help:"Edit a reminder"`
	Done    DoneCmd    `cmd:"" help:"Mark a reminder as done"`
	Rm      RmCmd      `cmd:"" help:"Delete a reminder"`
}

// EditCmd updates selected reminder fields.
type EditCmd struct {
	ID             string   `arg:"" help:"Reminder ID (first 8 chars or full GUID)"`
	Title          string   `help:"Replacement title"`
	Description    string   `short:"d" help:"Replacement description"`
	Due            string   `help:"Replacement due date"`
	ClearDue       bool     `help:"Remove the due date and its date alarm"`
	TimeZone       string   `name:"timezone" help:"IANA timezone for due dates (persisted)"`
	Priority       string   `short:"p" help:"Priority: high, medium, low, none"`
	Flagged        bool     `help:"Set the flag"`
	NoFlagged      bool     `name:"no-flagged" help:"Clear the flag"`
	Tags           []string `name:"tag" help:"Add a native tag (repeatable)"`
	RemoveTags     []string `name:"remove-tag" help:"Remove a native tag (repeatable)"`
	Assign         string   `help:"Assign to a shared-list participant by name, email, or ID"`
	Unassign       bool     `help:"Clear the current assignment"`
	LocationTitle  string   `help:"Location alarm title"`
	Address        string   `help:"Optional location alarm address"`
	Latitude       *float64 `help:"Location alarm latitude"`
	Longitude      *float64 `help:"Location alarm longitude"`
	Radius         float64  `default:"100" help:"Location alarm radius in meters"`
	Proximity      string   `default:"arriving" help:"Location alarm proximity: arriving or leaving"`
	ClearLocation  bool     `help:"Remove the location alarm"`
	URL            string   `help:"Set or replace the native URL attachment"`
	ClearURL       bool     `help:"Remove the native URL attachment"`
	Repeat         string   `help:"Repeat: daily, weekly, monthly, or yearly"`
	RepeatInterval int      `default:"1" help:"Recurrence interval"`
	RepeatUntil    string   `help:"Last recurrence date (YYYY-MM-DD)"`
	ClearRepeat    bool     `help:"Remove recurrence"`
	EarlyReminder  string   `name:"early-reminder" help:"Alert before due date (e.g. 15m, 1h, 2d, 1w, 1mo, or clear)"`
	Urgent         bool     `help:"Enable the Urgent alarm"`
	NoUrgent       bool     `name:"no-urgent" help:"Disable the Urgent alarm"`
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
	var dueDate *cloudkit.DueDateChange
	if c.Due != "" {
		location, zone, err := configuredTimeZone(c.TimeZone)
		if err != nil {
			return err
		}
		parsed, allDay, err := parseDueDate(c.Due, location)
		if err != nil {
			return fmt.Errorf("invalid due date: %w", err)
		}
		dueDate = &cloudkit.DueDateChange{Date: parsed, AllDay: allDay, TimeZone: zone}
	}
	if dueDate != nil && c.ClearDue {
		return fmt.Errorf("--due and --clear-due are mutually exclusive")
	}
	var priority *int
	if c.Priority != "" {
		value, err := parsePriority(c.Priority)
		if err != nil {
			return err
		}
		priority = &value
	}
	if c.Flagged && c.NoFlagged {
		return fmt.Errorf("--flagged and --no-flagged are mutually exclusive")
	}
	if c.Assign != "" && c.Unassign {
		return fmt.Errorf("--assign and --unassign are mutually exclusive")
	}
	locationRequested := c.LocationTitle != "" || c.Address != "" || c.Latitude != nil || c.Longitude != nil
	if locationRequested && c.ClearLocation {
		return fmt.Errorf("location options and --clear-location are mutually exclusive")
	}
	if c.URL != "" && c.ClearURL {
		return fmt.Errorf("--url and --clear-url are mutually exclusive")
	}
	if c.Repeat != "" && c.ClearRepeat {
		return fmt.Errorf("--repeat and --clear-repeat are mutually exclusive")
	}
	if c.Repeat == "" && c.RepeatUntil != "" {
		return fmt.Errorf("--repeat-until requires --repeat")
	}
	var recurrence *cloudkit.RecurrenceRule
	if c.Repeat != "" {
		frequency, err := parseRecurrenceFrequency(c.Repeat)
		if err != nil {
			return err
		}
		if c.RepeatInterval < 1 || c.RepeatInterval > 999 {
			return fmt.Errorf("--repeat-interval must be between 1 and 999")
		}
		recurrence = &cloudkit.RecurrenceRule{Frequency: frequency, Interval: c.RepeatInterval}
		if c.RepeatUntil != "" {
			location, _, err := configuredTimeZone(c.TimeZone)
			if err != nil {
				return err
			}
			end, err := time.ParseInLocation("2006-01-02", c.RepeatUntil, location)
			if err != nil {
				return fmt.Errorf("invalid recurrence end date: %w", err)
			}
			recurrence.EndDate = &end
		}
	}
	earlyReminder, clearEarlyReminder, err := parseEarlyReminder(c.EarlyReminder)
	if err != nil {
		return err
	}
	urgent, err := urgentChange(c.Urgent, c.NoUrgent)
	if err != nil {
		return err
	}
	var location *cloudkit.LocationAlarm
	if locationRequested {
		if c.Latitude == nil || c.Longitude == nil {
			return fmt.Errorf("location alarm requires --latitude and --longitude")
		}
		proximity, err := parseProximity(c.Proximity)
		if err != nil {
			return err
		}
		title := strings.TrimSpace(c.LocationTitle)
		if title == "" {
			title = "Location"
		}
		location = &cloudkit.LocationAlarm{
			Title: title, Address: c.Address, Latitude: *c.Latitude, Longitude: *c.Longitude,
			Radius: c.Radius, Proximity: proximity,
		}
	}
	var flagged *bool
	if c.Flagged || c.NoFlagged {
		value := c.Flagged
		flagged = &value
	}
	if title == nil && description == nil && dueDate == nil && !c.ClearDue && priority == nil && flagged == nil && len(c.Tags) == 0 && len(c.RemoveTags) == 0 && c.Assign == "" && !c.Unassign && location == nil && !c.ClearLocation && c.URL == "" && !c.ClearURL && recurrence == nil && !c.ClearRepeat && earlyReminder == nil && !clearEarlyReminder && urgent == nil {
		return fmt.Errorf("no changes specified; use title, due-date, priority, flag, tag, assignment, location, URL, recurrence, early-reminder, or urgent options")
	}
	if title != nil || description != nil || priority != nil || flagged != nil {
		changes := cloudkit.ReminderChanges{
			Title: title, Notes: description, Priority: priority, Flagged: flagged,
		}
		if err := svc.Update(guid, changes); err != nil {
			return fmt.Errorf("edit reminder: %w", err)
		}
	}
	if dueDate != nil || c.ClearDue {
		if err := svc.UpdateDueDate(guid, dueDate); err != nil {
			return fmt.Errorf("edit reminder due date: %w", err)
		}
	}
	if len(c.Tags) > 0 || len(c.RemoveTags) > 0 {
		if err := svc.UpdateTags(guid, c.Tags, c.RemoveTags); err != nil {
			return fmt.Errorf("edit reminder tags: %w", err)
		}
	}
	if c.Assign != "" || c.Unassign {
		if err := svc.UpdateAssignment(guid, c.Assign, c.Unassign); err != nil {
			return fmt.Errorf("edit reminder assignment: %w", err)
		}
	}
	if location != nil || c.ClearLocation {
		if err := svc.UpdateLocationAlarm(guid, location); err != nil {
			return fmt.Errorf("edit reminder location: %w", err)
		}
	}
	if c.URL != "" || c.ClearURL {
		if err := svc.UpdateURLAttachment(guid, c.URL); err != nil {
			return fmt.Errorf("edit reminder URL: %w", err)
		}
	}
	if recurrence != nil || c.ClearRepeat {
		if err := svc.UpdateRecurrence(guid, recurrence); err != nil {
			return fmt.Errorf("edit reminder recurrence: %w", err)
		}
	}
	if earlyReminder != nil || clearEarlyReminder {
		if err := svc.UpdateEarlyReminder(guid, earlyReminder); err != nil {
			return fmt.Errorf("edit reminder early alert: %w", err)
		}
	}
	if urgent != nil {
		if err := svc.UpdateUrgent(guid, *urgent); err != nil {
			return fmt.Errorf("edit reminder Urgent alarm: %w", err)
		}
	}
	color.Green("✓ Update submitted")
	return nil
}

func parseProximity(value string) (int, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "arriving", "arrive", "enter", "entering":
		return 1, nil
	case "leaving", "leave", "exit", "exiting":
		return 2, nil
	default:
		return 0, fmt.Errorf("invalid proximity %q; use arriving or leaving", value)
	}
}

func parseRecurrenceFrequency(value string) (int, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "daily":
		return 0, nil
	case "weekly":
		return 1, nil
	case "monthly":
		return 2, nil
	case "yearly", "annually":
		return 3, nil
	default:
		return 0, fmt.Errorf("invalid recurrence %q; use daily, weekly, monthly, or yearly", value)
	}
}

func parseEarlyReminder(value string) (*cloudkit.EarlyReminder, bool, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return nil, false, nil
	}
	if value == "clear" || value == "none" || value == "off" || value == "never" {
		return nil, true, nil
	}
	unitCode := -1
	number := value
	for _, suffix := range []struct {
		text string
		unit int
	}{{"mo", 4}, {"m", 0}, {"h", 1}, {"d", 2}, {"w", 3}} {
		if strings.HasSuffix(value, suffix.text) {
			unitCode = suffix.unit
			number = strings.TrimSuffix(value, suffix.text)
			break
		}
	}
	count, err := strconv.Atoi(number)
	if err != nil || unitCode < 0 || count < 1 || count > 999 {
		return nil, false, fmt.Errorf("invalid early reminder %q; use a positive offset such as 15m, 1h, 2d, 1w, or 1mo", value)
	}
	return &cloudkit.EarlyReminder{Unit: unitCode, Count: count}, false, nil
}

func urgentChange(enable, disable bool) (*bool, error) {
	if enable && disable {
		return nil, fmt.Errorf("--urgent and --no-urgent are mutually exclusive")
	}
	if !enable && !disable {
		return nil, nil
	}
	value := enable
	return &value, nil
}

// ShareesCmd lists assignment candidates for one shared list.
type ShareesCmd struct {
	List string `arg:"" help:"Shared list name or GUID"`
}

func (c *ShareesCmd) Run() error {
	svc, err := getRemindersService()
	if err != nil {
		return err
	}
	lists, err := svc.GetLists()
	if err != nil {
		return err
	}
	listID := ""
	for _, list := range lists {
		if strings.EqualFold(list.Title, c.List) || list.GUID == c.List {
			if listID != "" {
				return fmt.Errorf("multiple lists match %q; use the list GUID", c.List)
			}
			listID = list.GUID
		}
	}
	if listID == "" {
		return fmt.Errorf("list not found: %s", c.List)
	}
	sharees, err := svc.GetSharees(listID)
	if err != nil {
		return err
	}
	for _, sharee := range sharees {
		name := sharee.DisplayName
		if name == "" {
			name = sharee.Email
		}
		marker := ""
		if sharee.CurrentUser {
			marker = " (me)"
		}
		fmt.Printf("%s%s\n", name, marker)
		if sharee.Email != "" && !strings.EqualFold(name, sharee.Email) {
			fmt.Printf("  %s\n", sharee.Email)
		}
		fmt.Printf("  ID: %s\n", sharee.ParticipantID)
	}
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
			return item.RecordName, nil
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
	Flat bool   `help:"Show subtasks as a flat list"`
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

	renderReminders(os.Stdout, reminders, c.Flat)
	return nil
}

type reminderTreeNode struct {
	reminder reminders.ParsedReminder
	children []*reminderTreeNode
}

func renderReminders(writer io.Writer, items []reminders.ParsedReminder, flat bool) {
	fmt.Fprintln(writer)
	if flat {
		for _, item := range items {
			renderReminder(writer, item, "", "", true)
		}
		fmt.Fprintln(writer)
		return
	}

	nodes := make([]*reminderTreeNode, 0, len(items))
	byRecordName := make(map[string]*reminderTreeNode, len(items))
	for _, item := range items {
		node := &reminderTreeNode{reminder: item}
		nodes = append(nodes, node)
		byRecordName[item.RecordName] = node
	}
	var roots []*reminderTreeNode
	for _, node := range nodes {
		parent := byRecordName[node.reminder.ParentRecordName]
		if parent == nil || parent == node {
			roots = append(roots, node)
			continue
		}
		parent.children = append(parent.children, node)
	}

	rendered := make(map[*reminderTreeNode]bool, len(nodes))
	for _, root := range roots {
		renderReminderTree(writer, root, "", "", true, rendered)
	}
	// Cyclic or malformed parent references must not hide reminders.
	for _, node := range nodes {
		if !rendered[node] {
			renderReminderTree(writer, node, "", "", true, rendered)
		}
	}
	fmt.Fprintln(writer)
}

func renderReminderTree(writer io.Writer, node *reminderTreeNode, prefix, connector string, last bool, rendered map[*reminderTreeNode]bool) {
	if rendered[node] {
		return
	}
	rendered[node] = true
	renderReminder(writer, node.reminder, prefix, connector, last)
	childPrefix := prefix
	if connector != "" {
		if last {
			childPrefix += "   "
		} else {
			childPrefix += "│  "
		}
	}
	for index, child := range node.children {
		isLast := index == len(node.children)-1
		branch := "├─"
		if isLast {
			branch = "└─"
		}
		renderReminderTree(writer, child, childPrefix, branch, isLast, rendered)
	}
}

func renderReminder(writer io.Writer, r reminders.ParsedReminder, prefix, connector string, last bool) {
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

	linePrefix := "  " + prefix
	if connector != "" {
		linePrefix += connector + " "
	}
	fmt.Fprintf(writer, "%s%s %s%s\n", linePrefix, bullet, titleColor.Sprint(r.Title), priority)
	detailPrefix := "    "
	if connector != "" {
		detailPrefix = "  " + prefix
		if last {
			detailPrefix += "   "
		} else {
			detailPrefix += "│  "
		}
		detailPrefix += "  "
	}

	// Due date
	if r.DueDate != nil {
		dueStr := formatDueDate(*r.DueDate)
		if r.DueDate.Before(time.Now()) && !r.Completed {
			fmt.Fprintf(writer, "%s%s\n", detailPrefix, color.RedString("⏰ %s (overdue)", dueStr))
		} else {
			fmt.Fprintf(writer, "%s%s\n", detailPrefix, color.HiBlackString("⏰ %s", dueStr))
		}
	}

	// Description
	if r.Description != "" {
		fmt.Fprintf(writer, "%s%s\n", detailPrefix, color.HiBlackString(r.Description))
	}

	// GUID for reference
	fmt.Fprintf(writer, "%s%s\n", detailPrefix, color.HiBlackString("ID: %s", shortReminderID(r.GUID)))
}

func shortReminderID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
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
	Title         string `arg:"" help:"Reminder title"`
	List          string `short:"l" help:"List name or GUID"`
	Description   string `short:"d" help:"Description"`
	Due           string `help:"Due date (e.g., 'tomorrow 14:00', '2024-01-20')"`
	TimeZone      string `name:"timezone" help:"IANA timezone for due dates (persisted)"`
	Priority      string `short:"p" help:"Priority: high, medium, low (default: none)"`
	Parent        string `help:"Parent reminder ID or unique prefix"`
	EarlyReminder string `name:"early-reminder" help:"Alert before due date (e.g. 15m, 1h, 2d, 1w, or 1mo)"`
	Urgent        bool   `help:"Enable the Urgent alarm"`
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
	var dueDate *cloudkit.DueDateChange
	if c.Due != "" {
		location, zone, err := configuredTimeZone(c.TimeZone)
		if err != nil {
			return err
		}
		parsed, allDay, err := parseDueDate(c.Due, location)
		if err != nil {
			return fmt.Errorf("invalid due date: %w", err)
		}
		dueDate = &cloudkit.DueDateChange{Date: parsed, AllDay: allDay, TimeZone: zone}
	}

	// Parse priority
	priority := 0
	if c.Priority != "" {
		priority, err = parsePriority(c.Priority)
		if err != nil {
			return err
		}
	}

	var parentGUID string
	if c.Parent != "" {
		parentGUID, err = resolveReminderID(svc, c.Parent)
		if err != nil {
			return fmt.Errorf("resolve parent reminder: %w", err)
		}
	}

	earlyReminder, clearEarlyReminder, err := parseEarlyReminder(c.EarlyReminder)
	if err != nil {
		return err
	}
	if clearEarlyReminder {
		return fmt.Errorf("--early-reminder clear is only valid when editing a reminder")
	}
	if earlyReminder != nil && dueDate == nil {
		return fmt.Errorf("--early-reminder requires --due when adding a reminder")
	}

	if err := svc.Add(c.Title, c.Description, listGUID, dueDate, priority, parentGUID, earlyReminder, c.Urgent); err != nil {
		return fmt.Errorf("add reminder: %w", err)
	}

	color.Green("✓ Added: %s", c.Title)
	return nil
}

func parseDueDate(s string, location *time.Location) (time.Time, bool, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	now := time.Now().In(location)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())

	// Handle relative dates
	switch {
	case s == "today":
		return today, true, nil
	case s == "tomorrow":
		return today.AddDate(0, 0, 1), true, nil
	case strings.HasPrefix(s, "tomorrow "):
		timeStr := strings.TrimPrefix(s, "tomorrow ")
		t, err := time.Parse("15:04", timeStr)
		if err != nil {
			return time.Time{}, false, err
		}
		return today.AddDate(0, 0, 1).Add(time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute), false, nil
	default:
		// Try various formats
		formats := []string{
			"2006-01-02 15:04",
			"2006-01-02",
			"Jan 2 15:04",
			"Jan 2",
		}
		for _, format := range formats {
			if t, err := time.ParseInLocation(format, s, location); err == nil {
				if t.Year() == 0 {
					t = time.Date(now.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), 0, 0, now.Location())
				}
				allDay := format == "2006-01-02" || format == "Jan 2"
				return t, allDay, nil
			}
		}
		return time.Time{}, false, fmt.Errorf("unrecognized format: %s", s)
	}
}

func configuredTimeZone(override string) (*time.Location, string, error) {
	settings, err := config.LoadSettings()
	if err != nil {
		return nil, "", fmt.Errorf("load settings: %w", err)
	}
	zone := strings.TrimSpace(override)
	if zone == "" {
		zone = strings.TrimSpace(settings.TimeZone)
	}
	if zone == "" {
		zone = strings.TrimSpace(os.Getenv("TZ"))
	}
	if zone == "" {
		if target, err := filepath.EvalSymlinks("/etc/localtime"); err == nil {
			const marker = "/zoneinfo/"
			if index := strings.Index(target, marker); index >= 0 {
				zone = target[index+len(marker):]
			}
		}
	}
	if zone == "" || zone == "Local" {
		return nil, "", fmt.Errorf("cannot determine an IANA timezone; pass --timezone, for example --timezone Europe/Paris")
	}
	location, err := time.LoadLocation(zone)
	if err != nil {
		return nil, "", fmt.Errorf("invalid IANA timezone %q: %w", zone, err)
	}
	if settings.TimeZone != zone {
		settings.TimeZone = zone
		if err := settings.Save(); err != nil {
			return nil, "", fmt.Errorf("save timezone: %w", err)
		}
	}
	return location, zone, nil
}

// DoneCmd marks a reminder as complete.
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
