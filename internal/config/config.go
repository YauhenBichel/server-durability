// Copyright 2026 Yauhen Bichel
// SPDX-License-Identifier: Apache-2.0

// Package config reads the config file: the data to protect, where its backup is, where the backup's other
// copies are, and which services must start again after a reboot. Every command reads the same file.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// Data is a file, directory or database the machine must not lose.
type Data struct {
	Name string `toml:"name" json:"name"`
	Path string `toml:"path" json:"path"`
	Kind string `toml:"kind" json:"kind"` // sqlite | file | directory
}

// Backup says which tool holds the snapshots and where.
type Backup struct {
	Tool         string `toml:"tool"`          // restic
	Repository   string `toml:"repository"`    // a path, or anything the tool accepts
	PasswordFile string `toml:"password_file"` // optional: the tool's own environment is used otherwise
	Binary       string `toml:"binary"`        // optional: the tool's command, when it is not on PATH
	MaxAgeHours  int    `toml:"max_age_hours"` // the latest snapshot must be newer than this (default 30)
}

// BackupCopy is another place where the backup repository is kept.
type BackupCopy struct {
	Name     string `toml:"name"`
	Location string `toml:"location"` // same-disk | same-site | off-site
}

// Service is a systemd unit that must start again after a reboot.
type Service struct {
	Unit string `toml:"unit"`
	User bool   `toml:"user"` // a unit of the user's own systemd manager
}

// Config is the whole config file.
type Config struct {
	Data     []Data `toml:"data"`
	DBCopies struct {
		Dir string `toml:"dir"` // where consistent copies of the live databases are written; back this directory up
	} `toml:"db_copies"`
	Backup       Backup       `toml:"backup"`
	BackupCopies []BackupCopy `toml:"backup_copy"`
	Services     []Service    `toml:"service"`
	RestoreTest  struct {
		MaxAgeDays int `toml:"max_age_days"` // the last restore test must be newer than this (default 35)
	} `toml:"restore_test"`
	StateDir string `toml:"state_dir"` // where the last restore test's report is saved (default ~/.local/state/server-durability)
}

// Example is what `init` prints.
const Example = `# server-durability config file. Every command reads it.

[[data]]                      # a file, directory or database this machine must not lose
name = "app"
path = "~/app/data/app.db"
kind = "sqlite"               # sqlite | file | directory

[[data]]
name = "uploads"
path = "~/app/uploads"
kind = "directory"

[db_copies]                   # consistent copies of live SQLite databases are written here; back this directory up
dir = "~/backups/sqlite"

[backup]
tool = "restic"
repository = "~/backups/restic"
password_file = "~/.config/restic/password"
max_age_hours = 30            # the latest snapshot must be newer than this

[[backup_copy]]               # every other place where the backup repository is kept
name = "laptop mirror"
location = "same-site"        # same-disk | same-site | off-site

[[service]]                   # systemd units that must start again after a reboot
unit = "app.service"
user = true                   # a user unit (systemctl --user)

[restore_test]
max_age_days = 35             # the backup must have been restore-tested more recently than this
`

// Default is the default path of the config file.
func Default() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "server-durability", "config.toml")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "server-durability", "config.toml")
}

// Expand turns a leading ~ into the home directory.
func Expand(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(path, "~"))
		}
	}
	return path
}

// Load reads and validates a config file. It reports every problem at once, not only the first.
func Load(path string) (*Config, error) {
	var c Config
	meta, err := toml.DecodeFile(path, &c)
	if err != nil {
		return nil, err
	}
	var problems []string
	for _, key := range meta.Undecoded() {
		problems = append(problems, fmt.Sprintf("unknown key %q", key.String()))
	}
	seen := map[string]bool{}
	for i := range c.Data {
		s := &c.Data[i]
		s.Path = Expand(s.Path)
		if s.Kind == "" {
			s.Kind = "file"
		}
		switch {
		case s.Name == "" || s.Path == "":
			problems = append(problems, fmt.Sprintf("[[data]] entry %d needs a name and a path", i+1))
		case seen[s.Name]:
			problems = append(problems, fmt.Sprintf("two [[data]] entries are named %q", s.Name))
		case s.Kind != "sqlite" && s.Kind != "file" && s.Kind != "directory":
			problems = append(problems, fmt.Sprintf("data %q: kind must be sqlite, file or directory, not %q", s.Name, s.Kind))
		}
		seen[s.Name] = true
	}
	if len(c.Data) == 0 {
		problems = append(problems, "no [[data]] entry: list the files and databases this machine must not lose")
	}
	c.DBCopies.Dir = Expand(c.DBCopies.Dir)
	c.Backup.Repository = Expand(c.Backup.Repository)
	c.Backup.PasswordFile = Expand(c.Backup.PasswordFile)
	if c.Backup.Tool != "" && c.Backup.Tool != "restic" {
		problems = append(problems, fmt.Sprintf("backup tool %q is not supported yet: restic is", c.Backup.Tool))
	}
	if c.Backup.Tool != "" && c.Backup.Repository == "" {
		problems = append(problems, "[backup] needs a repository")
	}
	if c.Backup.MaxAgeHours == 0 {
		c.Backup.MaxAgeHours = 30
	}
	for _, cp := range c.BackupCopies {
		if cp.Location != "same-disk" && cp.Location != "same-site" && cp.Location != "off-site" {
			problems = append(problems, fmt.Sprintf("backup_copy %q: location must be same-disk, same-site or off-site, not %q", cp.Name, cp.Location))
		}
	}
	for _, w := range c.Services {
		if w.Unit == "" {
			problems = append(problems, "a [[service]] needs a unit")
		}
	}
	if c.RestoreTest.MaxAgeDays == 0 {
		c.RestoreTest.MaxAgeDays = 35
	}
	if c.StateDir == "" {
		home, _ := os.UserHomeDir()
		c.StateDir = filepath.Join(home, ".local", "state", "server-durability")
	}
	c.StateDir = Expand(c.StateDir)
	if len(problems) > 0 {
		return nil, errors.New(strings.Join(problems, "; "))
	}
	return &c, nil
}

// CopyName is the file name of a database's consistent copy in the db_copies directory.
func (c *Config) CopyName(s Data) string {
	return s.Name + ".db"
}

// SnapshotPath is the path to look for in a snapshot: the consistent copy of a database when [db_copies] is
// set (the live file of a database is not a reliable copy), the entry's own path otherwise.
func (c *Config) SnapshotPath(s Data) string {
	if s.Kind == "sqlite" && c.DBCopies.Dir != "" {
		return filepath.Join(c.DBCopies.Dir, c.CopyName(s))
	}
	return s.Path
}
