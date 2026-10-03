// Copyright 2026 Yauhen Bichel
// SPDX-License-Identifier: Apache-2.0

// server-durability: what would this machine lose, and when?
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/YauhenBichel/server-durability/durable"
	"github.com/YauhenBichel/server-durability/internal/audit"
	"github.com/YauhenBichel/server-durability/internal/backup"
	"github.com/YauhenBichel/server-durability/internal/config"
	"github.com/YauhenBichel/server-durability/internal/drill"
	"github.com/YauhenBichel/server-durability/sqlitedb"
)

var version = "dev"

const usage = `server-durability: what would this machine lose, and when?

  server-durability [-config FILE] [-json] COMMAND

  audit     every store, the backup, the copies, the last rehearsed restore, the services: what is in order,
            what depends on luck, what would be lost. Reads only. -strict makes a warning fail too
  stage     write consistent copies of the live SQLite databases into the stage directory (run before a backup)
  covers    is every declared store in the newest snapshot?
  drill     rehearse a restore: take every store out of the newest snapshot, open it, record the result
  init      print an example declaration
  version

The declaration is read from -config, or ~/.config/server-durability/config.toml.
Exit status: 0 in order, 1 something would be lost (or a step failed), 2 the command line was wrong,
3 the declaration could not be read.
`

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	fs := flag.NewFlagSet("server-durability", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	cfgPath := fs.String("config", config.Default(), "the declaration")
	asJSON := fs.Bool("json", false, "the answer as JSON")
	strict := fs.Bool("strict", false, "audit: a warning fails too")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	// flags may also follow the command: `audit -json`
	if err := fs.Parse(rest[1:]); err != nil || fs.NArg() > 0 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	switch rest[0] {
	case "version":
		fmt.Println("server-durability", version)
		return 0
	case "init":
		fmt.Print(config.Example)
		return 0
	case "audit", "stage", "covers", "drill":
	default:
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	c, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "server-durability: %s: %v\n", *cfgPath, err)
		if os.IsNotExist(err) {
			fmt.Fprintln(os.Stderr, "start with: server-durability init > "+*cfgPath)
		}
		return 3
	}
	ctx := context.Background()
	tool := backup.New(c.Backup)
	emit := func(v any) {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", " ")
		enc.Encode(v)
	}
	switch rest[0] {
	case "audit":
		findings := audit.Run(ctx, c, audit.Env{ProcRoot: "/proc", SysRoot: "/sys", Now: time.Now, Tool: tool, Run: runCommand})
		worst := audit.Worst(findings)
		if *asJSON {
			emit(map[string]any{"schema": 1, "worst": worst, "findings": findings})
		} else {
			printFindings(findings)
		}
		if worst == audit.Fail || (*strict && worst == audit.Warn) {
			return 1
		}
		return 0
	case "stage":
		return stage(c, *asJSON, emit)
	case "covers":
		if tool == nil {
			fmt.Fprintln(os.Stderr, "server-durability: no [backup] is declared")
			return 1
		}
		findings := audit.Covers(ctx, c, tool, nil)
		if *asJSON {
			emit(map[string]any{"schema": 1, "worst": audit.Worst(findings), "findings": findings})
		} else {
			printFindings(findings)
		}
		if audit.Worst(findings) == audit.Fail {
			return 1
		}
		return 0
	default: // drill
		if tool == nil {
			fmt.Fprintln(os.Stderr, "server-durability: no [backup] is declared")
			return 1
		}
		result, err := drill.Run(ctx, c, tool, time.Now())
		if *asJSON {
			emit(result)
		} else {
			for _, s := range result.Stores {
				fmt.Printf("%-5s %-14s %s\n", map[bool]string{true: "ok", false: "FAIL"}[s.OK], s.Name, s.Message)
			}
			fmt.Printf("\nthe rehearsal took %.1f s; recorded in %s\n", result.Seconds, audit.DrillFile(c))
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "server-durability:", err)
			return 1
		}
		if !result.OK {
			return 1
		}
		return 0
	}
}

func runCommand(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).Output()
	return string(out), err
}

func stage(c *config.Config, asJSON bool, emit func(any)) int {
	if c.Stage.Dir == "" {
		fmt.Fprintln(os.Stderr, "server-durability: no [stage] directory is declared")
		return 1
	}
	if err := os.MkdirAll(c.Stage.Dir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "server-durability:", err)
		return 1
	}
	durable.SweepTemp(c.Stage.Dir, time.Hour) // what killed runs left behind
	type line struct {
		Store string `json:"store"`
		OK    bool   `json:"ok"`
		Copy  string `json:"copy,omitempty"`
		Bytes int64  `json:"bytes,omitempty"`
		Error string `json:"error,omitempty"`
	}
	var lines []line
	failed := 0
	for _, s := range c.Stores {
		if s.Kind != "sqlite" {
			continue
		}
		dst := filepath.Join(c.Stage.Dir, c.StagedName(s))
		l := line{Store: s.Name, Copy: dst}
		if err := sqlitedb.Stage(s.Path, dst); err != nil {
			l.Error, l.Copy = err.Error(), ""
			failed++
		} else if info, err := os.Stat(dst); err == nil {
			l.OK, l.Bytes = true, info.Size()
		}
		lines = append(lines, l)
	}
	if asJSON {
		emit(map[string]any{"schema": 1, "staged": lines})
	} else {
		for _, l := range lines {
			if l.OK {
				fmt.Printf("ok    %-14s %s (%d bytes)\n", l.Store, l.Copy, l.Bytes)
			} else {
				fmt.Printf("FAIL  %-14s %s\n", l.Store, l.Error)
			}
		}
		if len(lines) == 0 {
			fmt.Println("no SQLite store is declared: nothing to stage")
		}
	}
	if failed > 0 {
		return 1
	}
	return 0
}

func printFindings(findings []audit.Finding) {
	rank := map[string]int{audit.Fail: 0, audit.Warn: 1, audit.Note: 2, audit.OK: 3}
	sorted := append([]audit.Finding(nil), findings...)
	sort.SliceStable(sorted, func(i, j int) bool { return rank[sorted[i].Level] < rank[sorted[j].Level] })
	label := map[string]string{audit.Fail: "FAIL", audit.Warn: "warn", audit.Note: "note", audit.OK: "ok"}
	counts := map[string]int{}
	for _, f := range sorted {
		counts[f.Level]++
		fmt.Printf("%-5s %-14s %s\n", label[f.Level], f.Subject, f.Message)
		if f.Fix != "" {
			fmt.Printf("%-5s %-14s fix: %s\n", "", "", f.Fix)
		}
	}
	var parts []string
	for _, p := range []struct{ level, words string }{{audit.Fail, "would lose data or work"}, {audit.Warn, "depend on luck"}, {audit.OK, "in order"}, {audit.Note, "to know"}} {
		if counts[p.level] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[p.level], p.words))
		}
	}
	fmt.Println("\n" + strings.Join(parts, ", "))
}
