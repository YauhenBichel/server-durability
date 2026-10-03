// Copyright 2026 Yauhen Bichel
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/YauhenBichel/server-durability/internal/backup"
	"github.com/YauhenBichel/server-durability/internal/config"
	_ "modernc.org/sqlite"
)

type fakeTool struct {
	snap  backup.Snapshot
	err   error
	paths map[string]bool
}

func (f *fakeTool) Latest(context.Context) (backup.Snapshot, error) { return f.snap, f.err }
func (f *fakeTool) Paths(context.Context) (map[string]bool, error)  { return f.paths, nil }
func (f *fakeTool) Restore(context.Context, []string, string) error { return nil }

var now = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

// machine builds a fake /proc and /sys: a root file system on LVM over a partition of nvme0n1, a second disk
// sdb mounted at <root>/other, and memory mounted at <root>/ram.
func machine(t *testing.T) (root string, env Env) {
	t.Helper()
	root, _ = filepath.EvalSymlinks(t.TempDir())
	proc, sys := filepath.Join(root, "proc"), filepath.Join(root, "sys")
	mk := func(p string) string { os.MkdirAll(p, 0o755); return p }
	part := mk(filepath.Join(sys, "devices", "pci", "nvme0n1", "nvme0n1p3"))
	os.WriteFile(filepath.Join(part, "partition"), []byte("3\n"), 0o644)
	os.WriteFile(filepath.Join(part, "dev"), []byte("259:3\n"), 0o644)
	dm := mk(filepath.Join(sys, "devices", "virtual", "block", "dm-0"))
	mk(filepath.Join(dm, "slaves"))
	os.Symlink(part, filepath.Join(dm, "slaves", "nvme0n1p3"))
	sdb := mk(filepath.Join(sys, "devices", "pci", "sdb"))
	mk(filepath.Join(sys, "dev", "block"))
	os.Symlink(dm, filepath.Join(sys, "dev", "block", "252:0"))
	os.Symlink(part, filepath.Join(sys, "dev", "block", "259:3"))
	os.Symlink(sdb, filepath.Join(sys, "dev", "block", "8:16"))
	mk(filepath.Join(sys, "block", "nvme0n1", "queue"))
	os.WriteFile(filepath.Join(sys, "block", "nvme0n1", "queue", "write_cache"), []byte("write back\n"), 0o644)
	mk(filepath.Join(proc, "self"))
	info := fmt.Sprintf("1 0 252:0 / / rw,relatime - ext4 /dev/mapper/vg-root rw\n2 1 8:16 / %s rw - ext4 /dev/sdb rw\n3 1 0:25 / %s rw - tmpfs tmpfs rw\n",
		mk(filepath.Join(root, "other")), mk(filepath.Join(root, "ram")))
	os.WriteFile(filepath.Join(proc, "self", "mountinfo"), []byte(info), 0o644)
	env = Env{ProcRoot: proc, SysRoot: sys, Now: func() time.Time { return now },
		Run: func(context.Context, string, ...string) (string, error) { return "", errors.New("no systemd") }}
	return root, env
}

func database(t *testing.T, path string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{"PRAGMA journal_mode=WAL", "CREATE TABLE t(x)", "INSERT INTO t VALUES(1)"} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
}

func lines(findings []Finding, level string) []string {
	var out []string
	for _, f := range findings {
		if f.Level == level {
			out = append(out, f.Subject+": "+f.Message)
		}
	}
	return out
}

func has(t *testing.T, got []string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		found := false
		for _, g := range got {
			found = found || strings.Contains(g, want)
		}
		if !found {
			t.Errorf("no finding says %q among:\n  %s", want, strings.Join(got, "\n  "))
		}
	}
}

func TestAMachineInGoodOrderHasNothingToFix(t *testing.T) {
	root, env := machine(t)
	c := &config.Config{StateDir: filepath.Join(root, "state")}
	c.Data = []config.Data{{Name: "app", Path: filepath.Join(root, "data", "app.db"), Kind: "sqlite"}, {Name: "uploads", Path: filepath.Join(root, "data", "uploads"), Kind: "directory"}}
	database(t, c.Data[0].Path)
	os.MkdirAll(c.Data[1].Path, 0o755)
	c.DBCopies.Dir = filepath.Join(root, "data", "staged")
	database(t, c.SnapshotPath(c.Data[0]))
	os.Chtimes(c.SnapshotPath(c.Data[0]), now.Add(-time.Hour), now.Add(-time.Hour))
	c.Backup = config.Backup{Tool: "restic", Repository: filepath.Join(root, "other", "repo"), MaxAgeHours: 30}
	c.BackupCopies = []config.BackupCopy{{Name: "cloud", Location: "off-site"}}
	c.Services = []config.Service{{Unit: "app.service", User: true}}
	c.RestoreTest.MaxAgeDays = 35
	os.MkdirAll(c.StateDir, 0o755)
	report, _ := json.Marshal(RestoreTest{Time: now.Add(-48 * time.Hour), OK: true})
	os.WriteFile(RestoreTestFile(c), report, 0o644)
	env.Tool = &fakeTool{snap: backup.Snapshot{ID: "abc123", Time: now.Add(-5 * time.Hour)},
		paths: map[string]bool{c.SnapshotPath(c.Data[0]): true, c.Data[1].Path: true}}
	env.Run = func(_ context.Context, _ string, args ...string) (string, error) {
		if strings.Contains(strings.Join(args, " "), "is-enabled app.service") {
			return "enabled\n", nil
		}
		return "", nil
	}
	got := Run(context.Background(), c, env)
	if fails, warns := lines(got, Error), lines(got, Warning); len(fails)+len(warns) != 0 {
		t.Fatalf("nothing should need fixing:\n%s\n%s", strings.Join(fails, "\n"), strings.Join(warns, "\n"))
	}
	has(t, lines(got, OK), "SQLite database, WAL mode", "latest snapshot (abc123) is 5 hours old", "in the latest snapshot", "last restore test: 48 hours ago", "starts automatically", "1 off-site")
	has(t, lines(got, Info), "disk nvme0n1: has a volatile write cache (used by: app, uploads)")
	if Worst(got) != Info {
		t.Fatalf("the worst level is %s", Worst(got))
	}
}

func TestEveryWayToLoseDataIsNamed(t *testing.T) {
	root, env := machine(t)
	c := &config.Config{StateDir: filepath.Join(root, "state")}
	c.Data = []config.Data{
		{Name: "app", Path: filepath.Join(root, "data", "app.db"), Kind: "sqlite"},
		{Name: "cache", Path: filepath.Join(root, "ram", "session.db"), Kind: "sqlite"},
		{Name: "gone", Path: filepath.Join(root, "data", "gone"), Kind: "directory"},
		{Name: "fake", Path: filepath.Join(root, "data", "fake.db"), Kind: "sqlite"},
	}
	database(t, c.Data[0].Path)
	database(t, c.Data[1].Path)
	os.WriteFile(c.Data[3].Path, []byte(strings.Repeat("not a database ", 10)), 0o644)
	c.Backup = config.Backup{Tool: "restic", Repository: filepath.Join(root, "data", "repo"), MaxAgeHours: 30} // the same disk as the data
	c.Services = []config.Service{{Unit: "app.service", User: true}, {Unit: "typo.service"}}
	c.RestoreTest.MaxAgeDays = 35
	env.Tool = &fakeTool{snap: backup.Snapshot{ID: "old111", Time: now.Add(-9 * 24 * time.Hour)}, paths: map[string]bool{c.Data[1].Path: true}}
	env.Run = func(_ context.Context, _ string, args ...string) (string, error) {
		a := strings.Join(args, " ")
		switch {
		case strings.Contains(a, "is-enabled app.service"):
			return "disabled\n", errors.New("exit 1")
		case strings.Contains(a, "is-enabled typo.service"):
			return "", errors.New("exit 1")
		case strings.HasPrefix(a, "--user show --type=service"):
			return "Id=run-u42.service\nUnitFileState=transient\nActiveEnterTimestamp=Sat 2026-10-03 05:00:00 UTC\n\nId=fresh.service\nUnitFileState=transient\nActiveEnterTimestamp=Sat 2026-10-03 11:50:00 UTC\n\nId=app.service\nUnitFileState=disabled\nActiveEnterTimestamp=Sat 2026-10-03 05:00:00 UTC\n", nil
		case strings.HasPrefix(a, "--user show --type=timer"):
			return "Id=nightly.timer\nPersistent=no\nTimersCalendar={ OnCalendar=*-*-* 03:00:00 ; next_elapse=n/a }\nUnitFileState=enabled\n\nId=often.timer\nPersistent=no\nTimersCalendar=\nUnitFileState=enabled\n", nil
		}
		return "", nil
	}
	got := Run(context.Background(), c, env)
	has(t, lines(got, Error),
		"gone: path not found",
		"cache: stored on tmpfs",
		"fake: ", "not a SQLite database",
		"backup: the backup repository is on the same disk (nvme0n1) as the data (app, fake): if this disk fails, the data and its only backup are both lost",
		"backup: the latest snapshot (old111) is 9 days old; the config allows 30 hours",
		"app: missing from the latest snapshot",
		"app.service: is not enabled (disabled)",
		"typo.service: systemd does not know this unit")
	for _, l := range lines(got, Warning) {
		if strings.Contains(l, "is the only copy") {
			t.Errorf("said twice: the only copy already fails for sharing the disk: %s", l)
		}
	}
	has(t, lines(got, Warning),
		"backup: live SQLite databases are backed up as plain files (app, cache, fake)",
		"restore test: the backup has never been restore-tested",
		"run-u42.service: a transient user unit",
		"nightly.timer: calendar timer without Persistent=true")
	for _, l := range append(lines(got, Warning), lines(got, Error)...) {
		if strings.Contains(l, "fresh.service") || strings.Contains(l, "often.timer") || strings.HasPrefix(l, "gone: missing from the latest") {
			t.Errorf("should not be reported: %s", l)
		}
	}
	if Worst(got) != Error {
		t.Fatalf("the worst level is %s", Worst(got))
	}
	c.BackupCopies = []config.BackupCopy{{Name: "laptop", Location: "same-site"}}
	withCopy := Run(context.Background(), c, env)
	has(t, lines(withCopy, Warning), "same disk (nvme0n1) as the data (app, fake): if this disk fails, only the other backup copy (laptop) is left", "there is no off-site backup copy")
	for _, l := range lines(withCopy, Error) {
		if strings.Contains(l, "same disk") {
			t.Errorf("with a copy elsewhere, a shared disk is a warning: %s", l)
		}
	}
	env.Tool = nil
	has(t, lines(Run(context.Background(), c, env), Error), "backup: no backup is configured")
	env.Tool = &fakeTool{err: backup.ErrNoSnapshot}
	has(t, lines(Run(context.Background(), c, env), Error), "the backup repository has no snapshots")
	env.ProcRoot = filepath.Join(root, "nowhere")
	has(t, lines(Run(context.Background(), c, env), Info), "file system and disk checks are skipped on this system")
}
