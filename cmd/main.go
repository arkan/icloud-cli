package main

import (
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
}

// VersionCmd shows the version
type VersionCmd struct{}

func (c *VersionCmd) Run() error {
	fmt.Printf("icloud-cli v%s\n", version)
	return nil
}

// LoginCmd handles authentication
type LoginCmd struct {
	AppleID string `arg:"" optional:"" help:"Apple ID (email)"`
}

func (c *LoginCmd) Run() error {
	appleID := c.AppleID
	if appleID == "" {
		fmt.Print("Apple ID: ")
		fmt.Scanln(&appleID)
	}

	fmt.Print("Password: ")
	passwordBytes, err := term.ReadPassword(int(syscall.Stdin))
	fmt.Println()
	if err != nil {
		return fmt.Errorf("read password: %w", err)
	}
	password := string(passwordBytes)

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
		if strings.Contains(err.Error(), "409") || strings.Contains(err.Error(), "no session token") {
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

	client := api.NewClient(session)
	authenticator := auth.NewAuthenticator(client)

	_, err = authenticator.Validate()
	if err != nil {
		color.Red("✗ Session expired")
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
	Done  DoneCmd  `cmd:"" help:"Mark a reminder as done"`
	Rm    RmCmd    `cmd:"" help:"Delete a reminder"`
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

	reminders, err := svc.GetReminders(listGUID)
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

	if err := svc.Add(c.Title, c.Description, listGUID, dueDate); err != nil {
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

// DoneCmd marks a reminder as complete
type DoneCmd struct {
	ID string `arg:"" help:"Reminder ID (first 8 chars or full GUID)"`
}

func (c *DoneCmd) Run() error {
	svc, err := getRemindersService()
	if err != nil {
		return err
	}

	// Find full GUID if partial
	guid := c.ID
	if len(guid) < 36 {
		reminders, err := svc.GetReminders("")
		if err != nil {
			return err
		}
		for _, r := range reminders {
			if strings.HasPrefix(r.GUID, guid) {
				guid = r.GUID
				break
			}
		}
	}

	if err := svc.Complete(guid); err != nil {
		return fmt.Errorf("complete reminder: %w", err)
	}

	color.Green("✓ Marked as done")
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

	// Find full GUID if partial
	guid := c.ID
	if len(guid) < 36 {
		reminders, err := svc.GetReminders("")
		if err != nil {
			return err
		}
		for _, r := range reminders {
			if strings.HasPrefix(r.GUID, guid) {
				guid = r.GUID
				break
			}
		}
	}

	if err := svc.Delete(guid); err != nil {
		return fmt.Errorf("delete reminder: %w", err)
	}

	color.Green("✓ Deleted")
	return nil
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

	client := api.NewClient(session)
	authenticator := auth.NewAuthenticator(client)

	// Validate and refresh session
	_, err = authenticator.Validate()
	if err != nil {
		return nil, fmt.Errorf("session expired - run 'icloud login' to refresh")
	}

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
