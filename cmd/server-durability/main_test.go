// Copyright 2026 Yauhen Bichel
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// capture runs the command line and returns its exit status and what it printed.
func capture(t *testing.T, args ...string) (int, string) {
	t.Helper()
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	code := run(args)
	w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	return code, string(out)
}

// The whole path with a real restic: stage, back up, ask the snapshot, rehearse the restore, audit.
func TestStageBackupCoversDrillAudit(t *testing.T) {
	restic, err := exec.LookPath("restic")
	if err != nil {
		t.Skip("restic is not installed")
	}
	// every path of the declaration goes through a symbolic link, as a home directory on another disk does:
	// the backup is given the link's path, and the snapshot holds it that way
	real, _ := filepath.EvalSymlinks(t.TempDir())
	dir := filepath.Join(real, "link")
	os.MkdirAll(filepath.Join(real, "target"), 0o755)
	if err := os.Symlink(filepath.Join(real, "target"), dir); err != nil {
		t.Fatal(err)
	}
	live := filepath.Join(dir, "data", "app.db")
	uploads := filepath.Join(dir, "data", "uploads")
	os.MkdirAll(uploads, 0o755)
	os.WriteFile(filepath.Join(uploads, "a.txt"), []byte("a"), 0o644)
	db, err := sql.Open("sqlite", live)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	for _, q := range []string{"PRAGMA journal_mode=WAL", "PRAGMA wal_autocheckpoint=0", "CREATE TABLE t(x)"} {
		db.Exec(q)
	}
	tx, _ := db.Begin()
	for i := 0; i < 2000; i++ {
		tx.Exec("INSERT INTO t VALUES(?)", i)
	}
	tx.Commit() // the service stays connected: every row is in the log, none in the main file
	defer db.Close()

	repo, pass, staged := filepath.Join(dir, "repo"), filepath.Join(dir, "password"), filepath.Join(dir, "staged")
	os.WriteFile(pass, []byte("for-this-test-only\n"), 0o600)
	cfg := filepath.Join(dir, "config.toml")
	os.WriteFile(cfg, []byte(fmt.Sprintf(`
state_dir = %q
[[store]]
name = "app"
path = %q
kind = "sqlite"
[[store]]
name = "uploads"
path = %q
kind = "directory"
[stage]
dir = %q
[backup]
tool = "restic"
repository = %q
password_file = %q
[[copy]]
name = "elsewhere"
where = "off-site"
`, filepath.Join(dir, "state"), live, uploads, staged, repo, pass)), 0o644)
	rs := func(args ...string) {
		t.Helper()
		if out, err := exec.Command(restic, append([]string{"--repo", repo, "--password-file", pass}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("restic %v: %v\n%s", args, err, out)
		}
	}
	rs("init")

	// before anything is backed up: the audit says so, and exits 1
	code, out := capture(t, "-config", cfg, "audit")
	if code != 1 || !strings.Contains(out, "the repository holds no snapshot") || !strings.Contains(out, "no staged copy yet") {
		t.Fatalf("audit of an empty repository: exit %d\n%s", code, out)
	}

	// a backup that takes only the uploads: `covers` names the database as missing
	rs("backup", uploads)
	code, out = capture(t, "-config", cfg, "covers")
	if code != 1 || !strings.Contains(out, "FAIL  app") || !strings.Contains(out, "not in the newest snapshot") || !strings.Contains(out, "ok    uploads") {
		t.Fatalf("covers with the database left out: exit %d\n%s", code, out)
	}

	// stage, then back up the staged copy and the uploads
	code, out = capture(t, "-config", cfg, "stage")
	if code != 0 || !strings.Contains(out, "ok    app") {
		t.Fatalf("stage: exit %d\n%s", code, out)
	}
	rs("backup", staged, uploads)
	if code, out = capture(t, "-config", cfg, "covers"); code != 0 {
		t.Fatalf("covers after a full backup: exit %d\n%s", code, out)
	}

	// the rehearsal restores, opens, counts: all 2000 committed rows, though the live main file holds none
	code, out = capture(t, "-config", cfg, "-json", "drill")
	var result struct {
		OK     bool
		Stores []struct {
			Name     string
			OK       bool
			Restored map[string]int64 `json:"restored_rows"`
		}
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil || code != 0 || !result.OK || len(result.Stores) != 2 || result.Stores[0].Restored["t"] != 2000 {
		t.Fatalf("drill: exit %d, %v\n%s", code, err, out)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "state", "drill-*")); len(left) != 0 {
		t.Fatalf("the scratch directory stayed: %v", left)
	}

	// now the audit has nothing that would lose data
	code, out = capture(t, "-config", cfg, "audit", "-json")
	var report struct {
		Worst    string
		Findings []struct{ Level, Subject, Message string }
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil || code != 0 || report.Worst == "fail" {
		t.Fatalf("audit after everything: exit %d, %v\n%s", code, err, out)
	}
	said := ""
	for _, f := range report.Findings {
		said += f.Level + " " + f.Subject + ": " + f.Message + "\n"
	}
	for _, want := range []string{"ok app: in the newest snapshot", "ok restore: a restore was rehearsed", "ok backup: the newest snapshot"} {
		if !strings.Contains(said, want) {
			t.Errorf("the audit does not say %q:\n%s", want, said)
		}
	}

	// a snapshot whose staged copy is damaged: the rehearsal fails, and the next audit says the restore failed
	os.WriteFile(filepath.Join(staged, "app.db"), []byte(strings.Repeat("damaged ", 2000)), 0o644)
	rs("backup", staged, uploads)
	if code, out = capture(t, "-config", cfg, "drill"); code != 1 || !strings.Contains(out, "FAIL  app") {
		t.Fatalf("drill on a damaged copy: exit %d\n%s", code, out)
	}
	if code, out = capture(t, "-config", cfg, "audit"); code != 1 || !strings.Contains(out, "the last rehearsed restore") || !strings.Contains(out, "failed") {
		t.Fatalf("audit after a failed drill: exit %d\n%s", code, out)
	}
}

func TestCommandLine(t *testing.T) {
	if code, out := capture(t, "init"); code != 0 || !strings.Contains(out, "[[store]]") {
		t.Fatalf("init: %d", code)
	}
	if code, out := capture(t, "version"); code != 0 || !strings.HasPrefix(out, "server-durability ") {
		t.Fatalf("version: %d %q", code, out)
	}
	for _, args := range [][]string{{}, {"dance"}, {"audit", "extra"}, {"-nope", "audit"}} {
		if code, _ := capture(t, args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
	if code, _ := capture(t, "-config", filepath.Join(t.TempDir(), "none.toml"), "audit"); code != 3 {
		t.Errorf("a missing declaration: exit %d, want 3", code)
	}
}
