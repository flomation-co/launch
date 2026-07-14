#!/usr/bin/env sh
# Fail the pipeline if this branch introduces a migration version NUMBER that
# already exists on the target branch under a different name — a cross-branch
# collision.
#
# Why the sibling TestMigrationsSourceLoads is not enough: it loads THIS branch's
# migration set in isolation, which is always valid — each branch picks a free
# number at the time it is written. The duplicate only materialises in the MERGE,
# once the OTHER branch that grabbed the same number has also landed on the target.
# That is exactly how api main was taken down on 14/07/2026 when
# 125_AddFlomationGateway and 125_RepairFormTriggerTypes merged independently, and
# how a duplicate 120 broke it before. golang-migrate's iofs source rejects a
# duplicate version, so the service fails to boot.
#
# This check compares the union of (this branch ∪ target branch) and fails if any
# version number maps to two different migration names. Runs on EVERY pipeline.
set -eu
MIGDIR="${1:-internal/persistence/migration}"
TARGET="${CI_MERGE_REQUEST_TARGET_BRANCH_NAME:-main}"

git fetch -q origin "$TARGET" 2>/dev/null || true
if ! git rev-parse -q --verify "origin/$TARGET" >/dev/null 2>&1; then
  echo "migration-collision-check: origin/$TARGET unavailable — skipping cross-branch check"
  exit 0
fi

dupes=$(
  { git ls-tree -r --name-only HEAD             -- "$MIGDIR"
    git ls-tree -r --name-only "origin/$TARGET" -- "$MIGDIR"; } \
    | grep -E '\.up\.sql$' \
    | sed -E 's#.*/##; s#\.up\.sql$##' \
    | sort -u \
    | awk -F_ '{ v=$1; if (v in seen && seen[v] != $0) print v": "seen[v]" vs "$0; seen[v]=$0 }'
)

if [ -n "$dupes" ]; then
  echo "ERROR: migration version collision with origin/$TARGET:"
  echo "$dupes" | sed 's/^/  /'
  echo ""
  echo "Another branch already used that number on $TARGET. Renumber your new"
  echo "migration(s) above the current max on $TARGET and rebase."
  exit 1
fi
echo "migration-collision-check: OK — no version collides with origin/$TARGET"
