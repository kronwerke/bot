package update

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The guard script restores the previous binary after three failed starts and
// remembers the bad tag, so the updater does not install it again.
func TestRollbackGuardScript(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "bin"), 0o755)
	os.MkdirAll(filepath.Join(dir, "state"), 0o755)
	bin := filepath.Join(dir, "bin", "kronwerke-bot")
	os.WriteFile(bin, []byte("new"), 0o755)
	os.WriteFile(bin+".prev", []byte("old"), 0o755)
	trial := filepath.Join(dir, "state", "update.trial")
	os.WriteFile(trial, []byte("0 v0.2.0\n"), 0o640)

	guard, _ := filepath.Abs("../../deploy/rollback-guard.sh")
	run := func() {
		cmd := exec.Command("sh", guard)
		cmd.Env = append(os.Environ(), "KW_DIR="+dir)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("guard: %v %s", err, out)
		}
	}
	for i := 1; i <= 3; i++ {
		run()
		b, _ := os.ReadFile(bin)
		if string(b) != "new" {
			t.Fatalf("rolled back too early, after start %d", i)
		}
	}
	run()
	b, _ := os.ReadFile(bin)
	if string(b) != "old" {
		t.Fatal("previous binary not restored after the fourth start")
	}
	skip, _ := os.ReadFile(filepath.Join(dir, "state", "update.skip"))
	if strings.TrimSpace(string(skip)) != "v0.2.0" {
		t.Fatalf("skip file %q", skip)
	}
	if _, err := os.Stat(trial); err == nil {
		t.Fatal("trial file left behind")
	}
	run() // no trial file: nothing happens
}

func TestGuardDoesNothingWithoutTrial(t *testing.T) {
	dir := t.TempDir()
	guard, _ := filepath.Abs("../../deploy/rollback-guard.sh")
	cmd := exec.Command("sh", guard)
	cmd.Env = append(os.Environ(), "KW_DIR="+dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
}
