// Package beads provides a wrapper for the bd (beads) CLI.
package beads

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// Common errors
var (
	ErrNotInstalled = errors.New("bd not installed: run 'pip install beads-cli' or see https://github.com/anthropics/beads")
	ErrNotARepo     = errors.New("not a beads repository (no .beads directory found)")
	ErrSyncConflict = errors.New("beads sync conflict")
	ErrNotFound     = errors.New("issue not found")
)

// Issue represents a beads issue.
type Issue struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Status      string   `json:"status"`
	Priority    int      `json:"priority"`
	Type        string   `json:"issue_type"`
	CreatedAt   string   `json:"created_at"`
	UpdatedAt   string   `json:"updated_at"`
	ClosedAt    string   `json:"closed_at,omitempty"`
	Parent      string   `json:"parent,omitempty"`
	Assignee    string   `json:"assignee,omitempty"`
	Children    []string `json:"children,omitempty"`
	DependsOn   []string `json:"depends_on,omitempty"`
	Blocks      []string `json:"blocks,omitempty"`
	BlockedBy   []string `json:"blocked_by,omitempty"`

	// Counts from list output
	DependencyCount int `json:"dependency_count,omitempty"`
	DependentCount  int `json:"dependent_count,omitempty"`
	BlockedByCount  int `json:"blocked_by_count,omitempty"`

	// Detailed dependency info from show output
	Dependencies []IssueDep `json:"dependencies,omitempty"`
	Dependents   []IssueDep `json:"dependents,omitempty"`
}

// IssueDep represents a dependency or dependent issue with its relation.
type IssueDep struct {
	ID             string `json:"id"`
	Title          string `json:"title"`
	Status         string `json:"status"`
	Priority       int    `json:"priority"`
	Type           string `json:"issue_type"`
	DependencyType string `json:"dependency_type,omitempty"`
}

// ListOptions specifies filters for listing issues.
type ListOptions struct {
	Status     string // "open", "closed", "all"
	Type       string // "task", "bug", "feature", "epic"
	Priority   int    // 0-4, -1 for no filter
	Parent     string // filter by parent ID
	Assignee   string // filter by assignee (e.g., "gastown/Toast")
	NoAssignee bool   // filter for issues with no assignee
}

// CreateOptions specifies options for creating an issue.
type CreateOptions struct {
	Title       string
	Type        string // "task", "bug", "feature", "epic"
	Priority    int    // 0-4
	Description string
	Parent      string
}

// UpdateOptions specifies options for updating an issue.
type UpdateOptions struct {
	Title        *string
	Status       *string
	Priority     *int
	Description  *string
	Assignee     *string
	AddLabels    []string // Labels to add
	RemoveLabels []string // Labels to remove
	SetLabels    []string // Labels to set (replaces all existing)
}

// SyncStatus represents the sync status of the beads repository.
type SyncStatus struct {
	Branch    string
	Ahead     int
	Behind    int
	Conflicts []string
}

// Beads wraps bd CLI operations for a working directory.
type Beads struct {
	workDir string
}

// New creates a new Beads wrapper for the given directory.
func New(workDir string) *Beads {
	return &Beads{workDir: workDir}
}

// run executes a bd command and returns stdout.
func (b *Beads) run(args ...string) ([]byte, error) {
	cmd := exec.Command("bd", args...)
	cmd.Dir = b.workDir

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		return nil, b.wrapError(err, stderr.String(), args)
	}

	return stdout.Bytes(), nil
}

// wrapError wraps bd errors with context.
func (b *Beads) wrapError(err error, stderr string, args []string) error {
	stderr = strings.TrimSpace(stderr)

	// Check for bd not installed
	if execErr, ok := err.(*exec.Error); ok && errors.Is(execErr.Err, exec.ErrNotFound) {
		return ErrNotInstalled
	}

	// Detect specific error types from stderr
	if strings.Contains(stderr, "not a beads repository") ||
		strings.Contains(stderr, "No .beads directory") ||
		strings.Contains(stderr, ".beads") && strings.Contains(stderr, "not found") {
		return ErrNotARepo
	}
	if strings.Contains(stderr, "sync conflict") || strings.Contains(stderr, "CONFLICT") {
		return ErrSyncConflict
	}
	if strings.Contains(stderr, "not found") || strings.Contains(stderr, "Issue not found") {
		return ErrNotFound
	}

	if stderr != "" {
		return fmt.Errorf("bd %s: %s", strings.Join(args, " "), stderr)
	}
	return fmt.Errorf("bd %s: %w", strings.Join(args, " "), err)
}

// List returns issues matching the given options.
func (b *Beads) List(opts ListOptions) ([]*Issue, error) {
	args := []string{"list", "--json"}

	if opts.Status != "" {
		args = append(args, "--status="+opts.Status)
	}
	if opts.Type != "" {
		args = append(args, "--type="+opts.Type)
	}
	if opts.Priority >= 0 {
		args = append(args, fmt.Sprintf("--priority=%d", opts.Priority))
	}
	if opts.Parent != "" {
		args = append(args, "--parent="+opts.Parent)
	}
	if opts.Assignee != "" {
		args = append(args, "--assignee="+opts.Assignee)
	}
	if opts.NoAssignee {
		args = append(args, "--no-assignee")
	}

	out, err := b.run(args...)
	if err != nil {
		return nil, err
	}

	var issues []*Issue
	if err := json.Unmarshal(out, &issues); err != nil {
		return nil, fmt.Errorf("parsing bd list output: %w", err)
	}

	return issues, nil
}

// ListByAssignee returns all issues assigned to a specific assignee.
// The assignee is typically in the format "rig/polecatName" (e.g., "gastown/Toast").
func (b *Beads) ListByAssignee(assignee string) ([]*Issue, error) {
	return b.List(ListOptions{
		Status:   "all", // Include both open and closed for state derivation
		Assignee: assignee,
		Priority: -1, // No priority filter
	})
}

// GetAssignedIssue returns the first open issue assigned to the given assignee.
// Returns nil if no open issue is assigned.
func (b *Beads) GetAssignedIssue(assignee string) (*Issue, error) {
	issues, err := b.List(ListOptions{
		Status:   "open",
		Assignee: assignee,
		Priority: -1,
	})
	if err != nil {
		return nil, err
	}

	// Also check in_progress status explicitly
	if len(issues) == 0 {
		issues, err = b.List(ListOptions{
			Status:   "in_progress",
			Assignee: assignee,
			Priority: -1,
		})
		if err != nil {
			return nil, err
		}
	}

	if len(issues) == 0 {
		return nil, nil
	}

	return issues[0], nil
}

// Ready returns issues that are ready to work (not blocked).
func (b *Beads) Ready() ([]*Issue, error) {
	out, err := b.run("ready", "--json")
	if err != nil {
		return nil, err
	}

	var issues []*Issue
	if err := json.Unmarshal(out, &issues); err != nil {
		return nil, fmt.Errorf("parsing bd ready output: %w", err)
	}

	return issues, nil
}

// ReadyWithType returns ready issues filtered by type.
// Uses bd ready --type flag for server-side filtering (gt-ktf3).
func (b *Beads) ReadyWithType(issueType string) ([]*Issue, error) {
	out, err := b.run("ready", "--json", "--type", issueType, "-n", "100")
	if err != nil {
		return nil, err
	}

	var issues []*Issue
	if err := json.Unmarshal(out, &issues); err != nil {
		return nil, fmt.Errorf("parsing bd ready output: %w", err)
	}

	return issues, nil
}

// Show returns detailed information about an issue.
func (b *Beads) Show(id string) (*Issue, error) {
	out, err := b.run("show", id, "--json")
	if err != nil {
		return nil, err
	}

	// bd show --json returns an array with one element
	var issues []*Issue
	if err := json.Unmarshal(out, &issues); err != nil {
		return nil, fmt.Errorf("parsing bd show output: %w", err)
	}

	if len(issues) == 0 {
		return nil, ErrNotFound
	}

	return issues[0], nil
}

// Blocked returns issues that are blocked by dependencies.
func (b *Beads) Blocked() ([]*Issue, error) {
	out, err := b.run("blocked", "--json")
	if err != nil {
		return nil, err
	}

	var issues []*Issue
	if err := json.Unmarshal(out, &issues); err != nil {
		return nil, fmt.Errorf("parsing bd blocked output: %w", err)
	}

	return issues, nil
}

// Create creates a new issue and returns it.
func (b *Beads) Create(opts CreateOptions) (*Issue, error) {
	args := []string{"create", "--json"}

	if opts.Title != "" {
		args = append(args, "--title="+opts.Title)
	}
	if opts.Type != "" {
		args = append(args, "--type="+opts.Type)
	}
	if opts.Priority >= 0 {
		args = append(args, fmt.Sprintf("--priority=%d", opts.Priority))
	}
	if opts.Description != "" {
		args = append(args, "--description="+opts.Description)
	}
	if opts.Parent != "" {
		args = append(args, "--parent="+opts.Parent)
	}

	out, err := b.run(args...)
	if err != nil {
		return nil, err
	}

	var issue Issue
	if err := json.Unmarshal(out, &issue); err != nil {
		return nil, fmt.Errorf("parsing bd create output: %w", err)
	}

	return &issue, nil
}

// Update updates an existing issue.
func (b *Beads) Update(id string, opts UpdateOptions) error {
	args := []string{"update", id}

	if opts.Title != nil {
		args = append(args, "--title="+*opts.Title)
	}
	if opts.Status != nil {
		args = append(args, "--status="+*opts.Status)
	}
	if opts.Priority != nil {
		args = append(args, fmt.Sprintf("--priority=%d", *opts.Priority))
	}
	if opts.Description != nil {
		args = append(args, "--description="+*opts.Description)
	}
	if opts.Assignee != nil {
		args = append(args, "--assignee="+*opts.Assignee)
	}
	// Label operations: set-labels replaces all, otherwise use add/remove
	if len(opts.SetLabels) > 0 {
		for _, label := range opts.SetLabels {
			args = append(args, "--set-labels="+label)
		}
	} else {
		for _, label := range opts.AddLabels {
			args = append(args, "--add-label="+label)
		}
		for _, label := range opts.RemoveLabels {
			args = append(args, "--remove-label="+label)
		}
	}

	_, err := b.run(args...)
	return err
}

// Close closes one or more issues.
func (b *Beads) Close(ids ...string) error {
	if len(ids) == 0 {
		return nil
	}

	args := append([]string{"close"}, ids...)
	_, err := b.run(args...)
	return err
}

// CloseWithReason closes one or more issues with a reason.
func (b *Beads) CloseWithReason(reason string, ids ...string) error {
	if len(ids) == 0 {
		return nil
	}

	args := append([]string{"close"}, ids...)
	args = append(args, "--reason="+reason)
	_, err := b.run(args...)
	return err
}

// Release moves an in_progress issue back to open status.
// This is used to recover stuck steps when a worker dies mid-task.
// It clears the assignee so the step can be claimed by another worker.
func (b *Beads) Release(id string) error {
	return b.ReleaseWithReason(id, "")
}

// ReleaseWithReason moves an in_progress issue back to open status with a reason.
// The reason is added as a note to the issue for tracking purposes.
func (b *Beads) ReleaseWithReason(id, reason string) error {
	args := []string{"update", id, "--status=open", "--assignee="}

	// Add reason as a note if provided
	if reason != "" {
		args = append(args, "--notes=Released: "+reason)
	}

	_, err := b.run(args...)
	return err
}

// AddDependency adds a dependency: issue depends on dependsOn.
func (b *Beads) AddDependency(issue, dependsOn string) error {
	_, err := b.run("dep", "add", issue, dependsOn)
	return err
}

// RemoveDependency removes a dependency.
func (b *Beads) RemoveDependency(issue, dependsOn string) error {
	_, err := b.run("dep", "remove", issue, dependsOn)
	return err
}

// Sync syncs beads with remote.
func (b *Beads) Sync() error {
	_, err := b.run("sync")
	return err
}

// SyncFromMain syncs beads updates from main branch.
func (b *Beads) SyncFromMain() error {
	_, err := b.run("sync", "--from-main")
	return err
}

// SyncStatus returns the sync status without performing a sync.
func (b *Beads) SyncStatus() (*SyncStatus, error) {
	out, err := b.run("sync", "--status", "--json")
	if err != nil {
		// If sync branch doesn't exist, return empty status
		if strings.Contains(err.Error(), "does not exist") {
			return &SyncStatus{}, nil
		}
		return nil, err
	}

	var status SyncStatus
	if err := json.Unmarshal(out, &status); err != nil {
		return nil, fmt.Errorf("parsing bd sync status output: %w", err)
	}

	return &status, nil
}

// Stats returns repository statistics.
func (b *Beads) Stats() (string, error) {
	out, err := b.run("stats")
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// IsBeadsRepo checks if the working directory is a beads repository.
func (b *Beads) IsBeadsRepo() bool {
	_, err := b.run("list", "--limit=1")
	return err == nil || !errors.Is(err, ErrNotARepo)
}

// StatusPinned is the status for pinned beads that never get closed.
const StatusPinned = "pinned"

// HandoffBeadTitle returns the well-known title for a role's handoff bead.
func HandoffBeadTitle(role string) string {
	return role + " Handoff"
}

// FindHandoffBead finds the pinned handoff bead for a role by title.
// Returns nil if not found (not an error).
func (b *Beads) FindHandoffBead(role string) (*Issue, error) {
	issues, err := b.List(ListOptions{Status: StatusPinned, Priority: -1})
	if err != nil {
		return nil, fmt.Errorf("listing pinned issues: %w", err)
	}

	targetTitle := HandoffBeadTitle(role)
	for _, issue := range issues {
		if issue.Title == targetTitle {
			return issue, nil
		}
	}

	return nil, nil
}

// GetOrCreateHandoffBead returns the handoff bead for a role, creating it if needed.
func (b *Beads) GetOrCreateHandoffBead(role string) (*Issue, error) {
	// Check if it exists
	existing, err := b.FindHandoffBead(role)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return existing, nil
	}

	// Create new handoff bead
	issue, err := b.Create(CreateOptions{
		Title:       HandoffBeadTitle(role),
		Type:        "task",
		Priority:    2,
		Description: "", // Empty until first handoff
	})
	if err != nil {
		return nil, fmt.Errorf("creating handoff bead: %w", err)
	}

	// Update to pinned status
	status := StatusPinned
	if err := b.Update(issue.ID, UpdateOptions{Status: &status}); err != nil {
		return nil, fmt.Errorf("setting handoff bead to pinned: %w", err)
	}

	// Re-fetch to get updated status
	return b.Show(issue.ID)
}

// UpdateHandoffContent updates the handoff bead's description with new content.
func (b *Beads) UpdateHandoffContent(role, content string) error {
	issue, err := b.GetOrCreateHandoffBead(role)
	if err != nil {
		return err
	}

	return b.Update(issue.ID, UpdateOptions{Description: &content})
}

// ClearHandoffContent clears the handoff bead's description.
func (b *Beads) ClearHandoffContent(role string) error {
	issue, err := b.FindHandoffBead(role)
	if err != nil {
		return err
	}
	if issue == nil {
		return nil // Nothing to clear
	}

	empty := ""
	return b.Update(issue.ID, UpdateOptions{Description: &empty})
}

// MRFields holds the structured fields for a merge-request issue.
// These fields are stored as key: value lines in the issue description.
type MRFields struct {
	Branch      string // Source branch name (e.g., "polecat/Nux/gt-xyz")
	Target      string // Target branch (e.g., "main" or "integration/gt-epic")
	SourceIssue string // The work item being merged (e.g., "gt-xyz")
	Worker      string // Who did the work
	Rig         string // Which rig
	MergeCommit string // SHA of merge commit (set on close)
	CloseReason string // Reason for closing: merged, rejected, conflict, superseded
}

// ParseMRFields extracts structured merge-request fields from an issue's description.
// Fields are expected as "key: value" lines, with optional prose text mixed in.
// Returns nil if no MR fields are found.
func ParseMRFields(issue *Issue) *MRFields {
	if issue == nil || issue.Description == "" {
		return nil
	}

	fields := &MRFields{}
	hasFields := false

	for _, line := range strings.Split(issue.Description, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// Look for "key: value" pattern
		colonIdx := strings.Index(line, ":")
		if colonIdx == -1 {
			continue
		}

		key := strings.TrimSpace(line[:colonIdx])
		value := strings.TrimSpace(line[colonIdx+1:])
		if value == "" {
			continue
		}

		// Map keys to fields (case-insensitive)
		switch strings.ToLower(key) {
		case "branch":
			fields.Branch = value
			hasFields = true
		case "target":
			fields.Target = value
			hasFields = true
		case "source_issue", "source-issue", "sourceissue":
			fields.SourceIssue = value
			hasFields = true
		case "worker":
			fields.Worker = value
			hasFields = true
		case "rig":
			fields.Rig = value
			hasFields = true
		case "merge_commit", "merge-commit", "mergecommit":
			fields.MergeCommit = value
			hasFields = true
		case "close_reason", "close-reason", "closereason":
			fields.CloseReason = value
			hasFields = true
		}
	}

	if !hasFields {
		return nil
	}
	return fields
}

// FormatMRFields formats MRFields as a string suitable for an issue description.
// Only non-empty fields are included.
func FormatMRFields(fields *MRFields) string {
	if fields == nil {
		return ""
	}

	var lines []string

	if fields.Branch != "" {
		lines = append(lines, "branch: "+fields.Branch)
	}
	if fields.Target != "" {
		lines = append(lines, "target: "+fields.Target)
	}
	if fields.SourceIssue != "" {
		lines = append(lines, "source_issue: "+fields.SourceIssue)
	}
	if fields.Worker != "" {
		lines = append(lines, "worker: "+fields.Worker)
	}
	if fields.Rig != "" {
		lines = append(lines, "rig: "+fields.Rig)
	}
	if fields.MergeCommit != "" {
		lines = append(lines, "merge_commit: "+fields.MergeCommit)
	}
	if fields.CloseReason != "" {
		lines = append(lines, "close_reason: "+fields.CloseReason)
	}

	return strings.Join(lines, "\n")
}

// SetMRFields updates an issue's description with the given MR fields.
// Existing MR field lines are replaced; other content is preserved.
// Returns the new description string.
func SetMRFields(issue *Issue, fields *MRFields) string {
	if issue == nil {
		return FormatMRFields(fields)
	}

	// Known MR field keys (lowercase)
	mrKeys := map[string]bool{
		"branch":       true,
		"target":       true,
		"source_issue": true,
		"source-issue": true,
		"sourceissue":  true,
		"worker":       true,
		"rig":          true,
		"merge_commit": true,
		"merge-commit": true,
		"mergecommit":  true,
		"close_reason": true,
		"close-reason": true,
		"closereason":  true,
	}

	// Collect non-MR lines from existing description
	var otherLines []string
	if issue.Description != "" {
		for _, line := range strings.Split(issue.Description, "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" {
				// Preserve blank lines in content
				otherLines = append(otherLines, line)
				continue
			}

			// Check if this is an MR field line
			colonIdx := strings.Index(trimmed, ":")
			if colonIdx == -1 {
				otherLines = append(otherLines, line)
				continue
			}

			key := strings.ToLower(strings.TrimSpace(trimmed[:colonIdx]))
			if !mrKeys[key] {
				otherLines = append(otherLines, line)
			}
			// Skip MR field lines - they'll be replaced
		}
	}

	// Build new description: MR fields first, then other content
	formatted := FormatMRFields(fields)

	// Trim trailing blank lines from other content
	for len(otherLines) > 0 && strings.TrimSpace(otherLines[len(otherLines)-1]) == "" {
		otherLines = otherLines[:len(otherLines)-1]
	}
	// Trim leading blank lines from other content
	for len(otherLines) > 0 && strings.TrimSpace(otherLines[0]) == "" {
		otherLines = otherLines[1:]
	}

	if formatted == "" {
		return strings.Join(otherLines, "\n")
	}
	if len(otherLines) == 0 {
		return formatted
	}

	return formatted + "\n\n" + strings.Join(otherLines, "\n")
}
