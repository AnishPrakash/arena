#!/usr/bin/env bash
set -uo pipefail
fail=0

if grep -rEn '"github.com/[^"]*/internal/(adapters|api|judge)' internal/core/ 2>/dev/null; then
  echo "ARCH VIOLATION: internal/core imports an adapter or the api layer"; fail=1
fi
for pkg in github.com/jackc/pgx github.com/redis/go-redis net/http database/sql; do
  if grep -rn "\"$pkg" internal/core/ 2>/dev/null; then
    echo "ARCH VIOLATION: internal/core imports $pkg (must stay I/O free)"; fail=1
  fi
done
if grep -rEn '"github.com/[^"]*/internal/adapters' internal/ports/ 2>/dev/null; then
  echo "ARCH VIOLATION: internal/ports imports an adapter"; fail=1
fi

[ "$fail" -eq 0 ] && echo "architecture rules: OK"
exit $fail
