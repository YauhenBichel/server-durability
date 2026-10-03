# server-durability

**Can this server lose data? Check the data, the backup and the restore.**

A single server usually has a database or two and a nightly backup, and everyone assumes it is fine. Often
it is not:

- the backup is on the same disk as the data;
- the backup copies a live database file, and the copy is missing the latest commits;
- a new database was added last month and is not in the backup at all;
- nobody has ever tried to restore from the backup.

`server-durability` reads a short config file that lists your data, then checks all of this:

```console
$ server-durability check
ERROR backup         the backup repository is on the same disk (nvme0n1) as the data (orders, uploads): if this disk fails, the data and its only backup are both lost
                     fix: keep a copy of the backup on another disk or another machine and add it as a [[backup_copy]]
ERROR orders         missing from the latest snapshot: /srv/db-copies/orders.db
                     fix: add this path to the backup job
WARN  orders         no database copy yet in /srv/db-copies
                     fix: run `server-durability copy-db` before each backup
WARN  restore test   the backup has never been restore-tested: an untested backup may not work
                     fix: run `server-durability restore-test`
INFO  disk nvme0n1   has a volatile write cache (used by: orders, uploads): data that was not fsynced is lost on power loss. Fsynced data is safe if the drive honours flush commands
OK    orders         SQLite database, WAL mode
OK    backup         the latest snapshot (22515009) is 0 minutes old
OK    uploads        in the latest snapshot: /srv/app/uploads

2 errors, 2 warnings, 3 ok, 1 info
```

Every line says what is wrong and how to fix it. After a database copy, a full backup and a restore test,
on the same server (the shared disk stays an error: this demo runs in one temporary directory):

```console
$ server-durability copy-db
OK    orders         /srv/db-copies/orders.db (24576 bytes)

$ server-durability verify
OK    orders         in the latest snapshot: /srv/db-copies/orders.db
OK    uploads        in the latest snapshot: /srv/app/uploads

2 ok

$ server-durability restore-test
OK    orders         restored, integrity check ok, 1 table, 1000 rows (live database: 1000 rows)
OK    uploads        restored, 1 file or directory at the top level

restore test took 2.1 s; report saved to /srv/state/restore-test.json

$ server-durability check
ERROR backup         the backup repository is on the same disk (nvme0n1) as the data (orders, uploads): if this disk fails, the data and its only backup are both lost
                     fix: keep a copy of the backup on another disk or another machine and add it as a [[backup_copy]]
INFO  disk nvme0n1   has a volatile write cache (used by: orders, uploads): data that was not fsynced is lost on power loss. Fsynced data is safe if the drive honours flush commands
OK    orders         SQLite database, WAL mode
OK    backup         the latest snapshot (0000dbae) is 0 minutes old
OK    orders         in the latest snapshot: /srv/db-copies/orders.db
OK    uploads        in the latest snapshot: /srv/app/uploads
OK    restore test   last restore test: 0 minutes ago, all data restored and opened

1 error, 5 ok, 1 info
```

It is one static binary written in Go. It is read-only, except for the database copies you ask for and the
restore test report.

## Install

```bash
go install github.com/YauhenBichel/server-durability/cmd/server-durability@latest
```

Linux and macOS. The disk and file system checks need Linux. The backup tool is [restic](https://restic.net)
for now, and it must be installed.

## Quick start

```bash
mkdir -p ~/.config/server-durability
server-durability init > ~/.config/server-durability/config.toml     # then edit it: your data, your backup
server-durability check
```

The config file:

```toml
[[data]]                      # a file, directory or database this server must not lose
name = "orders"
path = "~/app/data/app.db"
kind = "sqlite"               # sqlite | file | directory

[db_copies]                   # consistent copies of live SQLite databases are written here; back this directory up
dir = "~/backups/sqlite"

[backup]
tool = "restic"
repository = "~/backups/restic"
password_file = "~/.config/restic/password"

[[backup_copy]]               # every other place where the backup repository is kept
name = "laptop mirror"
location = "same-site"        # same-disk | same-site | off-site

[[service]]                   # systemd units that must start again after a reboot
unit = "app.service"
user = true
```

To try it without touching your own files, run [`examples/demo.sh`](examples/demo.sh). It creates a test
server in a temporary directory and prints the two reports above.

## Commands

| Command | What it does | What it writes |
|---|---|---|
| `check` | Runs all checks (listed below) and prints a report. Exit status 1 if there is an error (`-strict`: also for a warning) | nothing |
| `copy-db` | Makes a consistent copy of each live SQLite database, so the backup uses the copy and not the file the service is writing to | the copies |
| `verify` | Verifies that the latest backup snapshot contains all the data in the config file | nothing |
| `restore-test` | Tests the backup by restoring from it. See below | a small report |
| `init` | Prints an example config file | nothing |

All commands accept `-json`. Exit status: 0 ok, 1 errors found or a step failed, 2 wrong command line,
3 the config file cannot be read.

### `restore-test`: test the backup

A backup that was never restored may not work. `restore-test` tries it, without touching the real data:

1. It restores all configured data from the latest snapshot into a **temporary directory**, never over the
   real files.
2. It opens each restored database and runs the SQLite integrity check.
3. It counts the rows in the restored database and shows the count next to the live database's count.
4. It deletes the temporary directory.
5. It saves a report (date, each item, passed or failed). `check` reads the report and warns when the last
   test is too old, or failed.

```console
$ server-durability restore-test
OK    orders         restored, integrity check ok, 1 table, 1000 rows (live database: 1000 rows)
OK    uploads        restored, 1 file or directory at the top level

restore test took 2.1 s; report saved to /srv/state/restore-test.json
```

### In a backup script

```bash
server-durability copy-db && restic backup ~/backups/sqlite ~/app/uploads && server-durability verify
```

Run `server-durability restore-test` from a monthly cron job or systemd timer, and `server-durability check`
from your monitoring.

## What `check` checks

| Check | Error or warning when |
|---|---|
| each data path exists and is the right kind | the path is missing; a "database" file is not a SQLite database |
| file system | the data is on tmpfs (RAM); the file system is mounted with `data=writeback` or without barriers |
| disks | the backup repository is on the same physical disk as the data (it looks through partitions and LVM) |
| latest snapshot | there is none; it is older than `max_age_hours` |
| backup contents | a configured data path is missing from the latest snapshot |
| backup copies | the repository is the only copy; there is no off-site copy |
| database copies | a live SQLite database is backed up as a plain file; its consistent copy is out of date |
| restore test | never run; the last one failed; older than `max_age_days` |
| services | a configured service is not enabled; a transient unit (`systemd-run`) has been running for over an hour; a calendar timer has no `Persistent=true` |

A disk with a volatile write cache is reported as info, not as an error: fsynced data is safe on it if the
drive honours flush commands.

## Go library

Two packages can be used without the command-line tool.

```go
import (
    "github.com/YauhenBichel/server-durability/durable"
    "github.com/YauhenBichel/server-durability/sqlitedb"
)

durable.RemoveTempFiles(dir, time.Hour)                  // on startup: temp files left by killed writers
err := durable.AppendLine(logPath, line)                 // append + fsync before it returns
err  = durable.WriteFileAtomic(statePath, data, 0o644)   // atomic write: old content or new, never a partial file

err  = sqlitedb.Backup(livePath, copyPath)               // consistent copy while the service is writing
err  = sqlitedb.Checkpoint(path)                         // WAL checkpoint, e.g. before a planned shutdown
```

`WriteFileAtomic` writes a temporary file, calls fsync, renames it over the target and fsyncs the
directory. Its test kills the writing process 150 times at random moments and requires the file to be
complete every time.

## Why these checks: the experiments

The checks are based on measurements, published with raw results:
[yserver-durability-experiments](https://huggingface.co/datasets/YauhenBichel/yserver-durability-experiments).

- Plain file copy of a SQLite database while it is being written: 49 of 50 copies opened and passed the
  integrity check, but the median copy was missing 60,000 committed rows. The SQLite backup API lost none in
  100 copies.
- A file written in place with fsync after every block: partially written in 493 of 500 kills. Temporary
  file, fsync, rename, fsync of the directory: 0 in 1,000.
- Both a Go and a Rust writer left temporary files behind when killed. That is why `RemoveTempFiles` exists.

## Limitations

- **It cannot see how an application writes.** An application that appends without fsync, or replaces a
  file without fsyncing the directory, cannot be detected from outside. The library is for writing
  correctly; `check` covers everything around the write.
- **Killing a process is not a power loss.** The tests kill processes. What a drive does on power loss is a
  separate question; a volatile write cache is reported so you know to ask it.
- **restic only**, for now. The backup tool is behind a small interface (latest snapshot, list its paths,
  restore these paths), so borg and kopia can be added.
- **SQLite only**, among databases. PostgreSQL and others have their own tools for consistent dumps.
- **The embedded SQLite is a Go translation.** `modernc.org/sqlite` is SQLite compiled to Go, which makes a
  single static binary possible; it is not the upstream C build. `restore-test` opens every copy for that
  reason too.
- **Only configured data is checked.** Data that is not in the config file is not checked.

## Roadmap

1. Persistent jobs: a command that is retried until it exits 0, and continues after a reboot.
2. Graceful shutdown: stop services, wait for in-flight requests, checkpoint, save a report, power off.
3. borg and kopia support. An MCP server and a small web page, as in [llm-hops](https://github.com/YauhenBichel/llm-hops).
4. Tests with [LazyFS](https://github.com/dsrhaslab/lazyfs), which drops data that was not fsynced, to
   simulate a power loss.

## Contributing

```bash
make lint test        # go vet, gofmt, go test -race; the restic tests are skipped if restic is not installed
```

Issues are welcome: a server it reads wrongly (attach `server-durability check -json`), a check it should have.

## License

Apache-2.0.
