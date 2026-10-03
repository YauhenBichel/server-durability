// Copyright 2026 Yauhen Bichel
// SPDX-License-Identifier: Apache-2.0

package durable

import (
	"bytes"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAppendLineAndWriteFileAtomic(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "log")
	for _, line := range []string{"one", "two"} {
		if err := AppendLine(log, line); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := os.ReadFile(log); string(got) != "one\ntwo\n" {
		t.Fatalf("log holds %q", got)
	}
	state := filepath.Join(dir, "state.json")
	for _, v := range []string{`{"v":1}`, `{"v":2}`} {
		if err := WriteFileAtomic(state, []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := os.ReadFile(state)
	info, _ := os.Stat(state)
	if string(got) != `{"v":2}` || info.Mode().Perm() != 0o600 {
		t.Fatalf("state holds %q with mode %v", got, info.Mode().Perm())
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*"+TempMark+"*")); len(left) != 0 {
		t.Fatalf("temporary files left after clean writes: %v", left)
	}
	if err := WriteFileAtomic(filepath.Join(dir, "missing", "x"), []byte("x"), 0o644); err == nil {
		t.Fatal("a replace into a directory that does not exist must fail")
	}
}

func TestRemoveTempFilesRemovesOnlyOldLeftovers(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "state.json"+TempMark+"111")
	fresh := filepath.Join(dir, "state.json"+TempMark+"222")
	keep := filepath.Join(dir, "state.json")
	for _, f := range []string{old, fresh, keep} {
		os.WriteFile(f, []byte("x"), 0o644)
	}
	past := time.Now().Add(-2 * time.Hour)
	os.Chtimes(old, past, past)
	removed, err := RemoveTempFiles(dir, time.Hour)
	if err != nil || len(removed) != 1 || removed[0] != filepath.Base(old) {
		t.Fatalf("removed %v, err %v", removed, err)
	}
	for _, f := range []string{fresh, keep} {
		if _, err := os.Stat(f); err != nil {
			t.Fatalf("%s must stay: %v", f, err)
		}
	}
}

// TestWriteFileAtomicSurvivesKill kills a process that replaces a file, at a random moment, many times. The file
// must always be whole: the old content or the new. The helper below is this test binary run again.
func TestWriteFileAtomicSurvivesKill(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns and kills 150 processes")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "state")
	pad := strings.Repeat("x", 100_000)
	if err := os.WriteFile(target, []byte(pad), 0o644); err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(1))
	broken, finished := 0, 0
	for i := 0; i < 150; i++ {
		cmd := exec.Command(os.Args[0], "-test.run=TestHelperReplace")
		cmd.Env = append(os.Environ(), "DURABLE_HELPER_TARGET="+target)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Duration(rng.Intn(12000)) * time.Microsecond)
		cmd.Process.Kill()
		if cmd.Wait() == nil {
			finished++
		}
		got, err := os.ReadFile(target)
		if err != nil || !bytes.Equal(got, []byte(pad)) {
			broken++
		}
	}
	if broken != 0 {
		t.Fatalf("%d of 150 kills left a broken file", broken)
	}
	left, _ := filepath.Glob(filepath.Join(dir, "*"+TempMark+"*"))
	t.Logf("150 kills, %d replaces finished first, 0 broken files, %d temporary files left for RemoveTempFiles", finished, len(left))
	removed, err := RemoveTempFiles(dir, 0)
	if err != nil || len(removed) != len(left) {
		t.Fatalf("sweep removed %d of %d leftovers: %v", len(removed), len(left), err)
	}
}

func TestHelperReplace(t *testing.T) {
	target := os.Getenv("DURABLE_HELPER_TARGET")
	if target == "" {
		t.Skip("only as a helper process")
	}
	if err := WriteFileAtomic(target, []byte(strings.Repeat("x", 100_000)), 0o644); err != nil {
		os.Exit(1)
	}
}
