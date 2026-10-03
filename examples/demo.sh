#!/usr/bin/env bash
# demo.sh: a throwaway "machine" in a temporary directory, audited before and after it is put in order.
# Needs: server-durability on PATH (or SD=/path/to/it), restic, python3. Touches nothing outside its directory.
set -euo pipefail
SD="${SD:-server-durability}"
D="$(mktemp -d)"; trap 'rm -rf "$D"' EXIT
mkdir -p "$D/app/uploads" && echo "a photo" > "$D/app/uploads/photo.txt"
python3 - "$D/app/app.db" <<'PY'
import sqlite3, sys
c = sqlite3.connect(sys.argv[1]); c.execute("PRAGMA journal_mode=WAL"); c.execute("create table orders(id, item)")
c.executemany("insert into orders values(?, ?)", [(i, "item") for i in range(1000)]); c.commit()
PY
echo "demo-only" > "$D/password"
export RESTIC_REPOSITORY="$D/repo" RESTIC_PASSWORD_FILE="$D/password"
restic init -q
cat > "$D/config.toml" <<TOML
state_dir = "$D/state"
[[store]]
name = "orders"
path = "$D/app/app.db"
kind = "sqlite"
[[store]]
name = "uploads"
path = "$D/app/uploads"
kind = "directory"
[stage]
dir = "$D/staged"
[backup]
tool = "restic"
repository = "$D/repo"
password_file = "$D/password"
TOML
show() { echo; echo "\$ server-durability $*"; "$SD" -config "$D/config.toml" "$@" | sed "s#$D#/srv#g" || true; }

echo "== 1. A backup exists, and takes the uploads only =="
restic backup -q "$D/app/uploads"
show audit

echo; echo "== 2. Stage the database, back up everything, rehearse the restore =="
show stage
restic backup -q "$D/staged" "$D/app/uploads"
show covers
show drill
show audit
