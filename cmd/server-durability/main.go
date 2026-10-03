// Copyright 2026 Yauhen Bichel
// SPDX-License-Identifier: Apache-2.0

// server-durability: can this server lose data? Check the data, the backup and the restore.
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
	"github.com/YauhenBichel/server-durability/internal/restoretest"
	"github.com/YauhenBichel/server-durability/sqlitedb"
)

var version = "dev"

const usage = `server-durability: can this server lose data? Check the data, the backup and the restore.

  server-durability [-config FILE] [-json] COMMAND

  check         check everything and print a report: data, disks, backup, backup copies, restore test,
                services. Read-only. -strict: warnings also give exit status 1
  copy-db       make a consistent copy of each live SQLite database (run it before the backup)
  verify        verify that the latest backup snapshot contains all configured data
  restore-test  test the backup: restore everything into a temporary directory, open and check it,
                delete the directory, save a report. The real data is not touched
  init          print an example config file
  version

The config file is -config, or ~/.config/server-durability/config.toml.
Exit status: 0 ok, 1 errors found (or a step failed), 2 wrong command line, 3 the config file cannot be read.
`

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	fs := flag.NewFlagSet("server-durability", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	cfgPath := fs.String("config", config.Default(), "the config file")
	asJSON := fs.Bool("json", false, "print JSON")
	strict := fs.Bool("strict", false, "check: warnings also give exit status 1")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	// flags may also follow the command: `check -json`
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
	case "check", "copy-db", "verify", "restore-test":
	default:
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	c, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "server-durability: %s: %v\n", *cfgPath, err)
		if os.IsNotExist(err) {
			fmt.Fprintln(os.Stderr, "create it with: server-durability init > "+*cfgPath)
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
	case "check":
		findings := audit.Run(ctx, c, audit.Env{ProcRoot: "/proc", SysRoot: "/sys", Now: time.Now, Tool: tool, Run: runCommand})
		worst := audit.Worst(findings)
		if *asJSON {
			emit(map[string]any{"schema": 1, "worst": worst, "findings": findings})
		} else {
			printFindings(findings)
		}
		if worst == audit.Error || (*strict && worst == audit.Warning) {
			return 1
		}
		return 0
	case "copy-db":
		return copyDB(c, *asJSON, emit)
	case "verify":
		if tool == nil {
			fmt.Fprintln(os.Stderr, "server-durability: no [backup] section in the config file")
			return 1
		}
		findings := audit.Verify(ctx, c, tool, nil)
		if *asJSON {
			emit(map[string]any{"schema": 1, "worst": audit.Worst(findings), "findings": findings})
		} else {
			printFindings(findings)
		}
		if audit.Worst(findings) == audit.Error {
			return 1
		}
		return 0
	default: // restore-test
		if tool == nil {
			fmt.Fprintln(os.Stderr, "server-durability: no [backup] section in the config file")
			return 1
		}
		result, err := restoretest.Run(ctx, c, tool, time.Now())
		if *asJSON {
			emit(result)
		} else {
			for _, s := range result.Items {
				fmt.Printf("%-5s %-14s %s\n", map[bool]string{true: "OK", false: "ERROR"}[s.OK], s.Name, s.Message)
			}
			fmt.Printf("\nrestore test took %.1f s; report saved to %s\n", result.Seconds, audit.RestoreTestFile(c))
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

func copyDB(c *config.Config, asJSON bool, emit func(any)) int {
	if c.DBCopies.Dir == "" {
		fmt.Fprintln(os.Stderr, "server-durability: no [db_copies] dir in the config file")
		return 1
	}
	if err := os.MkdirAll(c.DBCopies.Dir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "server-durability:", err)
		return 1
	}
	durable.RemoveTempFiles(c.DBCopies.Dir, time.Hour) // left by runs that were killed
	type line struct {
		Name  string `json:"name"`
		OK    bool   `json:"ok"`
		Copy  string `json:"copy,omitempty"`
		Bytes int64  `json:"bytes,omitempty"`
		Error string `json:"error,omitempty"`
	}
	var lines []line
	failed := 0
	for _, s := range c.Data {
		if s.Kind != "sqlite" {
			continue
		}
		dst := filepath.Join(c.DBCopies.Dir, c.CopyName(s))
		l := line{Name: s.Name, Copy: dst}
		if err := sqlitedb.Backup(s.Path, dst); err != nil {
			l.Error, l.Copy = err.Error(), ""
			failed++
		} else if info, err := os.Stat(dst); err == nil {
			l.OK, l.Bytes = true, info.Size()
		}
		lines = append(lines, l)
	}
	if asJSON {
		emit(map[string]any{"schema": 1, "copies": lines})
	} else {
		for _, l := range lines {
			if l.OK {
				fmt.Printf("OK    %-14s %s (%d bytes)\n", l.Name, l.Copy, l.Bytes)
			} else {
				fmt.Printf("ERROR %-14s %s\n", l.Name, l.Error)
			}
		}
		if len(lines) == 0 {
			fmt.Println("no SQLite database in the config file: nothing to copy")
		}
	}
	if failed > 0 {
		return 1
	}
	return 0
}

func printFindings(findings []audit.Finding) {
	rank := map[string]int{audit.Error: 0, audit.Warning: 1, audit.Info: 2, audit.OK: 3}
	sorted := append([]audit.Finding(nil), findings...)
	sort.SliceStable(sorted, func(i, j int) bool { return rank[sorted[i].Level] < rank[sorted[j].Level] })
	label := map[string]string{audit.Error: "ERROR", audit.Warning: "WARN", audit.Info: "INFO", audit.OK: "OK"}
	counts := map[string]int{}
	for _, f := range sorted {
		counts[f.Level]++
		fmt.Printf("%-5s %-14s %s\n", label[f.Level], f.Subject, f.Message)
		if f.Fix != "" {
			fmt.Printf("%-5s %-14s fix: %s\n", "", "", f.Fix)
		}
	}
	var parts []string
	for _, p := range []struct{ level, one, many string }{{audit.Error, "error", "errors"}, {audit.Warning, "warning", "warnings"}, {audit.OK, "ok", "ok"}, {audit.Info, "info", "info"}} {
		if n := counts[p.level]; n == 1 {
			parts = append(parts, "1 "+p.one)
		} else if n > 1 {
			parts = append(parts, fmt.Sprintf("%d %s", n, p.many))
		}
	}
	fmt.Println("\n" + strings.Join(parts, ", "))
}
