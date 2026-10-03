// Copyright 2026 Yauhen Bichel
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, text string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestTheExampleLoadsAndPathsAreExpanded(t *testing.T) {
	c, err := Load(write(t, Example))
	if err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	if len(c.Stores) != 2 || c.Stores[0].Path != filepath.Join(home, "app/data/app.db") || c.Stores[1].Kind != "directory" {
		t.Fatalf("stores: %+v", c.Stores)
	}
	if c.SnapshotPath(c.Stores[0]) != filepath.Join(home, "backups/sqlite/app.db") || c.SnapshotPath(c.Stores[1]) != c.Stores[1].Path {
		t.Fatalf("a database is looked for as its staged copy, a directory as itself: %s", c.SnapshotPath(c.Stores[0]))
	}
	if c.Backup.MaxAgeHours != 30 || c.Drill.MaxAgeDays != 35 || len(c.Copies) != 1 || !c.Workers[0].User {
		t.Fatalf("defaults and lists: %+v", c)
	}
}

func TestEveryProblemIsNamedAtOnce(t *testing.T) {
	_, err := Load(write(t, `
colour = "blue"
[[store]]
name = "a"
path = "/x"
kind = "postgres"
[[store]]
name = "a"
path = "/y"
[backup]
tool = "tar"
[[copy]]
name = "cloud"
where = "somewhere"
`))
	if err == nil {
		t.Fatal("this declaration must be refused")
	}
	for _, want := range []string{`unknown key "colour"`, `kind is sqlite, file or directory, not "postgres"`, `two stores are named "a"`,
		`backup tool "tar" is not supported yet`, "[backup] needs a repository", `not "somewhere"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not say %q: %v", want, err)
		}
	}
	if _, err := Load(write(t, "")); err == nil || !strings.Contains(err.Error(), "no [[store]]") {
		t.Fatalf("an empty declaration: %v", err)
	}
}
