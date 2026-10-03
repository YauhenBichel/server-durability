// Copyright 2026 Yauhen Bichel
// SPDX-License-Identifier: Apache-2.0

// Package restoretest tests a backup by restoring from it: it restores every configured data entry from the
// latest snapshot into a temporary directory, opens what it got, and saves a report. An untested backup may
// not work; this shows, with a date, that this one does.
package restoretest

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/YauhenBichel/server-durability/durable"
	"github.com/YauhenBichel/server-durability/internal/audit"
	"github.com/YauhenBichel/server-durability/internal/backup"
	"github.com/YauhenBichel/server-durability/internal/config"
	"github.com/YauhenBichel/server-durability/sqlitedb"
)

func tables(rows map[string]int64) string {
	names := make([]string, 0, len(rows))
	for n := range rows {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}

func count(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	if strings.HasSuffix(word, "y") {
		return fmt.Sprintf("%d %sies", n, strings.TrimSuffix(word, "y"))
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func total(rows map[string]int64) (n int64) {
	for _, v := range rows {
		n += v
	}
	return n
}

// Run restores, opens, compares and saves the report. The temporary directory is always removed. The live
// data is only read.
func Run(ctx context.Context, c *config.Config, tool backup.Tool, now time.Time) (audit.RestoreTest, error) {
	started := time.Now()
	result := audit.RestoreTest{Time: now.UTC(), OK: true}
	if err := os.MkdirAll(c.StateDir, 0o755); err != nil {
		return result, err
	}
	durable.RemoveTempFiles(c.StateDir, time.Hour)
	scratch, err := os.MkdirTemp(c.StateDir, "restore-test-")
	if err != nil {
		return result, err
	}
	defer os.RemoveAll(scratch)
	// A snapshot stores a path the way the backup job gave it: through a symbolic link, or resolved. Ask which.
	inSnapshot, err := tool.Paths(ctx)
	if err != nil {
		return result, fmt.Errorf("cannot list the latest snapshot: %w", err)
	}
	for _, s := range c.Data {
		one := audit.RestoreTestItem{Name: s.Name}
		want := c.SnapshotPath(s)
		if real, err := filepath.EvalSymlinks(want); err == nil && !inSnapshot[want] && inSnapshot[real] {
			want = real
		}
		restored := filepath.Join(scratch, want)
		err := tool.Restore(ctx, []string{want}, scratch)
		info, statErr := os.Stat(restored)
		switch {
		case err != nil:
			one.Message = "restore failed: " + err.Error()
		case statErr != nil:
			one.Message = "the snapshot returned nothing for " + want
		case s.Kind == "sqlite":
			if err := sqlitedb.IntegrityCheck(restored); err != nil {
				one.Message = "the restored database is corrupt: " + err.Error()
				break
			}
			if one.Restored, err = sqlitedb.Rows(restored); err != nil {
				one.Message = "the restored database cannot be read: " + err.Error()
				break
			}
			one.Live, _ = sqlitedb.Rows(s.Path) // the live one may be busy or gone: then there is nothing to compare with
			switch {
			case len(one.Restored) == 0:
				one.Message = "the restored database opens but has no tables"
			case one.Live != nil && tables(one.Live) != tables(one.Restored):
				one.Message = fmt.Sprintf("the restored database has different tables (%s) than the live one (%s)", tables(one.Restored), tables(one.Live))
			default:
				one.OK = true
				one.Message = fmt.Sprintf("restored, integrity check ok, %s, %d rows", count(len(one.Restored), "table"), total(one.Restored))
				if one.Live != nil {
					one.Message += fmt.Sprintf(" (live database: %d rows)", total(one.Live))
				}
			}
		case s.Kind == "directory":
			entries, _ := os.ReadDir(restored)
			live, _ := os.ReadDir(s.Path)
			if !info.IsDir() || (len(entries) == 0 && len(live) > 0) {
				one.Message = "the restored directory is empty but the live one is not"
			} else {
				one.OK, one.Message = true, "restored, "+count(len(entries), "entry")+" at the top level"
			}
		default:
			live, liveErr := os.Stat(s.Path)
			if info.Size() == 0 && liveErr == nil && live.Size() > 0 {
				one.Message = "the restored file is empty but the live one is not"
			} else {
				one.OK, one.Message = true, fmt.Sprintf("restored, %d bytes", info.Size())
			}
		}
		result.OK = result.OK && one.OK
		result.Items = append(result.Items, one)
	}
	result.Seconds = float64(time.Since(started).Milliseconds()) / 1000
	raw, err := json.MarshalIndent(result, "", " ")
	if err != nil {
		return result, err
	}
	return result, durable.WriteFileAtomic(audit.RestoreTestFile(c), append(raw, '\n'), 0o644)
}
