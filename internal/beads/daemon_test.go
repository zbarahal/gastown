package beads

import (
	"os/exec"
	"testing"
)

func TestParseBdDaemonCount_Array(t *testing.T) {
	input := []byte(`[{"pid":1234},{"pid":5678}]`)
	count := parseBdDaemonCount(input)
	if count != 2 {
		t.Errorf("expected 2, got %d", count)
	}
}

func TestParseBdDaemonCount_ObjectWithCount(t *testing.T) {
	input := []byte(`{"count":3,"daemons":[{},{},{}]}`)
	count := parseBdDaemonCount(input)
	if count != 3 {
		t.Errorf("expected 3, got %d", count)
	}
}

func TestParseBdDaemonCount_ObjectWithDaemons(t *testing.T) {
	input := []byte(`{"daemons":[{},{}]}`)
	count := parseBdDaemonCount(input)
	if count != 2 {
		t.Errorf("expected 2, got %d", count)
	}
}

func TestParseBdDaemonCount_Empty(t *testing.T) {
	input := []byte(``)
	count := parseBdDaemonCount(input)
	if count != 0 {
		t.Errorf("expected 0, got %d", count)
	}
}

func TestParseBdDaemonCount_Invalid(t *testing.T) {
	input := []byte(`not json`)
	count := parseBdDaemonCount(input)
	if count != 0 {
		t.Errorf("expected 0 for invalid JSON, got %d", count)
	}
}

func TestCountBdActivityProcesses(t *testing.T) {
	count := CountBdActivityProcesses()
	if count < 0 {
		t.Errorf("count should be non-negative, got %d", count)
	}
}

func TestCountBdDaemons(t *testing.T) {
	if _, err := exec.LookPath("bd"); err != nil {
		t.Skip("bd not installed")
	}
	count := CountBdDaemons()
	if count < 0 {
		t.Errorf("count should be non-negative, got %d", count)
	}
}

func TestStopAllBdProcesses_DryRun(t *testing.T) {
	daemonsKilled, activityKilled, err := StopAllBdProcesses(true, false)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if daemonsKilled < 0 || activityKilled < 0 {
		t.Errorf("counts should be non-negative: daemons=%d, activity=%d", daemonsKilled, activityKilled)
	}
}

// Tests for configurable socket locations (hq-q9n)

func TestApplySocketConfig_NilConfig(t *testing.T) {
	cmd := exec.Command("echo", "test")
	applySocketConfig(cmd, nil)

	// Should not panic and env should be nil (inherit from parent)
	if cmd.Env != nil {
		t.Errorf("expected nil env for nil config, got %v", cmd.Env)
	}
}

func TestApplySocketConfig_Socket(t *testing.T) {
	cmd := exec.Command("echo", "test")
	cfg := &SocketConfig{
		Socket: "/custom/path/bd.sock",
	}
	applySocketConfig(cmd, cfg)

	// Should have BD_SOCKET in env
	found := false
	for _, env := range cmd.Env {
		if env == "BD_SOCKET=/custom/path/bd.sock" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("BD_SOCKET not found in env: %v", cmd.Env)
	}
}

func TestApplySocketConfig_SocketDir(t *testing.T) {
	cmd := exec.Command("echo", "test")
	cfg := &SocketConfig{
		SocketDir: "/var/run/beads",
	}
	applySocketConfig(cmd, cfg)

	// Should have BD_SOCKET_DIR in env
	found := false
	for _, env := range cmd.Env {
		if env == "BD_SOCKET_DIR=/var/run/beads" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("BD_SOCKET_DIR not found in env: %v", cmd.Env)
	}
}

func TestApplySocketConfig_Both(t *testing.T) {
	cmd := exec.Command("echo", "test")
	cfg := &SocketConfig{
		Socket:    "/custom/bd.sock",
		SocketDir: "/var/run/beads",
	}
	applySocketConfig(cmd, cfg)

	// Should have both env vars
	foundSocket := false
	foundSocketDir := false
	for _, env := range cmd.Env {
		if env == "BD_SOCKET=/custom/bd.sock" {
			foundSocket = true
		}
		if env == "BD_SOCKET_DIR=/var/run/beads" {
			foundSocketDir = true
		}
	}
	if !foundSocket {
		t.Errorf("BD_SOCKET not found in env")
	}
	if !foundSocketDir {
		t.Errorf("BD_SOCKET_DIR not found in env")
	}
}

func TestApplySocketConfig_EmptyStrings(t *testing.T) {
	cmd := exec.Command("echo", "test")
	cfg := &SocketConfig{
		Socket:    "",
		SocketDir: "",
	}
	applySocketConfig(cmd, cfg)

	// Empty strings should not add env vars
	for _, env := range cmd.Env {
		if env == "BD_SOCKET=" || env == "BD_SOCKET_DIR=" {
			t.Errorf("empty config values should not add empty env vars: %v", cmd.Env)
		}
	}
}
