package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/tmux"
)

// BeadsMessage represents a message from beads mail.
type BeadsMessage struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Sender      string `json:"sender"`
	Assignee    string `json:"assignee"`
	Priority    int    `json:"priority"`
	Status      string `json:"status"`
}

// ProcessLifecycleRequests checks for and processes lifecycle requests from the deacon inbox.
func (d *Daemon) ProcessLifecycleRequests() {
	// Get mail for deacon identity
	cmd := exec.Command("bd", "mail", "inbox", "--identity", "deacon/", "--json")
	cmd.Dir = d.config.TownRoot

	output, err := cmd.Output()
	if err != nil {
		// bd mail might not be available or inbox empty
		return
	}

	if len(output) == 0 || string(output) == "[]" || string(output) == "[]\n" {
		return
	}

	var messages []BeadsMessage
	if err := json.Unmarshal(output, &messages); err != nil {
		d.logger.Printf("Error parsing mail: %v", err)
		return
	}

	for _, msg := range messages {
		if msg.Status == "closed" {
			continue // Already processed
		}

		request := d.parseLifecycleRequest(&msg)
		if request == nil {
			continue // Not a lifecycle request
		}

		d.logger.Printf("Processing lifecycle request from %s: %s", request.From, request.Action)

		if err := d.executeLifecycleAction(request); err != nil {
			d.logger.Printf("Error executing lifecycle action: %v", err)
			continue
		}

		// Mark message as read (close the issue)
		if err := d.closeMessage(msg.ID); err != nil {
			d.logger.Printf("Warning: failed to close message %s: %v", msg.ID, err)
		}
	}
}

// parseLifecycleRequest extracts a lifecycle request from a message.
func (d *Daemon) parseLifecycleRequest(msg *BeadsMessage) *LifecycleRequest {
	// Look for lifecycle keywords in subject/title
	// Expected format: "LIFECYCLE: <role> requesting <action>"
	title := strings.ToLower(msg.Title)

	if !strings.HasPrefix(title, "lifecycle:") {
		return nil
	}

	var action LifecycleAction
	var from string

	if strings.Contains(title, "cycle") || strings.Contains(title, "cycling") {
		action = ActionCycle
	} else if strings.Contains(title, "restart") {
		action = ActionRestart
	} else if strings.Contains(title, "shutdown") || strings.Contains(title, "stop") {
		action = ActionShutdown
	} else {
		return nil
	}

	// Extract role from title: "LIFECYCLE: <role> requesting ..."
	// Parse between "lifecycle: " and " requesting"
	parts := strings.Split(title, " requesting")
	if len(parts) >= 1 {
		rolePart := strings.TrimPrefix(parts[0], "lifecycle:")
		from = strings.TrimSpace(rolePart)
	}

	if from == "" {
		from = msg.Sender // fallback
	}

	return &LifecycleRequest{
		From:      from,
		Action:    action,
		Timestamp: time.Now(),
	}
}

// executeLifecycleAction performs the requested lifecycle action.
func (d *Daemon) executeLifecycleAction(request *LifecycleRequest) error {
	// Determine session name from sender identity
	sessionName := d.identityToSession(request.From)
	if sessionName == "" {
		return fmt.Errorf("unknown agent identity: %s", request.From)
	}

	d.logger.Printf("Executing %s for session %s", request.Action, sessionName)

	// Verify agent state shows requesting_<action>=true before killing
	if err := d.verifyAgentRequestingState(request.From, request.Action); err != nil {
		return fmt.Errorf("state verification failed: %w", err)
	}

	// Check if session exists
	running, err := d.tmux.HasSession(sessionName)
	if err != nil {
		return fmt.Errorf("checking session: %w", err)
	}

	switch request.Action {
	case ActionShutdown:
		if running {
			if err := d.tmux.KillSession(sessionName); err != nil {
				return fmt.Errorf("killing session: %w", err)
			}
			d.logger.Printf("Killed session %s", sessionName)
		}
		return nil

	case ActionCycle, ActionRestart:
		if running {
			// Kill the session first
			if err := d.tmux.KillSession(sessionName); err != nil {
				return fmt.Errorf("killing session: %w", err)
			}
			d.logger.Printf("Killed session %s for restart", sessionName)

			// Wait a moment
			time.Sleep(500 * time.Millisecond)
		}

		// Restart the session
		if err := d.restartSession(sessionName, request.From); err != nil {
			return fmt.Errorf("restarting session: %w", err)
		}
		d.logger.Printf("Restarted session %s", sessionName)
		return nil

	default:
		return fmt.Errorf("unknown action: %s", request.Action)
	}
}

// identityToSession converts a beads identity to a tmux session name.
func (d *Daemon) identityToSession(identity string) string {
	// Handle known identities
	switch identity {
	case "mayor":
		return "gt-mayor"
	default:
		// Pattern: <rig>-witness → gt-<rig>-witness
		if strings.HasSuffix(identity, "-witness") {
			return "gt-" + identity
		}
		// Pattern: <rig>-refinery → gt-<rig>-refinery
		if strings.HasSuffix(identity, "-refinery") {
			return "gt-" + identity
		}
		// Pattern: <rig>-crew-<name> → gt-<rig>-crew-<name>
		if strings.Contains(identity, "-crew-") {
			return "gt-" + identity
		}
		// Unknown identity
		return ""
	}
}

// restartSession starts a new session for the given agent.
func (d *Daemon) restartSession(sessionName, identity string) error {
	// Determine working directory and startup command based on agent type
	var workDir, startCmd string
	var rigName string
	var agentRole string
	var needsPreSync bool

	if identity == "mayor" {
		workDir = d.config.TownRoot
		startCmd = "exec claude --dangerously-skip-permissions"
		agentRole = "coordinator"
	} else if strings.HasSuffix(identity, "-witness") {
		// Extract rig name: <rig>-witness → <rig>
		rigName = strings.TrimSuffix(identity, "-witness")
		workDir = d.config.TownRoot + "/" + rigName
		startCmd = "exec claude --dangerously-skip-permissions"
		agentRole = "witness"
	} else if strings.HasSuffix(identity, "-refinery") {
		// Extract rig name: <rig>-refinery → <rig>
		rigName = strings.TrimSuffix(identity, "-refinery")
		workDir = filepath.Join(d.config.TownRoot, rigName, "refinery", "rig")
		startCmd = "exec claude --dangerously-skip-permissions"
		agentRole = "refinery"
		needsPreSync = true
	} else if strings.Contains(identity, "-crew-") {
		// Extract rig and crew name: <rig>-crew-<name> → <rig>, <name>
		parts := strings.SplitN(identity, "-crew-", 2)
		if len(parts) != 2 {
			return fmt.Errorf("invalid crew identity format: %s", identity)
		}
		rigName = parts[0]
		crewName := parts[1]
		workDir = filepath.Join(d.config.TownRoot, rigName, "crew", crewName)
		startCmd = "exec claude --dangerously-skip-permissions"
		agentRole = "crew"
		needsPreSync = true
	} else {
		return fmt.Errorf("don't know how to restart %s", identity)
	}

	// Pre-sync workspace for agents with git clones (refinery)
	if needsPreSync {
		d.logger.Printf("Pre-syncing workspace for %s at %s", identity, workDir)
		d.syncWorkspace(workDir)
	}

	// Create session
	if err := d.tmux.NewSession(sessionName, workDir); err != nil {
		return fmt.Errorf("creating session: %w", err)
	}

	// Set environment
	_ = d.tmux.SetEnvironment(sessionName, "GT_ROLE", identity)

	// Apply theme
	if identity == "mayor" {
		theme := tmux.MayorTheme()
		_ = d.tmux.ConfigureGasTownSession(sessionName, theme, "", "Mayor", "coordinator")
	} else if rigName != "" {
		theme := tmux.AssignTheme(rigName)
		_ = d.tmux.ConfigureGasTownSession(sessionName, theme, rigName, agentRole, agentRole)
	}

	// Send startup command
	if err := d.tmux.SendKeys(sessionName, startCmd); err != nil {
		return fmt.Errorf("sending startup command: %w", err)
	}

	// Prime after delay
	if err := d.tmux.SendKeysDelayed(sessionName, "gt prime", 2000); err != nil {
		d.logger.Printf("Warning: could not send prime: %v", err)
	}

	return nil
}

// syncWorkspace syncs a git workspace before starting a new session.
// This ensures agents with persistent clones (like refinery) start with current code.
func (d *Daemon) syncWorkspace(workDir string) {
	// Fetch latest from origin
	fetchCmd := exec.Command("git", "fetch", "origin")
	fetchCmd.Dir = workDir
	if err := fetchCmd.Run(); err != nil {
		d.logger.Printf("Warning: git fetch failed in %s: %v", workDir, err)
	}

	// Pull with rebase to incorporate changes
	pullCmd := exec.Command("git", "pull", "--rebase", "origin", "main")
	pullCmd.Dir = workDir
	if err := pullCmd.Run(); err != nil {
		d.logger.Printf("Warning: git pull failed in %s: %v", workDir, err)
		// Don't fail - agent can handle conflicts
	}

	// Sync beads
	bdCmd := exec.Command("bd", "sync")
	bdCmd.Dir = workDir
	if err := bdCmd.Run(); err != nil {
		d.logger.Printf("Warning: bd sync failed in %s: %v", workDir, err)
	}
}

// closeMessage marks a mail message as read by closing the beads issue.
func (d *Daemon) closeMessage(id string) error {
	cmd := exec.Command("bd", "close", id)
	cmd.Dir = d.config.TownRoot
	return cmd.Run()
}

// verifyAgentRequestingState verifies that the agent has set requesting_<action>=true
// in its state.json before we kill its session. This ensures the agent is actually
// ready to be killed and has completed its pre-shutdown tasks (git clean, handoff mail, etc).
func (d *Daemon) verifyAgentRequestingState(identity string, action LifecycleAction) error {
	stateFile := d.identityToStateFile(identity)
	if stateFile == "" {
		// If we can't determine state file, log warning but allow action
		// This maintains backwards compatibility with agents that don't support state files yet
		d.logger.Printf("Warning: cannot determine state file for %s, skipping verification", identity)
		return nil
	}

	data, err := os.ReadFile(stateFile)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("agent state file not found: %s (agent must set requesting_%s=true before lifecycle request)", stateFile, action)
		}
		return fmt.Errorf("reading agent state: %w", err)
	}

	var state map[string]interface{}
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("parsing agent state: %w", err)
	}

	// Check for requesting_<action>=true
	key := "requesting_" + string(action)
	val, ok := state[key]
	if !ok {
		return fmt.Errorf("agent state missing %s field (agent must set this before lifecycle request)", key)
	}

	requesting, ok := val.(bool)
	if !ok || !requesting {
		return fmt.Errorf("agent state %s is not true (got: %v)", key, val)
	}

	d.logger.Printf("Verified agent %s has %s=true", identity, key)
	return nil
}

// identityToStateFile maps an agent identity to its state.json file path.
func (d *Daemon) identityToStateFile(identity string) string {
	switch identity {
	case "mayor":
		return filepath.Join(d.config.TownRoot, "mayor", "state.json")
	default:
		// Pattern: <rig>-witness → <townRoot>/<rig>/witness/state.json
		if strings.HasSuffix(identity, "-witness") {
			rigName := strings.TrimSuffix(identity, "-witness")
			return filepath.Join(d.config.TownRoot, rigName, "witness", "state.json")
		}
		// Pattern: <rig>-refinery → <townRoot>/<rig>/refinery/state.json
		if strings.HasSuffix(identity, "-refinery") {
			rigName := strings.TrimSuffix(identity, "-refinery")
			return filepath.Join(d.config.TownRoot, rigName, "refinery", "state.json")
		}
		// Pattern: <rig>-crew-<name> → <townRoot>/<rig>/crew/<name>/state.json
		if strings.Contains(identity, "-crew-") {
			parts := strings.SplitN(identity, "-crew-", 2)
			if len(parts) == 2 {
				rigName := parts[0]
				crewName := parts[1]
				return filepath.Join(d.config.TownRoot, rigName, "crew", crewName, "state.json")
			}
		}
		// Unknown identity - can't determine state file
		return ""
	}
}
