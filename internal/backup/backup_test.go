// Copyright 2026 Yauhen Bichel
// SPDX-License-Identifier: Apache-2.0

package backup

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/YauhenBichel/server-durability/internal/config"
)

// A real restic against a repository made for the test. Skipped where restic is not installed.
func TestResticLatestPathsAndRestore(t *testing.T) {
	bin, err := exec.LookPath("restic")
	if err != nil {
		t.Skip("restic is not installed")
	}
	dir := t.TempDir()
	repo, pass, data := filepath.Join(dir, "repo"), filepath.Join(dir, "password"), filepath.Join(dir, "data")
	os.WriteFile(pass, []byte("for-this-test-only\n"), 0o600)
	os.MkdirAll(filepath.Join(data, "sub"), 0o755)
	os.WriteFile(filepath.Join(data, "sub", "kept.txt"), []byte("kept"), 0o644)
	tool := New(config.Backup{Tool: "restic", Repository: repo, PasswordFile: pass}).(*Restic)
	ctx := context.Background()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command(bin, append([]string{"--repo", repo, "--password-file", pass}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("restic %v: %v\n%s", args, err, out)
		}
	}
	run("init")
	if _, err := tool.Latest(ctx); !errors.Is(err, ErrNoSnapshot) {
		t.Fatalf("an empty repository: %v", err)
	}
	run("backup", data)
	snap, err := tool.Latest(ctx)
	if err != nil || snap.ID == "" || time.Since(snap.Time) > time.Minute {
		t.Fatalf("latest: %+v, %v", snap, err)
	}
	real, _ := filepath.EvalSymlinks(data)
	paths, err := tool.Paths(ctx)
	if err != nil || !(paths[filepath.Join(data, "sub", "kept.txt")] || paths[filepath.Join(real, "sub", "kept.txt")]) {
		t.Fatalf("paths: %v, %v", paths, err)
	}
	want := filepath.Join(data, "sub", "kept.txt")
	if !paths[want] {
		want = filepath.Join(real, "sub", "kept.txt")
	}
	target := filepath.Join(dir, "restored")
	if err := tool.Restore(ctx, []string{want}, target); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(target, want)); err != nil || string(got) != "kept" {
		t.Fatalf("restored %q, %v", got, err)
	}
	os.WriteFile(pass, []byte("wrong\n"), 0o600)
	if _, err := tool.Latest(ctx); err == nil || strings.Contains(err.Error(), "for-this-test-only") {
		t.Fatalf("a wrong password must fail without echoing anything: %v", err)
	}
}

func TestToolsThatAreNotThere(t *testing.T) {
	if New(config.Backup{}) != nil {
		t.Fatal("no backup declared: no tool")
	}
	missing := &Restic{Binary: "restic-that-is-not-installed", Repository: "/nowhere"}
	if _, err := missing.Latest(context.Background()); err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("a missing command: %v", err)
	}
}
