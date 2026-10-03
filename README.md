# server-durability

**What would this machine lose, and when?**

One server at home or in a closet usually has a database or two, a nightly backup, and a belief that it is
all fine. The belief is rarely tested. The backup may sit on the same disk as the data. It may copy a live
database as a plain file, which opens, passes its check and lacks the newest commits. It may not contain the
one store that was added last month. And nobody has restored from it.

`server-durability` reads a short declaration of what the machine must not lose, and then looks:

```console
$ server-durability audit
FAIL  backup         the repository is on the same disk (nvme0n1) as orders, uploads: when that disk dies, the data and its only backup die together
                     fix: keep a copy of the repository on another disk or another machine, and declare it as a [[copy]]
FAIL  orders         not in the newest snapshot: /srv/staged/orders.db
                     fix: add this path to what the backup takes
warn  orders         no staged copy yet in /srv/staged
                     fix: run `server-durability stage` before each backup
warn  restore        a restore has never been rehearsed here: the backup is a hope until one is
                     fix: run `server-durability drill`
note  disk nvme0n1   a volatile write cache under orders, uploads: what is not synced is lost in a power cut. Synced writes are safe as long as the drive honours flushes
ok    orders         a SQLite database in WAL mode
ok    backup         the newest snapshot (17308612) is 0 minutes old
ok    uploads        in the newest snapshot: /srv/app/uploads

2 would lose data or work, 2 depend on luck, 3 in order, 1 to know
```

Every line says what would be lost and how to fix it. Then the database is staged, everything is backed up and a
restore is rehearsed, on the same machine (the shared disk stays: this demo lives in one temporary directory):

```console
$ server-durability stage
ok    orders         /srv/staged/orders.db (24576 bytes)

$ server-durability covers
ok    orders         in the newest snapshot: /srv/staged/orders.db
ok    uploads        in the newest snapshot: /srv/app/uploads

2 in order

$ server-durability drill
ok    orders         restored, sound, 1 tables, 1000 rows (the live one has 1000 now)
ok    uploads        restored, 1 entries at its top

the rehearsal took 2.1 s; recorded in /srv/state/drill.json

$ server-durability audit
FAIL  backup         the repository is on the same disk (nvme0n1) as orders, uploads: when that disk dies, the data and its only backup die together
                     fix: keep a copy of the repository on another disk or another machine, and declare it as a [[copy]]
note  disk nvme0n1   a volatile write cache under orders, uploads: what is not synced is lost in a power cut. Synced writes are safe as long as the drive honours flushes
ok    orders         a SQLite database in WAL mode
ok    backup         the newest snapshot (89700967) is 0 minutes old
ok    orders         in the newest snapshot: /srv/staged/orders.db
ok    uploads        in the newest snapshot: /srv/app/uploads
ok    restore        a restore was rehearsed 0 minutes ago and every store opened

1 would lose data or work, 5 in order, 1 to know
```

One static binary, written in Go. It reads; the only things it writes are the staged copies you ask for and
the record of a rehearsed restore.

## Install

```bash
go install github.com/YauhenBichel/server-durability/cmd/server-durability@latest
```

Linux and macOS. The disk and file-system checks need Linux. The backup tool is [restic](https://restic.net)
for now; it must be installed.

## Start

```bash
mkdir -p ~/.config/server-durability
server-durability init > ~/.config/server-durability/config.toml     # then edit: your stores, your repository
server-durability audit
```

The declaration is the whole configuration:

```toml
[[store]]                     # something this machine must not lose
name = "orders"
path = "~/app/data/app.db"
kind = "sqlite"               # sqlite | file | directory

[stage]                       # consistent copies of the live databases go here; back this directory up
dir = "~/backups/sqlite"

[backup]
tool = "restic"
repository = "~/backups/restic"
password_file = "~/.config/restic/password"

[[copy]]                      # every other place the repository exists
name = "laptop mirror"
where = "same-site"           # same-disk | same-site | off-site

[[worker]]                    # services that must start again by themselves after a restart
unit = "app.service"
user = true
```

To see it work without touching anything of yours, [`examples/demo.sh`](examples/demo.sh) builds a throwaway
machine in a temporary directory and produces the two audits above.

## Commands

| Command | Does | Writes |
|---|---|---|
| `audit` | every check below; exit 1 when something would be lost (`-strict`: also on a warning) | nothing |
| `stage` | a consistent copy of each live SQLite database into the stage directory, by SQLite's backup API, checked and synced before it gets its name | the copies |
| `covers` | asks the newest snapshot whether every declared store is in it | nothing |
| `drill` | rehearses a restore: every store out of the newest snapshot into a scratch directory, databases opened and checked, row counts beside the live ones | the result, with its date |
| `init` | prints an example declaration | nothing |

All take `-json`. Exit status: 0 in order, 1 something would be lost or a step failed, 2 the command line was
wrong, 3 the declaration could not be read.

A backup script then reads:

```bash
server-durability stage && restic backup ~/backups/sqlite ~/app/uploads && server-durability covers
```

and a monthly timer runs `server-durability drill`.

## What the audit checks

| Check | Fails or warns when |
|---|---|
| the store is there, and is what the declaration says | the path is gone; a "database" that is not one |
| its file system | it is on tmpfs; mounted with `data=writeback` or without barriers |
| the disk under it and under the backup | the repository shares a physical disk with the data (through partitions and LVM) |
| the newest snapshot | none; older than `max_age_hours` |
| coverage | a declared store is not in the newest snapshot |
| copies | the repository is the only copy; no copy is off-site |
| staging | a live database is backed up as a plain file; the staged copy is stale |
| a rehearsed restore | never done; the last one failed; older than `max_age_days` |
| services | a declared worker is not enabled; a transient unit has run for over an hour; a calendar timer lacks `Persistent=true` |

A drive with a volatile write cache is noted, not judged: synced writes are safe on it as long as the drive
honours flushes.

## The library

Two packages can be used without the tool.

```go
import (
    "github.com/YauhenBichel/server-durability/durable"
    "github.com/YauhenBichel/server-durability/sqlitedb"
)

durable.SweepTemp(dir, time.Hour)                       // at start: what killed writers left behind
err := durable.AppendLine(logPath, line)                // on the disk before it returns
err  = durable.ReplaceFile(statePath, data, 0o644)      // the old content or the new, never half

err  = sqlitedb.Stage(livePath, copyPath)               // a consistent copy while the service writes
err  = sqlitedb.Checkpoint(path)                        // before a planned power-off, writer stopped
```

`ReplaceFile` writes a temporary file, syncs it, renames it over the target and syncs the directory. Its test
kills a writer 150 times at random moments and requires the file to be whole every time.

## Why these checks: the experiments

The checks come from measurements, published with their raw results:
[yserver-durability-experiments](https://huggingface.co/datasets/YauhenBichel/yserver-durability-experiments).

- A plain file copy of a SQLite database under a writer: 49 of 50 copies opened and passed the integrity
  check, and the median copy lacked 60,000 committed rows. The backup API lost none in 100 copies.
- A file written in place with every block synced: half written in 493 of 500 kills. Temporary file, sync,
  rename, sync the directory: none in 1,000.
- Both a Go and a Rust writer left temporary files behind when killed. Hence `SweepTemp`.

## Honest limits

- **It cannot see how a program writes.** An application that appends without syncing, or replaces a file
  without syncing the directory, is invisible from outside. The library is for writing correctly; the audit
  is for everything around the write.
- **A kill is not a power cut.** The tests kill processes. What a drive does when the power goes is its own
  matter; a volatile write cache is reported so you know to ask.
- **restic only**, today. The backup tool sits behind three questions (newest snapshot, its paths, restore
  these), so borg and kopia are additions, not changes.
- **SQLite only**, among databases. PostgreSQL and others have their own tools for consistent dumps.
- **The SQLite inside is a translation.** `modernc.org/sqlite` is SQLite compiled to Go, which is what lets
  this be one static binary; it is not the upstream C build. `drill` opens every copy for that reason too.
- **A declaration is an allow-list.** A store nobody declared is not audited.

## Roadmap

1. Durable jobs: a command that runs until it exits 0, across restarts (an enabled systemd unit until done).
2. A graceful power-off: stop workers, wait for requests in flight, checkpoint, record, power off.
3. borg and kopia. An MCP server and a small page, as in [llm-hops](https://github.com/YauhenBichel/llm-hops).
4. Tests under [LazyFS](https://github.com/dsrhaslab/lazyfs), which drops unsynced writes: a power cut, simulated.

## Contributing

```bash
make lint test        # go vet, gofmt, go test -race; the restic tests are skipped where restic is missing
```

Issues are welcome: a machine it reads wrongly (paste `server-durability audit -json`), a check it should have.

## Licence

Apache-2.0.
