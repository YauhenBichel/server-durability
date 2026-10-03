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
	if len(c.Data) != 2 || c.Data[0].Path != filepath.Join(home, "app/data/app.db") || c.Data[1].Kind != "directory" {
		t.Fatalf("data: %+v", c.Data)
	}
	if c.SnapshotPath(c.Data[0]) != filepath.Join(home, "backups/sqlite/app.db") || c.SnapshotPath(c.Data[1]) != c.Data[1].Path {
		t.Fatalf("a database is looked for as its consistent copy, a directory as itself: %s", c.SnapshotPath(c.Data[0]))
	}
	if !strings.HasPrefix(c.Backup.PasswordFile, home) {
		t.Fatalf("~ in password_file is not expanded: %s", c.Backup.PasswordFile)
	}
	if c.Backup.MaxAgeHours != 30 || c.RestoreTest.MaxAgeDays != 35 || len(c.BackupCopies) != 1 || !c.Services[0].User {
		t.Fatalf("defaults and lists: %+v", c)
	}
}

func TestTildeInTheBackupBinaryIsExpanded(t *testing.T) {
	c, err := Load(write(t, "[[data]]\nname = \"a\"\npath = \"/x\"\n[backup]\ntool = \"restic\"\nrepository = \"/r\"\nbinary = \"~/.local/bin/restic\"\n"))
	home, _ := os.UserHomeDir()
	if err != nil || c.Backup.Binary != filepath.Join(home, ".local/bin/restic") {
		t.Fatalf("binary = %q, %v", c.Backup.Binary, err)
	}
}

func TestEveryProblemIsNamedAtOnce(t *testing.T) {
	_, err := Load(write(t, `
colour = "blue"
[[data]]
name = "a"
path = "/x"
kind = "postgres"
[[data]]
name = "a"
path = "/y"
[backup]
tool = "tar"
[[backup_copy]]
name = "cloud"
location = "somewhere"
`))
	if err == nil {
		t.Fatal("this config file must be rejected")
	}
	for _, want := range []string{`unknown key "colour"`, `kind must be sqlite, file or directory, not "postgres"`, `two [[data]] entries are named "a"`,
		`backup tool "tar" is not supported yet`, "[backup] needs a repository", `not "somewhere"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not say %q: %v", want, err)
		}
	}
	if _, err := Load(write(t, "")); err == nil || !strings.Contains(err.Error(), "no [[data]] entry") {
		t.Fatalf("an empty config file: %v", err)
	}
}
