// Copyright 2026 Yauhen Bichel
// SPDX-License-Identifier: Apache-2.0

// Package config reads the declaration of a machine: its stores, where their backup is, where else that
// backup exists, and which services must come back after a restart. Every command reads the same file.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// Store is something the machine must not lose.
type Store struct {
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
	MaxAgeHours  int    `toml:"max_age_hours"` // the newest snapshot must be younger than this (default 30)
}

// Copy is another place the backup repository exists.
type Copy struct {
	Name  string `toml:"name"`
	Where string `toml:"where"` // same-disk | same-site | off-site
}

// Worker is a service that must start again by itself after a restart.
type Worker struct {
	Unit string `toml:"unit"`
	User bool   `toml:"user"` // a unit of the user's own systemd manager
}

// Config is the whole declaration.
type Config struct {
	Stores []Store `toml:"store"`
	Stage  struct {
		Dir string `toml:"dir"` // where consistent copies of the live databases are written, inside the backup's paths
	} `toml:"stage"`
	Backup  Backup   `toml:"backup"`
	Copies  []Copy   `toml:"copy"`
	Workers []Worker `toml:"worker"`
	Drill   struct {
		MaxAgeDays int `toml:"max_age_days"` // a rehearsed restore must be younger than this (default 35)
	} `toml:"drill"`
	StateDir string `toml:"state_dir"` // where the last drill's result is kept (default ~/.local/state/server-durability)
}

// Example is what `init` prints.
const Example = `# server-durability: the declaration of this machine. Every command reads it.

[[store]]                     # something this machine must not lose
name = "app"
path = "~/app/data/app.db"
kind = "sqlite"               # sqlite | file | directory

[[store]]
name = "uploads"
path = "~/app/uploads"
kind = "directory"

[stage]                       # consistent copies of the live databases go here; back this directory up
dir = "~/backups/sqlite"

[backup]
tool = "restic"
repository = "~/backups/restic"
password_file = "~/.config/restic/password"
max_age_hours = 30            # the newest snapshot must be younger

[[copy]]                      # every other place the repository exists
name = "laptop mirror"
where = "same-site"           # same-disk | same-site | off-site

[[worker]]                    # services that must start again by themselves after a restart
unit = "app.service"
user = true                   # a unit of your own systemd manager (systemctl --user)

[drill]
max_age_days = 35             # a restore must have been rehearsed more recently
`

// Default is where the declaration is looked for.
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

// Load reads and checks a declaration. Every problem is named at once, not only the first.
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
	for i := range c.Stores {
		s := &c.Stores[i]
		s.Path = Expand(s.Path)
		if s.Kind == "" {
			s.Kind = "file"
		}
		switch {
		case s.Name == "" || s.Path == "":
			problems = append(problems, fmt.Sprintf("store %d needs a name and a path", i+1))
		case seen[s.Name]:
			problems = append(problems, fmt.Sprintf("two stores are named %q", s.Name))
		case s.Kind != "sqlite" && s.Kind != "file" && s.Kind != "directory":
			problems = append(problems, fmt.Sprintf("store %q: kind is sqlite, file or directory, not %q", s.Name, s.Kind))
		}
		seen[s.Name] = true
	}
	if len(c.Stores) == 0 {
		problems = append(problems, "no [[store]] is declared: name what this machine must not lose")
	}
	c.Stage.Dir = Expand(c.Stage.Dir)
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
	for _, cp := range c.Copies {
		if cp.Where != "same-disk" && cp.Where != "same-site" && cp.Where != "off-site" {
			problems = append(problems, fmt.Sprintf("copy %q: where is same-disk, same-site or off-site, not %q", cp.Name, cp.Where))
		}
	}
	for _, w := range c.Workers {
		if w.Unit == "" {
			problems = append(problems, "a [[worker]] needs a unit")
		}
	}
	if c.Drill.MaxAgeDays == 0 {
		c.Drill.MaxAgeDays = 35
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

// StagedName is the file name a store's consistent copy has in the stage directory.
func (c *Config) StagedName(s Store) string {
	return s.Name + ".db"
}

// SnapshotPath is the path to look for in a snapshot: the staged copy of a database when staging is
// declared (the live file of a database is not a trustworthy copy), the store's own path otherwise.
func (c *Config) SnapshotPath(s Store) string {
	if s.Kind == "sqlite" && c.Stage.Dir != "" {
		return filepath.Join(c.Stage.Dir, c.StagedName(s))
	}
	return s.Path
}
