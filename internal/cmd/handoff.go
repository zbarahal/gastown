package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/workspace"
)

// HandoffAction for handoff command.
type HandoffAction string

const (
	HandoffCycle    HandoffAction = "cycle"    // Restart with handoff mail
	HandoffRestart  HandoffAction = "restart"  // Fresh restart, no handoff
	HandoffShutdown HandoffAction = "shutdown" // Terminate, no restart
)

var handoffCmd = &cobra.Command{
	Use:   "handoff",
	Short: "Request lifecycle action (retirement/restart)",
	Long: `Request a lifecycle action from your manager.

This command initiates graceful retirement:
1. Verifies git state is clean
2. For polecats (shutdown): auto-submits MR to merge queue
3. Sends handoff mail to yourself (for cycle)
4. Sends lifecycle request to your manager
5. Sets requesting state and waits for retirement

Your manager (daemon for Mayor/Witness, witness for polecats) will
verify the request and terminate your session. For cycle/restart,
a new session starts and reads your handoff mail to continue work.

Polecat auto-MR:
When a polecat runs 'gt handoff' (default: shutdown), the current branch
is automatically submitted to the merge queue if it follows the
polecat/<name>/<issue> naming convention. The Refinery will process
the merge request.

Flags:
  --cycle     Restart with handoff mail (default for Mayor/Witness)
  --restart   Fresh restart, no handoff context
  --shutdown  Terminate without restart (default for polecats)

Examples:
  gt handoff           # Use role-appropriate default
  gt handoff --cycle   # Restart with context handoff
  gt handoff --restart # Fresh restart
`,
	RunE: runHandoff,
}

var (
	handoffCycle    bool
	handoffRestart  bool
	handoffShutdown bool
	handoffForce    bool
	handoffMessage  string
)

func init() {
	handoffCmd.Flags().BoolVar(&handoffCycle, "cycle", false, "Restart with handoff mail")
	handoffCmd.Flags().BoolVar(&handoffRestart, "restart", false, "Fresh restart, no handoff")
	handoffCmd.Flags().BoolVar(&handoffShutdown, "shutdown", false, "Terminate without restart")
	handoffCmd.Flags().BoolVarP(&handoffForce, "force", "f", false, "Skip pre-flight checks")
	handoffCmd.Flags().StringVarP(&handoffMessage, "message", "m", "", "Handoff message for successor")

	rootCmd.AddCommand(handoffCmd)
}

func runHandoff(cmd *cobra.Command, args []string) error {
	// Detect our role
	role := detectHandoffRole()
	if role == RoleUnknown {
		return fmt.Errorf("cannot detect agent role (set GT_ROLE or run from known context)")
	}

	// Determine action
	action := determineAction(role)

	fmt.Printf("Agent role: %s\n", style.Bold.Render(string(role)))
	fmt.Printf("Action: %s\n", style.Bold.Render(string(action)))

	// Find workspace
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	// Pre-flight checks (unless forced)
	if !handoffForce {
		if err := preFlightChecks(); err != nil {
			return fmt.Errorf("pre-flight check failed: %w\n\nUse --force to skip checks", err)
		}
	}

	// For polecats shutting down with work complete, auto-submit MR to merge queue
	if role == RolePolecat && action == HandoffShutdown {
		if err := submitMRForPolecat(); err != nil {
			// Non-fatal: warn but continue with handoff
			fmt.Printf("%s Could not auto-submit MR: %v\n", style.Warning.Render("Warning:"), err)
			fmt.Println(style.Dim.Render("  You may need to run 'gt mq submit' manually"))
		} else {
			fmt.Printf("%s Auto-submitted work to merge queue\n", style.Bold.Render("✓"))
		}
	}

	// For cycle, update handoff bead for successor
	if action == HandoffCycle {
		if err := sendHandoffMail(role, townRoot); err != nil {
			return fmt.Errorf("updating handoff bead: %w", err)
		}
		fmt.Printf("%s Updated handoff bead for successor\n", style.Bold.Render("✓"))
	}

	// Send lifecycle request to manager
	manager := getManager(role)
	if err := sendLifecycleRequest(manager, role, action, townRoot); err != nil {
		return fmt.Errorf("sending lifecycle request: %w", err)
	}
	fmt.Printf("%s Sent %s request to %s\n", style.Bold.Render("✓"), action, manager)

	// Signal daemon for immediate processing (if manager is deacon)
	if manager == "deacon/" {
		if err := signalDaemon(townRoot); err != nil {
			// Non-fatal: daemon will eventually poll
			fmt.Printf("%s Could not signal daemon (will poll): %v\n", style.Dim.Render("○"), err)
		} else {
			fmt.Printf("%s Signaled daemon for immediate processing\n", style.Bold.Render("✓"))
		}
	}

	// Set requesting state
	if err := setRequestingState(role, action, townRoot); err != nil {
		fmt.Printf("Warning: failed to set state: %v\n", err)
	}

	// Wait for retirement with timeout warning
	fmt.Println()
	fmt.Printf("%s Waiting for retirement...\n", style.Dim.Render("◌"))
	fmt.Println(style.Dim.Render("(Manager will terminate this session)"))

	// Wait with periodic warnings - manager should kill us
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	waitStart := time.Now()
	for {
		select {
		case <-ticker.C:
			elapsed := time.Since(waitStart).Round(time.Second)
			fmt.Printf("%s Still waiting (%v elapsed)...\n", style.Dim.Render("◌"), elapsed)
			if elapsed >= 2*time.Minute {
				fmt.Println(style.Dim.Render("  Hint: If manager isn't responding, you may need to:"))
				fmt.Println(style.Dim.Render("  - Check if daemon/witness is running"))
				fmt.Println(style.Dim.Render("  - Use Ctrl+C to abort and manually exit"))
			}
		}
	}
}

// detectHandoffRole figures out what kind of agent we are.
// Uses GT_ROLE env var, tmux session name, or directory context.
func detectHandoffRole() Role {
	// Check GT_ROLE environment variable first
	if role := os.Getenv("GT_ROLE"); role != "" {
		switch strings.ToLower(role) {
		case "mayor":
			return RoleMayor
		case "witness":
			return RoleWitness
		case "refinery":
			return RoleRefinery
		case "polecat":
			return RolePolecat
		case "crew":
			return RoleCrew
		}
	}

	// Check tmux session name
	out, err := exec.Command("tmux", "display-message", "-p", "#{session_name}").Output()
	if err == nil {
		sessionName := strings.TrimSpace(string(out))
		if sessionName == "gt-mayor" {
			return RoleMayor
		}
		if strings.HasSuffix(sessionName, "-witness") {
			return RoleWitness
		}
		if strings.HasSuffix(sessionName, "-refinery") {
			return RoleRefinery
		}
		// Polecat sessions: gt-<rig>-<name>
		if strings.HasPrefix(sessionName, "gt-") && strings.Count(sessionName, "-") >= 2 {
			return RolePolecat
		}
	}

	// Fall back to directory-based detection
	cwd, err := os.Getwd()
	if err != nil {
		return RoleUnknown
	}

	townRoot, err := workspace.FindFromCwd()
	if err != nil || townRoot == "" {
		return RoleUnknown
	}

	ctx := detectRole(cwd, townRoot)
	return ctx.Role
}

// determineAction picks the action based on flags or role default.
func determineAction(role Role) HandoffAction {
	// Explicit flags take precedence
	if handoffCycle {
		return HandoffCycle
	}
	if handoffRestart {
		return HandoffRestart
	}
	if handoffShutdown {
		return HandoffShutdown
	}

	// Role-based defaults
	switch role {
	case RolePolecat:
		return HandoffShutdown // Ephemeral, work is done
	case RoleMayor, RoleWitness, RoleRefinery:
		return HandoffCycle // Long-running, preserve context
	case RoleCrew:
		return HandoffCycle // Will only send mail, not actually retire
	default:
		return HandoffCycle
	}
}

// preFlightChecks verifies it's safe to retire.
func preFlightChecks() error {
	// Check git status
	cmd := exec.Command("git", "status", "--porcelain")
	out, err := cmd.Output()
	if err != nil {
		// Not a git repo, that's fine
		return nil
	}

	if len(strings.TrimSpace(string(out))) > 0 {
		return fmt.Errorf("uncommitted changes in git working tree")
	}

	return nil
}

// getManager returns the address of our lifecycle manager.
// For polecats and refineries, it detects the rig from context.
func getManager(role Role) string {
	switch role {
	case RoleMayor, RoleWitness:
		return "deacon/"
	case RolePolecat, RoleRefinery:
		// Detect rig from current directory context
		rig := detectRigFromContext()
		if rig == "" {
			// Fallback if rig detection fails - this shouldn't happen
			// in normal operation but is better than a literal placeholder
			return "deacon/"
		}
		return rig + "/witness"
	case RoleCrew:
		return "human" // Crew is human-managed
	default:
		return "deacon/"
	}
}

// detectRigFromContext determines the rig name from the current directory.
func detectRigFromContext() string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}

	townRoot, err := workspace.FindFromCwd()
	if err != nil || townRoot == "" {
		return ""
	}

	ctx := detectRole(cwd, townRoot)
	return ctx.Rig
}

// sendHandoffMail updates the pinned handoff bead for the successor to read.
func sendHandoffMail(role Role, townRoot string) error {
	// Build handoff content
	content := handoffMessage
	if content == "" {
		content = fmt.Sprintf(`🤝 HANDOFF: Session cycling

Time: %s
Role: %s
Action: cycle

Check bd ready for pending work.
Check gt mail inbox for messages received during transition.
`, time.Now().Format(time.RFC3339), role)
	}

	// Determine the handoff role key
	// For role-specific handoffs, use the role name
	roleKey := string(role)

	// Update the pinned handoff bead
	bd := beads.New(townRoot)
	if err := bd.UpdateHandoffContent(roleKey, content); err != nil {
		return fmt.Errorf("updating handoff bead: %w", err)
	}

	return nil
}

// getPolecatName extracts the polecat name from the tmux session.
// Returns empty string if not a polecat session.
func getPolecatName() string {
	out, err := exec.Command("tmux", "display-message", "-p", "#{session_name}").Output()
	if err != nil {
		return ""
	}
	sessionName := strings.TrimSpace(string(out))

	// Polecat sessions: gt-<rig>-<name>
	if strings.HasPrefix(sessionName, "gt-") {
		parts := strings.SplitN(sessionName, "-", 3)
		if len(parts) >= 3 {
			return parts[2] // The polecat name
		}
	}
	return ""
}

// sendLifecycleRequest sends the lifecycle request to our manager.
func sendLifecycleRequest(manager string, role Role, action HandoffAction, townRoot string) error {
	if manager == "human" {
		// Crew is human-managed, just print a message
		fmt.Println(style.Dim.Render("(Crew sessions are human-managed, no lifecycle request sent)"))
		return nil
	}

	// For polecats, include the specific name
	polecatName := ""
	if role == RolePolecat {
		polecatName = getPolecatName()
	}

	subject := fmt.Sprintf("LIFECYCLE: %s requesting %s", role, action)
	body := fmt.Sprintf(`Lifecycle request from %s.

Action: %s
Time: %s
Polecat: %s

Please verify state and execute lifecycle action.
`, role, action, time.Now().Format(time.RFC3339), polecatName)

	// Send via bd mail (syntax: bd mail send <recipient> -s <subject> -m <body>)
	cmd := exec.Command("bd", "mail", "send", manager,
		"-s", subject,
		"-m", body,
	)
	cmd.Dir = townRoot

	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, string(out))
	}

	return nil
}

// submitMRForPolecat submits the current branch to the merge queue.
// This is called automatically when a polecat shuts down with completed work.
func submitMRForPolecat() error {
	// Check if we're on a polecat branch with work to submit
	cmd := exec.Command("git", "branch", "--show-current")
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("getting current branch: %w", err)
	}
	branch := strings.TrimSpace(string(out))

	// Skip if on main/master (no work to submit)
	if branch == "main" || branch == "master" || branch == "" {
		return nil // Nothing to submit, that's OK
	}

	// Check if branch follows polecat/<name>/<issue> pattern
	parts := strings.Split(branch, "/")
	if len(parts) < 3 || parts[0] != "polecat" {
		// Not a polecat work branch, skip
		return nil
	}

	// Run gt mq submit
	submitCmd := exec.Command("gt", "mq", "submit")
	submitOutput, err := submitCmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s", strings.TrimSpace(string(submitOutput)))
	}

	// Print the submit output (trimmed)
	output := strings.TrimSpace(string(submitOutput))
	if output != "" {
		for _, line := range strings.Split(output, "\n") {
			fmt.Printf("  %s\n", line)
		}
	}

	return nil
}

// setRequestingState updates state.json to indicate we're requesting lifecycle action.
func setRequestingState(role Role, action HandoffAction, townRoot string) error {
	// Determine state file location based on role
	var stateFile string
	switch role {
	case RoleMayor:
		stateFile = filepath.Join(townRoot, "mayor", "state.json")
	case RoleWitness:
		// Would need rig context
		stateFile = filepath.Join(townRoot, "witness", "state.json")
	default:
		// For other roles, use a generic location
		stateFile = filepath.Join(townRoot, ".gastown", "agent-state.json")
	}

	// Ensure directory exists
	if err := os.MkdirAll(filepath.Dir(stateFile), 0755); err != nil {
		return err
	}

	// Read existing state or create new
	state := make(map[string]interface{})
	if data, err := os.ReadFile(stateFile); err == nil {
		_ = json.Unmarshal(data, &state)
	}

	// Set requesting state
	state["requesting_"+string(action)] = true
	state["requesting_time"] = time.Now().Format(time.RFC3339)

	// Write back
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(stateFile, data, 0644)
}

// signalDaemon sends SIGUSR1 to the daemon to trigger immediate lifecycle processing.
func signalDaemon(townRoot string) error {
	pidFile := filepath.Join(townRoot, "daemon", "daemon.pid")
	data, err := os.ReadFile(pidFile)
	if err != nil {
		return fmt.Errorf("reading daemon PID: %w", err)
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return fmt.Errorf("parsing daemon PID: %w", err)
	}

	process, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("finding daemon process: %w", err)
	}

	if err := process.Signal(syscall.SIGUSR1); err != nil {
		return fmt.Errorf("signaling daemon: %w", err)
	}

	return nil
}
