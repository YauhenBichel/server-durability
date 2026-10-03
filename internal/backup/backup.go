// Copyright 2026 Yauhen Bichel
// SPDX-License-Identifier: Apache-2.0

// Package backup asks a backup tool three questions about its newest snapshot: when it was taken, which
// paths are in it, and to give some of them back. The tool itself stays what it is; restic is the first.
package backup

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/YauhenBichel/server-durability/internal/config"
)

// Snapshot is the newest snapshot of a repository.
type Snapshot struct {
	ID   string    `json:"id"`
	Time time.Time `json:"time"`
}

// Tool is what the rest of the program needs from a backup tool.
type Tool interface {
	Latest(ctx context.Context) (Snapshot, error)
	Paths(ctx context.Context) (map[string]bool, error)               // every path in the newest snapshot
	Restore(ctx context.Context, paths []string, target string) error // the paths, under target, as they were
}

// ErrNoSnapshot means the repository opens and holds nothing yet.
var ErrNoSnapshot = errors.New("the repository has no snapshot")

// New gives the tool a declaration names; nil when no backup is declared.
func New(b config.Backup) Tool {
	if b.Tool != "restic" {
		return nil
	}
	bin := b.Binary
	if bin == "" {
		bin = "restic"
	}
	return &Restic{Binary: bin, Repository: b.Repository, PasswordFile: b.PasswordFile}
}

// Restic runs the restic command. Its password comes from PasswordFile, or from restic's own environment
// (RESTIC_PASSWORD_FILE, RESTIC_PASSWORD_COMMAND) when that is empty. It is never read or printed here.
type Restic struct {
	Binary, Repository, PasswordFile string
}

func (r *Restic) run(ctx context.Context, args ...string) ([]byte, error) {
	full := []string{"--repo", r.Repository}
	if r.PasswordFile != "" {
		full = append(full, "--password-file", r.PasswordFile)
	}
	cmd := exec.CommandContext(ctx, r.Binary, append(full, args...)...)
	cmd.Env = os.Environ()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, fmt.Errorf("the command %q is not installed or not on PATH", r.Binary)
		}
		first := strings.SplitN(strings.TrimSpace(stderr.String()), "\n", 2)[0]
		return nil, fmt.Errorf("restic %s: %s", args[0], first)
	}
	return out, nil
}

// Latest is the newest snapshot.
func (r *Restic) Latest(ctx context.Context) (Snapshot, error) {
	out, err := r.run(ctx, "snapshots", "--json", "--latest", "1")
	if err != nil {
		return Snapshot{}, err
	}
	var all []struct {
		ID   string    `json:"short_id"`
		Time time.Time `json:"time"`
	}
	if err := json.Unmarshal(out, &all); err != nil {
		return Snapshot{}, fmt.Errorf("restic snapshots: %w", err)
	}
	var newest Snapshot
	for _, s := range all { // --latest 1 is one a host and path set: take the newest of those
		if s.Time.After(newest.Time) {
			newest = Snapshot{ID: s.ID, Time: s.Time}
		}
	}
	if newest.ID == "" {
		return Snapshot{}, ErrNoSnapshot
	}
	return newest, nil
}

// Paths lists the newest snapshot.
func (r *Restic) Paths(ctx context.Context) (map[string]bool, error) {
	out, err := r.run(ctx, "ls", "latest", "--json")
	if err != nil {
		return nil, err
	}
	paths := map[string]bool{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var node struct {
			Path string `json:"path"`
			Type string `json:"struct_type"`
		}
		if json.Unmarshal(sc.Bytes(), &node) == nil && node.Type == "node" && node.Path != "" {
			paths[node.Path] = true
		}
	}
	return paths, sc.Err()
}

// Restore writes the named paths of the newest snapshot under target.
func (r *Restic) Restore(ctx context.Context, paths []string, target string) error {
	args := []string{"restore", "latest", "--target", target}
	for _, p := range paths {
		args = append(args, "--include", p)
	}
	_, err := r.run(ctx, args...)
	return err
}
