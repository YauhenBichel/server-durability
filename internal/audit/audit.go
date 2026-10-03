// Copyright 2026 Yauhen Bichel
// SPDX-License-Identifier: Apache-2.0

// Package audit checks a machine against its config file and reports, item by item, what could be lost:
// data on a RAM disk, a backup on the same disk as the data, a snapshot that is missing data, a backup that
// was never restore-tested, a service that will not start after a reboot. It only reads.
package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/YauhenBichel/server-durability/internal/backup"
	"github.com/YauhenBichel/server-durability/internal/config"
	"github.com/YauhenBichel/server-durability/sqlitedb"
)

// Levels of a finding, worst first.
const (
	Error   = "error"   // data or work would be lost
	Warning = "warning" // works today, but is a risk
	OK      = "ok"
	Info    = "info" // could not be checked here, or good to know
)

// Finding is one line of the report.
type Finding struct {
	Level   string `json:"level"`
	Subject string `json:"subject"` // a data entry's name, "backup", "restore test", a unit
	Message string `json:"message"`
	Fix     string `json:"fix,omitempty"`
}

// Env is everything outside the config file that the checks read. Tests pass their own.
type Env struct {
	ProcRoot string // "/proc"
	SysRoot  string // "/sys"
	Run      func(ctx context.Context, name string, args ...string) (string, error)
	Now      func() time.Time
	Tool     backup.Tool
}

// RestoreTest is the report a restore test saves in the state directory.
type RestoreTest struct {
	Time    time.Time         `json:"time"`
	OK      bool              `json:"ok"`
	Seconds float64           `json:"seconds"`
	Items   []RestoreTestItem `json:"items"`
}

// RestoreTestItem is one data entry of a restore test.
type RestoreTestItem struct {
	Name     string           `json:"name"`
	OK       bool             `json:"ok"`
	Message  string           `json:"message"`
	Restored map[string]int64 `json:"restored_rows,omitempty"`
	Live     map[string]int64 `json:"live_rows,omitempty"`
}

// RestoreTestFile is where the last restore test's report is saved.
func RestoreTestFile(c *config.Config) string { return filepath.Join(c.StateDir, "restore-test.json") }

type mount struct {
	point, fstype, options, dev string
}

func mounts(procRoot string) ([]mount, error) {
	raw, err := os.ReadFile(filepath.Join(procRoot, "self", "mountinfo"))
	if err != nil {
		return nil, err
	}
	var out []mount
	for _, line := range strings.Split(string(raw), "\n") {
		left, right, ok := strings.Cut(line, " - ")
		a, b := strings.Fields(left), strings.Fields(right)
		if !ok || len(a) < 6 || len(b) < 3 {
			continue
		}
		out = append(out, mount{point: strings.ReplaceAll(a[4], `\040`, " "), fstype: b[0], options: a[5] + "," + b[2], dev: a[2]})
	}
	return out, nil
}

func mountOf(all []mount, path string) (mount, bool) {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	best, found := mount{}, false
	for _, m := range all {
		if path == m.point || strings.HasPrefix(path, strings.TrimSuffix(m.point, "/")+"/") {
			if !found || len(m.point) >= len(best.point) {
				best, found = m, true
			}
		}
	}
	return best, found
}

// disks returns the physical disks under a block device, through partitions and device-mapper (LVM) layers.
func disks(sysRoot, dev string, depth int) []string {
	if depth > 8 {
		return nil
	}
	real, err := filepath.EvalSymlinks(filepath.Join(sysRoot, "dev", "block", dev))
	if err != nil {
		return nil
	}
	if slaves, err := os.ReadDir(filepath.Join(real, "slaves")); err == nil && len(slaves) > 0 {
		seen := map[string]bool{}
		for _, s := range slaves {
			if raw, err := os.ReadFile(filepath.Join(real, "slaves", s.Name(), "dev")); err == nil {
				for _, d := range disks(sysRoot, strings.TrimSpace(string(raw)), depth+1) {
					seen[d] = true
				}
			}
		}
		out := make([]string, 0, len(seen))
		for d := range seen {
			out = append(out, d)
		}
		sort.Strings(out)
		return out
	}
	if _, err := os.Stat(filepath.Join(real, "partition")); err == nil {
		return []string{filepath.Base(filepath.Dir(real))}
	}
	return []string{filepath.Base(real)}
}

func sortedKeys(m map[string][]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func shared(a, b []string) string {
	for _, x := range a {
		for _, y := range b {
			if x == y {
				return x
			}
		}
	}
	return ""
}

func age(d time.Duration) string {
	switch {
	case d < 2*time.Hour:
		return fmt.Sprintf("%d minutes", int(d.Minutes()))
	case d < 72*time.Hour:
		return fmt.Sprintf("%d hours", int(d.Hours()))
	default:
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	}
}

// Run checks the machine against its config file.
func Run(ctx context.Context, c *config.Config, env Env) []Finding {
	var out []Finding
	add := func(level, subject, message, fix string) {
		out = append(out, Finding{Level: level, Subject: subject, Message: message, Fix: fix})
	}
	now := env.Now()
	all, mountErr := mounts(env.ProcRoot)
	if mountErr != nil {
		add(Info, "machine", "file system and disk checks are skipped on this system (no "+env.ProcRoot+"/self/mountinfo)", "")
	}
	var repoDisks []string
	localRepo := c.Backup.Repository != "" && filepath.IsAbs(c.Backup.Repository)
	if localRepo && mountErr == nil {
		if m, ok := mountOf(all, c.Backup.Repository); ok {
			repoDisks = disks(env.SysRoot, m.dev, 0)
		}
	}

	// ---- each data entry: does it exist, what kind is it, what is it stored on ----
	present := map[string]bool{}
	var sameDisk []string // entries on the same disk as the backup repository
	sharedDisk := ""
	volatile := map[string][]string{} // disk -> entries on it, for disks with a volatile write cache
	for _, s := range c.Data {
		info, err := os.Stat(s.Path)
		if err != nil {
			add(Error, s.Name, "path not found: "+s.Path, "fix the path in the config file, or remove this entry")
			continue
		}
		present[s.Name] = true
		if s.Kind == "directory" != info.IsDir() {
			add(Warning, s.Name, fmt.Sprintf("the config says kind = %q, but %s is %s", s.Kind, s.Path, map[bool]string{true: "a directory", false: "a file"}[info.IsDir()]), "fix `kind` in the config file")
		}
		if s.Kind == "sqlite" {
			mode, err := sqlitedb.JournalMode(s.Path)
			switch {
			case err != nil:
				add(Error, s.Name, err.Error(), "")
			case mode == "wal":
				if log := sqlitedb.LogBytes(s.Path); log > 256<<20 {
					add(Warning, s.Name, fmt.Sprintf("its WAL file is %d MB: changes are not being checkpointed into the main database file", log>>20), "let the service run a checkpoint, or run one while the service is stopped")
				} else {
					add(OK, s.Name, "SQLite database, WAL mode", "")
				}
			default:
				add(OK, s.Name, "SQLite database, rollback journal mode", "")
			}
		}
		if mountErr != nil {
			continue
		}
		m, ok := mountOf(all, s.Path)
		if !ok {
			continue
		}
		switch {
		case m.fstype == "tmpfs" || m.fstype == "ramfs" || m.fstype == "devtmpfs":
			add(Error, s.Name, "stored on "+m.fstype+" ("+m.point+"): this is RAM, the data is lost on reboot", "move it to a disk")
		case strings.Contains(m.options, "data=writeback") || strings.Contains(m.options, "nobarrier") || strings.Contains(m.options, "barrier=0"):
			add(Warning, s.Name, "its file system ("+m.fstype+" at "+m.point+") is mounted with unsafe options: a power loss can corrupt data", "remove data=writeback / nobarrier from the mount options")
		}
		under := disks(env.SysRoot, m.dev, 0)
		if d := shared(under, repoDisks); d != "" {
			sameDisk, sharedDisk = append(sameDisk, s.Name), d
		}
		for _, d := range under {
			if raw, err := os.ReadFile(filepath.Join(env.SysRoot, "block", d, "queue", "write_cache")); err == nil && strings.TrimSpace(string(raw)) == "write back" {
				volatile[d] = append(volatile[d], s.Name)
			}
		}
	}
	for _, d := range sortedKeys(volatile) {
		add(Info, "disk "+d, "has a volatile write cache (used by: "+strings.Join(volatile[d], ", ")+"): data that was not fsynced is lost on power loss. Fsynced data is safe if the drive honours flush commands", "")
	}
	var elsewhere []string // configured backup copies that are not on the repository's disk
	for _, cp := range c.BackupCopies {
		if cp.Location != "same-disk" {
			elsewhere = append(elsewhere, cp.Name)
		}
	}
	if len(sameDisk) > 0 {
		what := "the backup repository is on the same disk (" + sharedDisk + ") as the data (" + strings.Join(sameDisk, ", ") + ")"
		if len(elsewhere) == 0 {
			add(Error, "backup", what+": if this disk fails, the data and its only backup are both lost", "keep a copy of the backup on another disk or another machine and add it as a [[backup_copy]]")
		} else {
			add(Warning, "backup", what+": if this disk fails, only the other backup copy ("+strings.Join(elsewhere, ", ")+") is left, as old as its last sync", "move the backup repository to another disk, or sync the other copy more often")
		}
	}

	// ---- the backup: is there one, how old is it, does it contain all data, where are its copies ----
	if env.Tool == nil {
		add(Error, "backup", "no backup is configured", "add a [backup] section; `server-durability init` prints an example")
	} else {
		snap, err := env.Tool.Latest(ctx)
		switch {
		case errors.Is(err, backup.ErrNoSnapshot):
			add(Error, "backup", "the backup repository has no snapshots", "run the backup once")
		case err != nil:
			add(Error, "backup", "cannot read the latest snapshot: "+err.Error(), "")
		default:
			old := now.Sub(snap.Time)
			if old > time.Duration(c.Backup.MaxAgeHours)*time.Hour {
				add(Error, "backup", fmt.Sprintf("the latest snapshot (%s) is %s old; the config allows %d hours", snap.ID, age(old), c.Backup.MaxAgeHours), "find out why the backup stopped running")
			} else {
				add(OK, "backup", fmt.Sprintf("the latest snapshot (%s) is %s old", snap.ID, age(old)), "")
			}
			out = append(out, Verify(ctx, c, env.Tool, present)...)
		}
		offsite := 0
		for _, cp := range c.BackupCopies {
			if cp.Location == "off-site" {
				offsite++
			}
		}
		switch {
		case len(elsewhere) == 0 && len(sameDisk) > 0:
			// already reported above, as an error: the only copy is on the data's disk
		case len(elsewhere) == 0:
			add(Warning, "backup", "the backup repository is the only copy", "copy the repository to another machine and add it as a [[backup_copy]]")
		case offsite == 0:
			add(Warning, "backup", "there is no off-site backup copy: a fire or theft would destroy all copies", "keep one copy off-site and add it with location = \"off-site\"")
		default:
			add(OK, "backup", fmt.Sprintf("%d other backup copies are configured, %d off-site", len(elsewhere), offsite), "")
		}
	}

	// ---- database copies: are live databases backed up from a consistent copy ----
	var plain []string
	for _, s := range c.Data {
		if s.Kind != "sqlite" || !present[s.Name] || env.Tool == nil {
			continue
		}
		if c.DBCopies.Dir == "" {
			plain = append(plain, s.Name)
			continue
		}
		info, err := os.Stat(c.SnapshotPath(s))
		switch {
		case err != nil:
			add(Warning, s.Name, "no database copy yet in "+c.DBCopies.Dir, "run `server-durability copy-db` before each backup")
		case now.Sub(info.ModTime()) > time.Duration(c.Backup.MaxAgeHours)*time.Hour:
			add(Warning, s.Name, "its database copy is "+age(now.Sub(info.ModTime()))+" old: copy-db is not running before the backup", "run `server-durability copy-db` before each backup")
		}
	}
	if len(plain) > 0 {
		add(Warning, "backup", "live SQLite databases are backed up as plain files ("+strings.Join(plain, ", ")+"): such a copy can open, pass the integrity check and still miss the latest commits", "set [db_copies] dir inside the backed-up paths and run `server-durability copy-db` before each backup")
	}

	// ---- has a restore ever been tested ----
	if env.Tool != nil {
		var last RestoreTest
		raw, err := os.ReadFile(RestoreTestFile(c))
		switch {
		case err != nil || json.Unmarshal(raw, &last) != nil:
			add(Warning, "restore test", "the backup has never been restore-tested: an untested backup may not work", "run `server-durability restore-test`")
		case !last.OK:
			add(Error, "restore test", "the last restore test ("+age(now.Sub(last.Time))+" ago) failed", "run `server-durability restore-test` and read its output")
		case now.Sub(last.Time) > time.Duration(c.RestoreTest.MaxAgeDays)*24*time.Hour:
			add(Warning, "restore test", fmt.Sprintf("the last restore test was %s ago; the config allows %d days", age(now.Sub(last.Time)), c.RestoreTest.MaxAgeDays), "run `server-durability restore-test`, or schedule it monthly")
		default:
			add(OK, "restore test", "last restore test: "+age(now.Sub(last.Time))+" ago, all data restored and opened", "")
		}
	}

	out = append(out, services(ctx, c, env)...)
	return out
}

// Verify checks that every configured data entry is in the latest snapshot. `present` limits the check to
// entries that exist; nil checks all of them.
func Verify(ctx context.Context, c *config.Config, tool backup.Tool, present map[string]bool) []Finding {
	paths, err := tool.Paths(ctx)
	if err != nil {
		return []Finding{{Level: Error, Subject: "backup", Message: "cannot list the latest snapshot: " + err.Error()}}
	}
	var out []Finding
	for _, s := range c.Data {
		if present != nil && !present[s.Name] {
			continue
		}
		want := c.SnapshotPath(s)
		real, _ := filepath.EvalSymlinks(want)
		if paths[want] || (real != "" && paths[real]) {
			out = append(out, Finding{Level: OK, Subject: s.Name, Message: "in the latest snapshot: " + want})
			continue
		}
		out = append(out, Finding{Level: Error, Subject: s.Name, Message: "missing from the latest snapshot: " + want,
			Fix: "add this path to the backup job"})
	}
	return out
}

// services checks what must start again after a reboot: configured services, long-running transient units,
// and calendar timers that skip a run when the machine was off.
func services(ctx context.Context, c *config.Config, env Env) []Finding {
	var out []Finding
	if _, err := env.Run(ctx, "systemctl", "--version"); err != nil {
		if len(c.Services) > 0 {
			out = append(out, Finding{Level: Info, Subject: "services", Message: "systemd not found: services are not checked"})
		}
		return out
	}
	for _, w := range c.Services {
		args := []string{"is-enabled", w.Unit}
		enable := "systemctl enable " + w.Unit
		if w.User {
			args = append([]string{"--user"}, args...)
			enable = "systemctl --user enable " + w.Unit
		}
		state, _ := env.Run(ctx, "systemctl", args...)
		state = strings.TrimSpace(state)
		switch state {
		case "enabled", "enabled-runtime", "static", "alias", "indirect", "generated":
			out = append(out, Finding{Level: OK, Subject: w.Unit, Message: "starts automatically after a reboot (" + state + ")"})
		case "":
			out = append(out, Finding{Level: Error, Subject: w.Unit, Message: "systemd does not know this unit", Fix: "fix the unit name in the config file"})
		default:
			out = append(out, Finding{Level: Error, Subject: w.Unit, Message: "is not enabled (" + state + "): it will not start after a reboot", Fix: "run: " + enable})
		}
	}
	for _, scope := range [][]string{{"--user"}, {}} {
		who := map[bool]string{true: "user", false: "system"}[len(scope) > 0]
		show, err := env.Run(ctx, "systemctl", append(append([]string{}, scope...), "show", "--type=service", "--state=running", "-p", "Id", "-p", "UnitFileState", "-p", "ActiveEnterTimestamp", "*")...)
		if err == nil {
			for _, block := range strings.Split(show, "\n\n") {
				p := props(block)
				if p["UnitFileState"] != "transient" {
					continue
				}
				started, err := time.Parse("Mon 2006-01-02 15:04:05 MST", p["ActiveEnterTimestamp"])
				if err == nil && env.Now().Sub(started) < time.Hour {
					continue
				}
				out = append(out, Finding{Level: Warning, Subject: p["Id"], Message: "a transient " + who + " unit (started with systemd-run) running for over an hour: it will not exist after a reboot",
					Fix: "run long jobs from an enabled unit file"})
			}
		}
		timers, err := env.Run(ctx, "systemctl", append(append([]string{}, scope...), "show", "--type=timer", "-p", "Id", "-p", "Persistent", "-p", "TimersCalendar", "-p", "UnitFileState", "*")...)
		if err != nil || len(scope) == 0 { // the system's own timers belong to the distribution
			continue
		}
		for _, block := range strings.Split(timers, "\n\n") {
			p := props(block)
			if p["TimersCalendar"] != "" && p["Persistent"] == "no" && p["UnitFileState"] == "enabled" && !severalTimesADay(p["TimersCalendar"]) {
				out = append(out, Finding{Level: Warning, Subject: p["Id"], Message: "calendar timer without Persistent=true: a run missed while the machine was off is skipped",
					Fix: "add Persistent=true to its [Timer] section"})
			}
		}
	}
	return out
}

// severalTimesADay reports whether a calendar timer runs more than once a day (any hour, or a list or a step
// of hours). Such a timer loses little when one run is missed, so Persistent=true is not worth a warning.
// The value is systemd's own form: "{ OnCalendar=*-*-* *:00/15:00 ; next_elapse=... }".
func severalTimesADay(timersCalendar string) bool {
	_, spec, ok := strings.Cut(timersCalendar, "OnCalendar=")
	if !ok {
		return false
	}
	spec, _, _ = strings.Cut(spec, ";")
	fields := strings.Fields(spec)
	if len(fields) == 0 {
		return false
	}
	hour, _, isTime := strings.Cut(fields[len(fields)-1], ":")
	return isTime && strings.ContainsAny(hour, "*/,.")
}

func props(block string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(block, "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			out[k] = v
		}
	}
	return out
}

// Worst is the most severe level among findings.
func Worst(findings []Finding) string {
	rank := map[string]int{Error: 3, Warning: 2, Info: 1, OK: 0}
	worst := OK
	for _, f := range findings {
		if rank[f.Level] > rank[worst] {
			worst = f.Level
		}
	}
	return worst
}
