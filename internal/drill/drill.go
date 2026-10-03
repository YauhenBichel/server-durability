// Copyright 2026 Yauhen Bichel
// SPDX-License-Identifier: Apache-2.0

// Package drill rehearses a restore: it takes every declared store out of the newest snapshot into a scratch
// directory, opens what it got, and writes down whether each one is usable. A backup that has never been
// restored is a hope; this turns it into something that was seen to work, with a date.
package drill

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

func total(rows map[string]int64) (n int64) {
	for _, v := range rows {
		n += v
	}
	return n
}

// Run restores, opens, compares, records. The scratch directory is removed whatever happens. The live
// stores are only read.
func Run(ctx context.Context, c *config.Config, tool backup.Tool, now time.Time) (audit.DrillResult, error) {
	started := time.Now()
	result := audit.DrillResult{Time: now.UTC(), OK: true}
	if err := os.MkdirAll(c.StateDir, 0o755); err != nil {
		return result, err
	}
	durable.SweepTemp(c.StateDir, time.Hour)
	scratch, err := os.MkdirTemp(c.StateDir, "drill-")
	if err != nil {
		return result, err
	}
	defer os.RemoveAll(scratch)
	// A snapshot holds a path as the backup was given it: through a symbolic link, or resolved. Ask which.
	inSnapshot, err := tool.Paths(ctx)
	if err != nil {
		return result, fmt.Errorf("the newest snapshot cannot be listed: %w", err)
	}
	for _, s := range c.Stores {
		one := audit.DrillStore{Name: s.Name}
		want := c.SnapshotPath(s)
		if real, err := filepath.EvalSymlinks(want); err == nil && !inSnapshot[want] && inSnapshot[real] {
			want = real
		}
		restored := filepath.Join(scratch, want)
		err := tool.Restore(ctx, []string{want}, scratch)
		info, statErr := os.Stat(restored)
		switch {
		case err != nil:
			one.Message = "the restore failed: " + err.Error()
		case statErr != nil:
			one.Message = "the snapshot gave nothing back for " + want
		case s.Kind == "sqlite":
			if err := sqlitedb.IntegrityCheck(restored); err != nil {
				one.Message = "the restored database is not sound: " + err.Error()
				break
			}
			if one.Restored, err = sqlitedb.Rows(restored); err != nil {
				one.Message = "the restored database cannot be read: " + err.Error()
				break
			}
			one.Live, _ = sqlitedb.Rows(s.Path) // the live one may be busy or gone: then there is nothing to compare with
			switch {
			case len(one.Restored) == 0:
				one.Message = "the restored database opens and has no tables"
			case one.Live != nil && tables(one.Live) != tables(one.Restored):
				one.Message = fmt.Sprintf("the restored database has other tables (%s) than the live one (%s)", tables(one.Restored), tables(one.Live))
			default:
				one.OK = true
				one.Message = fmt.Sprintf("restored, sound, %d tables, %d rows", len(one.Restored), total(one.Restored))
				if one.Live != nil {
					one.Message += fmt.Sprintf(" (the live one has %d now)", total(one.Live))
				}
			}
		case s.Kind == "directory":
			entries, _ := os.ReadDir(restored)
			live, _ := os.ReadDir(s.Path)
			if !info.IsDir() || (len(entries) == 0 && len(live) > 0) {
				one.Message = "the restored directory is empty and the live one is not"
			} else {
				one.OK, one.Message = true, fmt.Sprintf("restored, %d entries at its top", len(entries))
			}
		default:
			live, liveErr := os.Stat(s.Path)
			if info.Size() == 0 && liveErr == nil && live.Size() > 0 {
				one.Message = "the restored file is empty and the live one is not"
			} else {
				one.OK, one.Message = true, fmt.Sprintf("restored, %d bytes", info.Size())
			}
		}
		result.OK = result.OK && one.OK
		result.Stores = append(result.Stores, one)
	}
	result.Seconds = float64(time.Since(started).Milliseconds()) / 1000
	raw, err := json.MarshalIndent(result, "", " ")
	if err != nil {
		return result, err
	}
	return result, durable.ReplaceFile(audit.DrillFile(c), append(raw, '\n'), 0o644)
}
