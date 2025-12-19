package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/tmux"
	"github.com/steveyegge/gastown/internal/workspace"
)

// polecatNames are Mad Max: Fury Road themed names for auto-generated polecats.
var polecatNames = []string{
	"Nux", "Toast", "Capable", "Cheedo", "Dag", "Rictus", "Slit", "Morsov",
	"Ace", "Coma", "Valkyrie", "Keeper", "Vuvalini", "Organic", "Immortan",
	"Corpus", "Doof", "Scabrous", "Splendid", "Fragile",
}

// Spawn command flags
var (
	spawnIssue   string
	spawnMessage string
	spawnCreate  bool
	spawnNoStart bool
)

var spawnCmd = &cobra.Command{
	Use:     "spawn <rig/polecat> | <rig>",
	Aliases: []string{"sp"},
	Short:   "Spawn a polecat with work assignment",
	Long: `Spawn a polecat with a work assignment.

Assigns an issue or task to a polecat and starts a session. If no polecat
is specified, auto-selects an idle polecat in the rig.

Examples:
  gt spawn gastown/Toast --issue gt-abc
  gt spawn gastown --issue gt-def          # auto-select polecat
  gt spawn gastown/Nux -m "Fix the tests"  # free-form task
  gt spawn gastown/Capable --issue gt-xyz --create  # create if missing`,
	Args: cobra.ExactArgs(1),
	RunE: runSpawn,
}

func init() {
	spawnCmd.Flags().StringVar(&spawnIssue, "issue", "", "Beads issue ID to assign")
	spawnCmd.Flags().StringVarP(&spawnMessage, "message", "m", "", "Free-form task description")
	spawnCmd.Flags().BoolVar(&spawnCreate, "create", false, "Create polecat if it doesn't exist")
	spawnCmd.Flags().BoolVar(&spawnNoStart, "no-start", false, "Assign work but don't start session")

	rootCmd.AddCommand(spawnCmd)
}

// BeadsIssue represents a beads issue from JSON output.
type BeadsIssue struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Priority    int    `json:"priority"`
	Type        string `json:"issue_type"`
	Status      string `json:"status"`
}

func runSpawn(cmd *cobra.Command, args []string) error {
	if spawnIssue == "" && spawnMessage == "" {
		return fmt.Errorf("must specify --issue or -m/--message")
	}

	// Parse address: rig/polecat or just rig
	rigName, polecatName, err := parseSpawnAddress(args[0])
	if err != nil {
		return err
	}

	// Find workspace and rig
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	rigsConfigPath := filepath.Join(townRoot, "mayor", "rigs.json")
	rigsConfig, err := config.LoadRigsConfig(rigsConfigPath)
	if err != nil {
		rigsConfig = &config.RigsConfig{Rigs: make(map[string]config.RigEntry)}
	}

	g := git.NewGit(townRoot)
	rigMgr := rig.NewManager(townRoot, rigsConfig, g)
	r, err := rigMgr.GetRig(rigName)
	if err != nil {
		return fmt.Errorf("rig '%s' not found", rigName)
	}

	// Get polecat manager
	polecatGit := git.NewGit(r.Path)
	polecatMgr := polecat.NewManager(r, polecatGit)

	// Auto-select polecat if not specified
	if polecatName == "" {
		polecatName, err = selectIdlePolecat(polecatMgr, r)
		if err != nil {
			// If --create is set, generate a new polecat name instead of failing
			if spawnCreate {
				polecatName = generatePolecatName(polecatMgr)
				fmt.Printf("Generated polecat name: %s\n", polecatName)
			} else {
				return fmt.Errorf("auto-select polecat: %w", err)
			}
		} else {
			fmt.Printf("Auto-selected polecat: %s\n", polecatName)
		}
	}

	// Check/create polecat
	pc, err := polecatMgr.Get(polecatName)
	if err != nil {
		if err == polecat.ErrPolecatNotFound {
			if !spawnCreate {
				return fmt.Errorf("polecat '%s' not found (use --create to create)", polecatName)
			}
			fmt.Printf("Creating polecat %s...\n", polecatName)
			pc, err = polecatMgr.Add(polecatName)
			if err != nil {
				return fmt.Errorf("creating polecat: %w", err)
			}
		} else {
			return fmt.Errorf("getting polecat: %w", err)
		}
	}

	// Check polecat state
	if pc.State == polecat.StateWorking {
		return fmt.Errorf("polecat '%s' is already working on %s", polecatName, pc.Issue)
	}

	// Get issue details if specified
	var issue *BeadsIssue
	if spawnIssue != "" {
		issue, err = fetchBeadsIssue(r.Path, spawnIssue)
		if err != nil {
			return fmt.Errorf("fetching issue %s: %w", spawnIssue, err)
		}
	}

	// Assign issue/task to polecat
	assignmentID := spawnIssue
	if assignmentID == "" {
		assignmentID = "task:" + time.Now().Format("20060102-150405")
	}
	if err := polecatMgr.AssignIssue(polecatName, assignmentID); err != nil {
		return fmt.Errorf("assigning issue: %w", err)
	}

	fmt.Printf("%s Assigned %s to %s/%s\n",
		style.Bold.Render("✓"),
		assignmentID, rigName, polecatName)

	// Stop here if --no-start
	if spawnNoStart {
		fmt.Printf("\n  %s\n", style.Dim.Render("Use 'gt session start' to start the session"))
		return nil
	}

	// Start session
	t := tmux.NewTmux()
	sessMgr := session.NewManager(t, r)

	// Check if already running
	running, _ := sessMgr.IsRunning(polecatName)
	if running {
		// Just inject the context
		fmt.Printf("Session already running, injecting context...\n")
	} else {
		// Start new session
		fmt.Printf("Starting session for %s/%s...\n", rigName, polecatName)
		if err := sessMgr.Start(polecatName, session.StartOptions{}); err != nil {
			return fmt.Errorf("starting session: %w", err)
		}
		// Wait for Claude to fully initialize (needs 4-5s for prompt)
		fmt.Printf("Waiting for Claude to initialize...\n")
		time.Sleep(5 * time.Second)
	}

	// Inject initial context
	context := buildSpawnContext(issue, spawnMessage)
	fmt.Printf("Injecting work assignment...\n")
	if err := sessMgr.Inject(polecatName, context); err != nil {
		return fmt.Errorf("injecting context: %w", err)
	}

	fmt.Printf("%s Session started. Attach with: %s\n",
		style.Bold.Render("✓"),
		style.Dim.Render(fmt.Sprintf("gt session at %s/%s", rigName, polecatName)))

	return nil
}

// parseSpawnAddress parses "rig/polecat" or "rig".
func parseSpawnAddress(addr string) (rigName, polecatName string, err error) {
	if strings.Contains(addr, "/") {
		parts := strings.SplitN(addr, "/", 2)
		if parts[0] == "" {
			return "", "", fmt.Errorf("invalid address: missing rig name")
		}
		return parts[0], parts[1], nil
	}
	return addr, "", nil
}

// generatePolecatName generates a unique polecat name that doesn't conflict with existing ones.
func generatePolecatName(mgr *polecat.Manager) string {
	existing, _ := mgr.List()
	existingNames := make(map[string]bool)
	for _, p := range existing {
		existingNames[p.Name] = true
	}

	// Try to find an unused name from the list
	// Shuffle to avoid always picking the same name
	shuffled := make([]string, len(polecatNames))
	copy(shuffled, polecatNames)
	rand.Shuffle(len(shuffled), func(i, j int) {
		shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
	})

	for _, name := range shuffled {
		if !existingNames[name] {
			return name
		}
	}

	// All names taken, generate one with a number suffix
	base := shuffled[0]
	for i := 2; ; i++ {
		name := fmt.Sprintf("%s%d", base, i)
		if !existingNames[name] {
			return name
		}
	}
}

// selectIdlePolecat finds an idle polecat in the rig.
func selectIdlePolecat(mgr *polecat.Manager, r *rig.Rig) (string, error) {
	polecats, err := mgr.List()
	if err != nil {
		return "", err
	}

	// Prefer idle polecats
	for _, pc := range polecats {
		if pc.State == polecat.StateIdle {
			return pc.Name, nil
		}
	}

	// Accept active polecats without current work
	for _, pc := range polecats {
		if pc.State == polecat.StateActive && pc.Issue == "" {
			return pc.Name, nil
		}
	}

	// Check rig's polecat list for any we haven't loaded yet
	for _, name := range r.Polecats {
		found := false
		for _, pc := range polecats {
			if pc.Name == name {
				found = true
				break
			}
		}
		if !found {
			return name, nil
		}
	}

	return "", fmt.Errorf("no available polecats in rig '%s'", r.Name)
}

// fetchBeadsIssue gets issue details from beads CLI.
func fetchBeadsIssue(rigPath, issueID string) (*BeadsIssue, error) {
	cmd := exec.Command("bd", "show", issueID, "--json")
	cmd.Dir = rigPath

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		errMsg := strings.TrimSpace(stderr.String())
		if errMsg != "" {
			return nil, fmt.Errorf("%s", errMsg)
		}
		return nil, err
	}

	// bd show --json returns an array, take the first element
	var issues []BeadsIssue
	if err := json.Unmarshal(stdout.Bytes(), &issues); err != nil {
		return nil, fmt.Errorf("parsing issue: %w", err)
	}
	if len(issues) == 0 {
		return nil, fmt.Errorf("issue not found: %s", issueID)
	}

	return &issues[0], nil
}

// buildSpawnContext creates the initial context message for the polecat.
func buildSpawnContext(issue *BeadsIssue, message string) string {
	var sb strings.Builder

	sb.WriteString("[SPAWN] You have been assigned work.\n\n")

	if issue != nil {
		sb.WriteString(fmt.Sprintf("Issue: %s\n", issue.ID))
		sb.WriteString(fmt.Sprintf("Title: %s\n", issue.Title))
		sb.WriteString(fmt.Sprintf("Priority: P%d\n", issue.Priority))
		sb.WriteString(fmt.Sprintf("Type: %s\n", issue.Type))
		if issue.Description != "" {
			sb.WriteString(fmt.Sprintf("\nDescription:\n%s\n", issue.Description))
		}
	} else if message != "" {
		sb.WriteString(fmt.Sprintf("Task: %s\n", message))
	}

	sb.WriteString("\nWork on this task. When complete, commit your changes and signal DONE.\n")

	return sb.String()
}
