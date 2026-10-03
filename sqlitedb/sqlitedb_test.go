// Copyright 2026 Yauhen Bichel
// SPDX-License-Identifier: Apache-2.0

package sqlitedb

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/YauhenBichel/server-durability/durable"
)

// live makes a database in WAL mode whose 5,000 committed rows are all still in the log, with a writer that
// holds one more row uncommitted: the state of a busy service at the moment a backup runs.
func live(t *testing.T, path string) (*sql.DB, *sql.Tx) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	for _, q := range []string{"PRAGMA journal_mode=WAL", "PRAGMA wal_autocheckpoint=0", "CREATE TABLE t(x)", "CREATE TABLE empty(y)"} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	tx, _ := db.Begin()
	for i := 0; i < 5000; i++ {
		tx.Exec("INSERT INTO t VALUES(?)", i)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	open, _ := db.Begin()
	open.Exec("INSERT INTO t VALUES(-1)")
	return db, open
}

func TestStageCopiesExactlyWhatIsCommitted(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "live.db")
	db, open := live(t, src)
	defer db.Close()
	if mode, err := JournalMode(src); err != nil || mode != "wal" {
		t.Fatalf("journal mode %q, %v", mode, err)
	}
	if LogBytes(src) == 0 {
		t.Fatal("the test wants the commits to be in the log")
	}
	dst := filepath.Join(dir, "staged", "copy.db")
	if err := Stage(src, dst); err != nil {
		t.Fatal(err)
	}
	rows, err := Rows(dst)
	if err != nil || rows["t"] != 5000 || rows["empty"] != 0 || len(rows) != 2 {
		t.Fatalf("the copy holds %v, %v: want 5000 committed rows and not the uncommitted one", rows, err)
	}
	if err := IntegrityCheck(dst); err != nil {
		t.Fatal(err)
	}
	open.Rollback()
	if err := Stage(src, dst); err != nil { // a second run replaces the copy
		t.Fatal(err)
	}
}

func TestStageLeavesNothingBehindWhenItFails(t *testing.T) {
	dir := t.TempDir()
	notDB := filepath.Join(dir, "broken.db")
	os.WriteFile(notDB, []byte(strings.Repeat("this only has the name of a database ", 300)), 0o644)
	dest := filepath.Join(dir, "staged")
	for _, src := range []string{notDB, filepath.Join(dir, "missing.db")} {
		if err := Stage(src, filepath.Join(dest, "copy.db")); err == nil {
			t.Fatalf("staging %s must fail", src)
		}
	}
	left, _ := filepath.Glob(filepath.Join(dest, "*"))
	if len(left) != 0 {
		t.Fatalf("a failed copy left files: %v", left)
	}
	if _, err := JournalMode(notDB); err == nil || !strings.Contains(err.Error(), "not a SQLite database") {
		t.Fatalf("the header check said %v", err)
	}
	if !strings.Contains(durable.TempMark, "tmp") {
		t.Fatal("staging uses the mark SweepTemp knows")
	}
}

func TestCheckpointEmptiesTheLogAndRefusesUnderAWriter(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "live.db")
	db, open := live(t, src)
	if err := Checkpoint(src); err == nil {
		t.Fatal("a checkpoint under an open write transaction must be refused")
	}
	open.Rollback()
	db.Close()
	if err := Checkpoint(src); err != nil {
		t.Fatal(err)
	}
	if LogBytes(src) != 0 {
		t.Fatalf("the log still has %d bytes", LogBytes(src))
	}
	if rows, err := Rows(src); err != nil || rows["t"] != 5000 {
		t.Fatalf("after the checkpoint: %v, %v", rows, err)
	}
	if err := QuickCheck(src); err != nil {
		t.Fatal(err)
	}
}
