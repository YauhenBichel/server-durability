// Copyright 2026 Yauhen Bichel
// SPDX-License-Identifier: Apache-2.0

// Package sqlitedb does to a SQLite database what a backup and a shutdown need, without the service that
// owns it noticing: a consistent copy while it is being written, a checkpoint that empties its log, and the
// checks that say a copy can be trusted.
//
// Why a copy needs this: in WAL mode the newest commits are in the -wal file, not in the main file. A plain
// file copy opens, passes its integrity check, and lacks them. SQLite's backup API reads one consistent
// snapshot instead, and that is what Stage uses.
package sqlitedb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/YauhenBichel/server-durability/durable"
	"modernc.org/sqlite"
)

type backuper interface {
	NewBackup(string) (*sqlite.Backup, error)
}

func uri(path string, params string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	u := url.URL{Scheme: "file", Path: abs, RawQuery: params}
	return u.String(), nil
}

// JournalMode reads the database header: "wal", or "rollback" for the older journal modes. It opens nothing
// with SQLite, so it is safe on a database of any service.
func JournalMode(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	head := make([]byte, 20)
	if _, err := f.ReadAt(head, 0); err != nil {
		return "", fmt.Errorf("%s: too short to be a SQLite database", path)
	}
	if string(head[:16]) != "SQLite format 3\x00" {
		return "", fmt.Errorf("%s: not a SQLite database", path)
	}
	if head[18] == 2 && head[19] == 2 {
		return "wal", nil
	}
	return "rollback", nil
}

// LogBytes is the size of the write-ahead log beside path: commits not yet in the main file. 0 when there is none.
func LogBytes(path string) int64 {
	info, err := os.Stat(path + "-wal")
	if err != nil {
		return 0
	}
	return info.Size()
}

// Stage writes a consistent copy of the live database src to dst. The copy holds every committed change and
// nothing uncommitted, is checked, synced, and only then given its name: a failed copy leaves no file.
func Stage(src, dst string) error {
	if _, err := JournalMode(src); err != nil {
		return err
	}
	from, err := uri(src, "mode=ro&_pragma=busy_timeout(30000)")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp := dst + durable.TempMark + "stage"
	os.Remove(tmp)
	done := false
	defer func() {
		if !done {
			os.Remove(tmp)
		}
	}()
	db, err := sql.Open("sqlite", from)
	if err != nil {
		return err
	}
	defer db.Close()
	conn, err := db.Conn(context.Background())
	if err != nil {
		return err
	}
	defer conn.Close()
	err = conn.Raw(func(dc any) error {
		b, ok := dc.(backuper)
		if !ok {
			return errors.New("the SQLite driver has no backup API")
		}
		bk, err := b.NewBackup(tmp)
		if err != nil {
			return err
		}
		for more := true; more; {
			if more, err = bk.Step(-1); err != nil {
				bk.Finish()
				return err
			}
		}
		return bk.Finish()
	})
	if err != nil {
		return fmt.Errorf("copy %s: %w", src, err)
	}
	if err := QuickCheck(tmp); err != nil {
		return fmt.Errorf("the copy of %s: %w", src, err)
	}
	f, err := os.Open(tmp)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	f.Close()
	if err := os.Rename(tmp, dst); err != nil {
		return err
	}
	done = true
	return durable.SyncDir(filepath.Dir(dst))
}

// QuickCheck opens path read-only and runs SQLite's quick_check. nil means the file is a sound database.
func QuickCheck(path string) error {
	return check(path, "quick_check")
}

// IntegrityCheck is the thorough check: slower, and what a rehearsed restore should pass.
func IntegrityCheck(path string) error {
	return check(path, "integrity_check")
}

func check(path, pragma string) error {
	u, err := uri(path, "mode=ro")
	if err != nil {
		return err
	}
	db, err := sql.Open("sqlite", u)
	if err != nil {
		return err
	}
	defer db.Close()
	var result string
	if err := db.QueryRow("PRAGMA " + pragma).Scan(&result); err != nil {
		return fmt.Errorf("%s: %w", pragma, err)
	}
	if result != "ok" {
		return fmt.Errorf("%s says: %s", pragma, result)
	}
	return nil
}

// Checkpoint moves every committed change from the log into the main file and empties the log. It fails,
// changing nothing, when another process is writing: stop the writer first.
func Checkpoint(path string) error {
	u, err := uri(path, "_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	db, err := sql.Open("sqlite", u)
	if err != nil {
		return err
	}
	defer db.Close()
	var busy, log, moved int
	if err := db.QueryRow("PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &log, &moved); err != nil {
		return err
	}
	if busy != 0 {
		return fmt.Errorf("%s still has a writer", path)
	}
	return nil
}

// Rows counts the rows of every table: what a rehearsed restore compares with the live database.
func Rows(path string) (map[string]int64, error) {
	u, err := uri(path, "mode=ro")
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", u)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	names, err := db.Query("SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name")
	if err != nil {
		return nil, err
	}
	var tables []string
	for names.Next() {
		var n string
		if err := names.Scan(&n); err != nil {
			names.Close()
			return nil, err
		}
		tables = append(tables, n)
	}
	names.Close()
	out := map[string]int64{}
	for _, t := range tables {
		var n int64
		if err := db.QueryRow(`SELECT count(*) FROM "` + strings.ReplaceAll(t, `"`, `""`) + `"`).Scan(&n); err != nil {
			return nil, fmt.Errorf("count %s: %w", t, err)
		}
		out[t] = n
	}
	return out, nil
}
