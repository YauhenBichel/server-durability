// Copyright 2026 Yauhen Bichel
// SPDX-License-Identifier: Apache-2.0

// Package audit looks at a machine the way its declaration describes it and says, store by store, what
// would be lost and when: a store on a file system that forgets, a backup on the same disk as the data, a
// snapshot that does not hold what it is believed to hold, a restore nobody has tried, a service that will
// not come back after a restart. It reads and never changes anything.
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
	Fail = "fail" // data or work would be lost
	Warn = "warn" // it holds today and depends on luck
	OK   = "ok"
	Note = "note" // could not be checked here, or worth knowing
)

// Finding is one line of the audit.
type Finding struct {
	Level   string `json:"level"`
	Subject string `json:"subject"` // a store's name, "backup", "restore", a unit
	Message string `json:"message"`
	Fix     string `json:"fix,omitempty"`
}

// Env is everything outside the declaration that the audit reads. Tests give their own.
type Env struct {
	ProcRoot string // "/proc"
	SysRoot  string // "/sys"
	Run      func(ctx context.Context, name string, args ...string) (string, error)
	Now      func() time.Time
	Tool     backup.Tool
}

// DrillResult is what a rehearsed restore leaves in the state directory.
type DrillResult struct {
	Time    time.Time    `json:"time"`
	OK      bool         `json:"ok"`
	Seconds float64      `json:"seconds"`
	Stores  []DrillStore `json:"stores"`
}

// DrillStore is one store of a rehearsed restore.
type DrillStore struct {
	Name     string           `json:"name"`
	OK       bool             `json:"ok"`
	Message  string           `json:"message"`
	Restored map[string]int64 `json:"restored_rows,omitempty"`
	Live     map[string]int64 `json:"live_rows,omitempty"`
}

// DrillFile is where the last rehearsal's result is kept.
func DrillFile(c *config.Config) string { return filepath.Join(c.StateDir, "drill.json") }

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

// disks names the physical disks under a block device, through partitions and device-mapper layers.
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

// Run audits the machine against its declaration.
func Run(ctx context.Context, c *config.Config, env Env) []Finding {
	var out []Finding
	add := func(level, subject, message, fix string) {
		out = append(out, Finding{Level: level, Subject: subject, Message: message, Fix: fix})
	}
	now := env.Now()
	all, mountErr := mounts(env.ProcRoot)
	if mountErr != nil {
		add(Note, "machine", "file systems and disks are not checked on this system (no "+env.ProcRoot+"/self/mountinfo)", "")
	}
	var repoDisks []string
	localRepo := c.Backup.Repository != "" && filepath.IsAbs(c.Backup.Repository)
	if localRepo && mountErr == nil {
		if m, ok := mountOf(all, c.Backup.Repository); ok {
			repoDisks = disks(env.SysRoot, m.dev, 0)
		}
	}

	// ---- each store: is it there, how is it kept, what is under it ----
	present := map[string]bool{}
	var sameDisk []string // stores that share a disk with the backup repository
	sharedDisk := ""
	volatile := map[string][]string{} // disk -> stores on it, for disks with a volatile write cache
	for _, s := range c.Stores {
		info, err := os.Stat(s.Path)
		if err != nil {
			add(Fail, s.Name, "the store is not there: "+s.Path, "correct the path in the declaration, or remove the store from it")
			continue
		}
		present[s.Name] = true
		if s.Kind == "directory" != info.IsDir() {
			add(Warn, s.Name, fmt.Sprintf("declared as %s, but %s is %s", s.Kind, s.Path, map[bool]string{true: "a directory", false: "a file"}[info.IsDir()]), "correct `kind` in the declaration")
		}
		if s.Kind == "sqlite" {
			mode, err := sqlitedb.JournalMode(s.Path)
			switch {
			case err != nil:
				add(Fail, s.Name, err.Error(), "")
			case mode == "wal":
				if log := sqlitedb.LogBytes(s.Path); log > 256<<20 {
					add(Warn, s.Name, fmt.Sprintf("its write-ahead log is %d MB: commits are piling up outside the main file", log>>20), "let its service checkpoint, or checkpoint it while the service is stopped")
				} else {
					add(OK, s.Name, "a SQLite database in WAL mode", "")
				}
			default:
				add(OK, s.Name, "a SQLite database with a rollback journal", "")
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
			add(Fail, s.Name, "it is on "+m.fstype+" ("+m.point+"): memory, gone at the next restart", "move the store to a disk")
		case strings.Contains(m.options, "data=writeback") || strings.Contains(m.options, "nobarrier") || strings.Contains(m.options, "barrier=0"):
			add(Warn, s.Name, "its file system ("+m.fstype+" at "+m.point+") is mounted without the protections that make a power cut safe", "remove data=writeback / nobarrier from the mount options")
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
		add(Note, "disk "+d, "a volatile write cache under "+strings.Join(volatile[d], ", ")+": what is not synced is lost in a power cut. Synced writes are safe as long as the drive honours flushes", "")
	}
	var elsewhere []string // declared copies of the repository that are not on its disk
	for _, cp := range c.Copies {
		if cp.Where != "same-disk" {
			elsewhere = append(elsewhere, cp.Name)
		}
	}
	if len(sameDisk) > 0 {
		what := "the repository is on the same disk (" + sharedDisk + ") as " + strings.Join(sameDisk, ", ")
		if len(elsewhere) == 0 {
			add(Fail, "backup", what+": when that disk dies, the data and its only backup die together", "keep a copy of the repository on another disk or another machine, and declare it as a [[copy]]")
		} else {
			add(Warn, "backup", what+": when that disk dies, what remains is the declared copy ("+strings.Join(elsewhere, ", ")+"), as fresh as its last sync", "put the repository itself on another disk, or check how often the copy is refreshed")
		}
	}

	// ---- the backup: is there one, how old, does it hold every store, where else is it ----
	if env.Tool == nil {
		add(Fail, "backup", "no backup is declared", "add a [backup] section; `server-durability init` shows one")
	} else {
		snap, err := env.Tool.Latest(ctx)
		switch {
		case errors.Is(err, backup.ErrNoSnapshot):
			add(Fail, "backup", "the repository holds no snapshot", "run the backup once")
		case err != nil:
			add(Fail, "backup", "the newest snapshot cannot be read: "+err.Error(), "")
		default:
			old := now.Sub(snap.Time)
			if old > time.Duration(c.Backup.MaxAgeHours)*time.Hour {
				add(Fail, "backup", fmt.Sprintf("the newest snapshot (%s) is %s old; the declaration allows %d hours", snap.ID, age(old), c.Backup.MaxAgeHours), "look at why the backup stopped running")
			} else {
				add(OK, "backup", fmt.Sprintf("the newest snapshot (%s) is %s old", snap.ID, age(old)), "")
			}
			out = append(out, Covers(ctx, c, env.Tool, present)...)
		}
		offsite, others := 0, 0
		for _, cp := range c.Copies {
			if cp.Where == "off-site" {
				offsite++
			}
			if cp.Where != "same-disk" {
				others++
			}
		}
		switch {
		case others == 0 && len(sameDisk) > 0:
			// already said, and worse: the only copy shares a disk with the data
		case others == 0:
			add(Warn, "backup", "the repository is declared as the only copy", "copy the repository to another machine and declare it as a [[copy]]")
		case offsite == 0:
			add(Warn, "backup", "every copy of the repository is in one place: a fire or a theft takes them all", "keep one copy off-site and declare it with where = \"off-site\"")
		default:
			add(OK, "backup", fmt.Sprintf("%d other copies of the repository are declared, %d of them off-site", others, offsite), "")
		}
	}

	// ---- staging: are live databases copied consistently ----
	var plain []string
	for _, s := range c.Stores {
		if s.Kind != "sqlite" || !present[s.Name] || env.Tool == nil {
			continue
		}
		if c.Stage.Dir == "" {
			plain = append(plain, s.Name)
			continue
		}
		info, err := os.Stat(c.SnapshotPath(s))
		switch {
		case err != nil:
			add(Warn, s.Name, "no staged copy yet in "+c.Stage.Dir, "run `server-durability stage` before each backup")
		case now.Sub(info.ModTime()) > time.Duration(c.Backup.MaxAgeHours)*time.Hour:
			add(Warn, s.Name, "its staged copy is "+age(now.Sub(info.ModTime()))+" old: staging is not running before the backup", "run `server-durability stage` before each backup")
		}
	}

	if len(plain) > 0 {
		add(Warn, "backup", "live databases are copied as plain files ("+strings.Join(plain, ", ")+"): such a copy can open, pass its check and lack the newest commits", "declare a [stage] directory inside the backup's paths and run `server-durability stage` before each backup")
	}

	// ---- a restore that has been tried ----
	if env.Tool != nil {
		var last DrillResult
		raw, err := os.ReadFile(DrillFile(c))
		switch {
		case err != nil || json.Unmarshal(raw, &last) != nil:
			add(Warn, "restore", "a restore has never been rehearsed here: the backup is a hope until one is", "run `server-durability drill`")
		case !last.OK:
			add(Fail, "restore", "the last rehearsed restore, "+age(now.Sub(last.Time))+" ago, failed", "run `server-durability drill` and read what it says")
		case now.Sub(last.Time) > time.Duration(c.Drill.MaxAgeDays)*24*time.Hour:
			add(Warn, "restore", fmt.Sprintf("the last rehearsed restore was %s ago; the declaration allows %d days", age(now.Sub(last.Time)), c.Drill.MaxAgeDays), "run `server-durability drill`, or put it on a monthly timer")
		default:
			add(OK, "restore", "a restore was rehearsed "+age(now.Sub(last.Time))+" ago and every store opened", "")
		}
	}

	out = append(out, services(ctx, c, env)...)
	return out
}

// Covers asks the newest snapshot whether every declared store is in it. `present` limits the question to
// stores that exist; nil asks about all of them.
func Covers(ctx context.Context, c *config.Config, tool backup.Tool, present map[string]bool) []Finding {
	paths, err := tool.Paths(ctx)
	if err != nil {
		return []Finding{{Level: Fail, Subject: "backup", Message: "the newest snapshot cannot be listed: " + err.Error()}}
	}
	var out []Finding
	for _, s := range c.Stores {
		if present != nil && !present[s.Name] {
			continue
		}
		want := c.SnapshotPath(s)
		real, _ := filepath.EvalSymlinks(want)
		if paths[want] || (real != "" && paths[real]) {
			out = append(out, Finding{Level: OK, Subject: s.Name, Message: "in the newest snapshot: " + want})
			continue
		}
		out = append(out, Finding{Level: Fail, Subject: s.Name, Message: "not in the newest snapshot: " + want,
			Fix: "add this path to what the backup takes"})
	}
	return out
}

// services checks what must come back after a restart: declared workers, long-running transient units,
// and calendar timers that skip a run when the machine was off.
func services(ctx context.Context, c *config.Config, env Env) []Finding {
	var out []Finding
	if _, err := env.Run(ctx, "systemctl", "--version"); err != nil {
		if len(c.Workers) > 0 {
			out = append(out, Finding{Level: Note, Subject: "services", Message: "systemd is not here: workers are not checked"})
		}
		return out
	}
	for _, w := range c.Workers {
		args := []string{"is-enabled", w.Unit}
		if w.User {
			args = append([]string{"--user"}, args...)
		}
		state, _ := env.Run(ctx, "systemctl", args...)
		state = strings.TrimSpace(state)
		switch state {
		case "enabled", "enabled-runtime", "static", "alias", "indirect", "generated":
			out = append(out, Finding{Level: OK, Subject: w.Unit, Message: "starts by itself after a restart (" + state + ")"})
		case "":
			out = append(out, Finding{Level: Fail, Subject: w.Unit, Message: "systemd does not know this unit", Fix: "correct the unit's name in the declaration"})
		default:
			out = append(out, Finding{Level: Fail, Subject: w.Unit, Message: "does not start after a restart (" + state + ")", Fix: "systemctl enable it"})
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
				out = append(out, Finding{Level: Warn, Subject: p["Id"], Message: "a transient " + who + " unit that has been running for over an hour: it will not exist after a restart",
					Fix: "give long work a unit file that is enabled until the work is done"})
			}
		}
		timers, err := env.Run(ctx, "systemctl", append(append([]string{}, scope...), "show", "--type=timer", "-p", "Id", "-p", "Persistent", "-p", "TimersCalendar", "-p", "UnitFileState", "*")...)
		if err != nil || len(scope) == 0 { // the system's own timers are the distribution's business
			continue
		}
		for _, block := range strings.Split(timers, "\n\n") {
			p := props(block)
			if p["TimersCalendar"] != "" && p["Persistent"] == "no" && p["UnitFileState"] == "enabled" {
				out = append(out, Finding{Level: Warn, Subject: p["Id"], Message: "a calendar timer without Persistent=true: a run that falls while the machine is off is skipped",
					Fix: "add Persistent=true to its [Timer] section"})
			}
		}
	}
	return out
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

// Worst is the gravest level among findings.
func Worst(findings []Finding) string {
	rank := map[string]int{Fail: 3, Warn: 2, Note: 1, OK: 0}
	worst := OK
	for _, f := range findings {
		if rank[f.Level] > rank[worst] {
			worst = f.Level
		}
	}
	return worst
}
