// Copyright 2026 Yauhen Bichel
// SPDX-License-Identifier: Apache-2.0

// Package durable holds the few file writes a program needs to keep what it was told across a crash or a
// power cut: a line appended and on the disk before the call returns, a file replaced so that a reader finds
// the old content or the new and never half of either, and the clean-up that no language does after a kill.
//
// The order of the system calls is the whole point. For a replace: write a temporary file in the same
// directory, sync it, rename it over the target, then sync the directory, because the rename itself is not
// on the disk until the directory is. The third call is the one most often forgotten.
//
// A process killed in the middle leaves its temporary file behind. Nothing runs after SIGKILL, so SweepTemp
// exists: call it when the program starts.
package durable

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// TempMark is in the name of every temporary file this package makes, so SweepTemp can find them.
const TempMark = ".durable-tmp-"

// SyncDir makes a rename, a creation or a removal inside dir reach the disk.
func SyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		d.Close()
		return fmt.Errorf("sync directory %s: %w", dir, err)
	}
	return d.Close()
}

// AppendLine appends line and a newline to path and returns only after the data is on the disk.
// A file it had to create is made durable too: its directory is synced.
func AppendLine(path, line string) error {
	_, statErr := os.Lstat(path)
	created := errors.Is(statErr, fs.ErrNotExist)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(line + "\n"); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("sync %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	if created {
		return SyncDir(filepath.Dir(path))
	}
	return nil
}

// ReplaceFile puts data at path atomically and durably. After a crash at any moment path holds either what
// it held before or data, whole. A reader never sees a partial file.
func ReplaceFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+TempMark+"*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	renamed := false
	defer func() {
		if !renamed {
			os.Remove(name) // a half file never stays, when we live to remove it
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	renamed = true
	return SyncDir(dir)
}

// SweepTemp removes the temporary files that killed writers left in dir, when they are older than olderThan
// (so a writer at work now is not disturbed). It returns the names it removed.
func SweepTemp(dir string, olderThan time.Duration) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var removed []string
	for _, e := range entries {
		if e.IsDir() || !strings.Contains(e.Name(), TempMark) {
			continue
		}
		info, err := e.Info()
		if err != nil || time.Since(info.ModTime()) < olderThan {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err == nil {
			removed = append(removed, e.Name())
		}
	}
	if len(removed) > 0 {
		return removed, SyncDir(dir)
	}
	return removed, nil
}
