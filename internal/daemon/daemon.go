package daemon

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/keepalive"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/tmux"
)

// Daemon is the town-level background service.
type Daemon struct {
	config  *Config
	tmux    *tmux.Tmux
	logger  *log.Logger
	ctx     context.Context
	cancel  context.CancelFunc
	backoff *BackoffManager
}

// New creates a new daemon instance.
func New(config *Config) (*Daemon, error) {
	// Ensure daemon directory exists
	daemonDir := filepath.Dir(config.LogFile)
	if err := os.MkdirAll(daemonDir, 0755); err != nil {
		return nil, fmt.Errorf("creating daemon directory: %w", err)
	}

	// Open log file
	logFile, err := os.OpenFile(config.LogFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return nil, fmt.Errorf("opening log file: %w", err)
	}

	logger := log.New(logFile, "", log.LstdFlags)
	ctx, cancel := context.WithCancel(context.Background())

	return &Daemon{
		config:  config,
		tmux:    tmux.NewTmux(),
		logger:  logger,
		ctx:     ctx,
		cancel:  cancel,
		backoff: NewBackoffManager(DefaultBackoffConfig()),
	}, nil
}

// Run starts the daemon main loop.
func (d *Daemon) Run() error {
	d.logger.Printf("Daemon starting (PID %d)", os.Getpid())

	// Write PID file
	if err := os.WriteFile(d.config.PidFile, []byte(strconv.Itoa(os.Getpid())), 0644); err != nil {
		return fmt.Errorf("writing PID file: %w", err)
	}
	defer func() { _ = os.Remove(d.config.PidFile) }()

	// Update state
	state := &State{
		Running:   true,
		PID:       os.Getpid(),
		StartedAt: time.Now(),
	}
	if err := SaveState(d.config.TownRoot, state); err != nil {
		d.logger.Printf("Warning: failed to save state: %v", err)
	}

	// Handle signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Heartbeat ticker
	ticker := time.NewTicker(d.config.HeartbeatInterval)
	defer ticker.Stop()

	d.logger.Printf("Daemon running, heartbeat every %v", d.config.HeartbeatInterval)

	// Initial heartbeat
	d.heartbeat(state)

	for {
		select {
		case <-d.ctx.Done():
			d.logger.Println("Daemon context cancelled, shutting down")
			return d.shutdown(state)

		case sig := <-sigChan:
			d.logger.Printf("Received signal %v, shutting down", sig)
			return d.shutdown(state)

		case <-ticker.C:
			d.heartbeat(state)
		}
	}
}

// heartbeat performs one heartbeat cycle.
func (d *Daemon) heartbeat(state *State) {
	d.logger.Println("Heartbeat starting")

	// 1. Poke Mayor
	d.pokeMayor()

	// 2. Poke Witnesses (for each rig)
	d.pokeWitnesses()

	// 3. Process lifecycle requests
	d.processLifecycleRequests()

	// Update state
	state.LastHeartbeat = time.Now()
	state.HeartbeatCount++
	if err := SaveState(d.config.TownRoot, state); err != nil {
		d.logger.Printf("Warning: failed to save state: %v", err)
	}

	d.logger.Printf("Heartbeat complete (#%d)", state.HeartbeatCount)
}

// pokeMayor sends a heartbeat to the Mayor session.
func (d *Daemon) pokeMayor() {
	const mayorSession = "gt-mayor"
	const agentID = "mayor"

	running, err := d.tmux.HasSession(mayorSession)
	if err != nil {
		d.logger.Printf("Error checking Mayor session: %v", err)
		return
	}

	if !running {
		d.logger.Println("Mayor session not running, skipping poke")
		return
	}

	// Check keepalive to see if agent is active
	state := keepalive.Read(d.config.TownRoot)
	if state != nil && state.IsFresh() {
		// Agent is actively working, reset backoff
		d.backoff.RecordActivity(agentID)
		d.logger.Printf("Mayor is fresh (cmd: %s), skipping poke", state.LastCommand)
		return
	}

	// Check if we should poke based on backoff interval
	if !d.backoff.ShouldPoke(agentID) {
		interval := d.backoff.GetInterval(agentID)
		d.logger.Printf("Mayor backoff in effect (interval: %v), skipping poke", interval)
		return
	}

	// Send heartbeat message via tmux
	msg := "HEARTBEAT: check your rigs"
	if err := d.tmux.SendKeys(mayorSession, msg); err != nil {
		d.logger.Printf("Error poking Mayor: %v", err)
		return
	}

	d.backoff.RecordPoke(agentID)

	// If agent is stale or very stale, record a miss (increase backoff)
	if state == nil || state.IsVeryStale() {
		d.backoff.RecordMiss(agentID)
		interval := d.backoff.GetInterval(agentID)
		d.logger.Printf("Poked Mayor (very stale, backoff now: %v)", interval)
	} else if state.IsStale() {
		// Stale but not very stale - don't increase backoff, but don't reset either
		d.logger.Println("Poked Mayor (stale)")
	} else {
		d.logger.Println("Poked Mayor")
	}
}

// pokeWitnesses sends heartbeats to all Witness sessions.
// Uses proper rig discovery from rigs.json instead of scanning tmux sessions.
func (d *Daemon) pokeWitnesses() {
	// Discover rigs from configuration
	rigs := d.discoverRigs()
	if len(rigs) == 0 {
		d.logger.Println("No rigs discovered")
		return
	}

	for _, r := range rigs {
		session := fmt.Sprintf("gt-%s-witness", r.Name)

		// Check if witness session exists
		running, err := d.tmux.HasSession(session)
		if err != nil {
			d.logger.Printf("Error checking witness session for rig %s: %v", r.Name, err)
			continue
		}

		if !running {
			// Rig exists but no witness session - log for visibility
			d.logger.Printf("Rig %s has no witness session (may need: gt witness start %s)", r.Name, r.Name)
			continue
		}

		d.pokeWitness(session)
	}
}

// discoverRigs finds all registered rigs using the rig manager.
// Falls back to directory scanning if rigs.json is not available.
func (d *Daemon) discoverRigs() []*rig.Rig {
	// Load rigs config from mayor/rigs.json
	rigsConfigPath := filepath.Join(d.config.TownRoot, "mayor", "rigs.json")
	rigsConfig, err := config.LoadRigsConfig(rigsConfigPath)
	if err != nil {
		// Try fallback: scan town directory for rig directories
		return d.discoverRigsFromDirectory()
	}

	// Use rig manager for proper discovery
	g := git.NewGit(d.config.TownRoot)
	mgr := rig.NewManager(d.config.TownRoot, rigsConfig, g)
	rigs, err := mgr.DiscoverRigs()
	if err != nil {
		d.logger.Printf("Error discovering rigs from config: %v", err)
		return d.discoverRigsFromDirectory()
	}

	return rigs
}

// discoverRigsFromDirectory scans the town directory for rig directories.
// A directory is considered a rig if it has a .beads subdirectory or config.json.
func (d *Daemon) discoverRigsFromDirectory() []*rig.Rig {
	entries, err := os.ReadDir(d.config.TownRoot)
	if err != nil {
		d.logger.Printf("Error reading town directory: %v", err)
		return nil
	}

	var rigs []*rig.Rig
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		name := entry.Name()
		// Skip known non-rig directories
		if name == "mayor" || name == "daemon" || name == ".git" || name[0] == '.' {
			continue
		}

		dirPath := filepath.Join(d.config.TownRoot, name)

		// Check for .beads directory (indicates a rig)
		beadsPath := filepath.Join(dirPath, ".beads")
		if _, err := os.Stat(beadsPath); err == nil {
			rigs = append(rigs, &rig.Rig{Name: name, Path: dirPath})
			continue
		}

		// Check for config.json with type: rig
		configPath := filepath.Join(dirPath, "config.json")
		if _, err := os.Stat(configPath); err == nil {
			// For simplicity, assume any directory with config.json is a rig
			rigs = append(rigs, &rig.Rig{Name: name, Path: dirPath})
		}
	}

	return rigs
}

// pokeWitness sends a heartbeat to a single witness session with backoff.
func (d *Daemon) pokeWitness(session string) {
	// Extract rig name from session (gt-<rig>-witness -> <rig>)
	rigName := extractRigName(session)
	agentID := session // Use session name as agent ID

	// Find the rig's workspace for keepalive check
	rigWorkspace := filepath.Join(d.config.TownRoot, "gastown", rigName)

	// Check keepalive to see if the witness is active
	state := keepalive.Read(rigWorkspace)
	if state != nil && state.IsFresh() {
		// Witness is actively working, reset backoff
		d.backoff.RecordActivity(agentID)
		d.logger.Printf("Witness %s is fresh (cmd: %s), skipping poke", session, state.LastCommand)
		return
	}

	// Check if we should poke based on backoff interval
	if !d.backoff.ShouldPoke(agentID) {
		interval := d.backoff.GetInterval(agentID)
		d.logger.Printf("Witness %s backoff in effect (interval: %v), skipping poke", session, interval)
		return
	}

	// Send heartbeat message
	msg := "HEARTBEAT: check your workers"
	if err := d.tmux.SendKeys(session, msg); err != nil {
		d.logger.Printf("Error poking Witness %s: %v", session, err)
		return
	}

	d.backoff.RecordPoke(agentID)

	// If agent is stale or very stale, record a miss (increase backoff)
	if state == nil || state.IsVeryStale() {
		d.backoff.RecordMiss(agentID)
		interval := d.backoff.GetInterval(agentID)
		d.logger.Printf("Poked Witness %s (very stale, backoff now: %v)", session, interval)
	} else if state.IsStale() {
		d.logger.Printf("Poked Witness %s (stale)", session)
	} else {
		d.logger.Printf("Poked Witness %s", session)
	}
}

// extractRigName extracts the rig name from a witness session name.
// "gt-gastown-witness" -> "gastown"
func extractRigName(session string) string {
	// Remove "gt-" prefix and "-witness" suffix
	name := strings.TrimPrefix(session, "gt-")
	name = strings.TrimSuffix(name, "-witness")
	return name
}

// isWitnessSession checks if a session name is a witness session.
func isWitnessSession(name string) bool {
	// Pattern: gt-<rig>-witness
	if len(name) < 12 { // "gt-x-witness" minimum
		return false
	}
	return name[:3] == "gt-" && name[len(name)-8:] == "-witness"
}

// processLifecycleRequests checks for and processes lifecycle requests.
func (d *Daemon) processLifecycleRequests() {
	d.ProcessLifecycleRequests()
}

// shutdown performs graceful shutdown.
func (d *Daemon) shutdown(state *State) error {
	d.logger.Println("Daemon shutting down")

	state.Running = false
	if err := SaveState(d.config.TownRoot, state); err != nil {
		d.logger.Printf("Warning: failed to save final state: %v", err)
	}

	d.logger.Println("Daemon stopped")
	return nil
}

// Stop signals the daemon to stop.
func (d *Daemon) Stop() {
	d.cancel()
}

// IsRunning checks if a daemon is running for the given town.
func IsRunning(townRoot string) (bool, int, error) {
	pidFile := filepath.Join(townRoot, "daemon", "daemon.pid")
	data, err := os.ReadFile(pidFile)
	if err != nil {
		if os.IsNotExist(err) {
			return false, 0, nil
		}
		return false, 0, err
	}

	pid, err := strconv.Atoi(string(data))
	if err != nil {
		return false, 0, nil
	}

	// Check if process is running
	process, err := os.FindProcess(pid)
	if err != nil {
		return false, 0, nil
	}

	// On Unix, FindProcess always succeeds. Send signal 0 to check if alive.
	err = process.Signal(syscall.Signal(0))
	if err != nil {
		// Process not running, clean up stale PID file
		_ = os.Remove(pidFile)
		return false, 0, nil
	}

	return true, pid, nil
}

// StopDaemon stops the running daemon for the given town.
func StopDaemon(townRoot string) error {
	running, pid, err := IsRunning(townRoot)
	if err != nil {
		return err
	}
	if !running {
		return fmt.Errorf("daemon is not running")
	}

	process, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("finding process: %w", err)
	}

	// Send SIGTERM for graceful shutdown
	if err := process.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("sending SIGTERM: %w", err)
	}

	// Wait a bit for graceful shutdown
	time.Sleep(500 * time.Millisecond)

	// Check if still running
	if err := process.Signal(syscall.Signal(0)); err == nil {
		// Still running, force kill
		_ = process.Signal(syscall.SIGKILL)
	}

	// Clean up PID file
	pidFile := filepath.Join(townRoot, "daemon", "daemon.pid")
	_ = os.Remove(pidFile)

	return nil
}
