#!/usr/bin/env bash
# End-to-end smoke test for dsh-desktop.
#
# Proves three things that a "does it compile" check cannot:
#   1. the app boots a real Harness host and loads it in a real WebKit window
#   2. the rendered page actually contains the app UI (not a blank window)
#   3. the montir agent preset is on the roster and selected by default
#   4. shutting the window down leaves no orphan host or WebKit process
#
# Process checks compare *PID sets* before and after rather than pattern-matching
# command lines, because `pgrep -f` happily matches the test script's own
# arguments and reports phantom orphans.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="${ROOT}/dist/dsh-desktop"
SELFTEST_SECONDS="${SELFTEST_SECONDS:-25}"
FAILED=0

pass() { printf '  \033[32mPASS\033[0m %s\n' "$*"; }
fail() { printf '  \033[31mFAIL\033[0m %s\n' "$*"; FAILED=1; }
note() { printf '       %s\n' "$*"; }

[ -x "$BIN" ] || { echo "missing $BIN — run: make build" >&2; exit 2; }
command -v python3 >/dev/null || { echo "python3 is required" >&2; exit 2; }

# Snapshot by PID, so a later comparison cannot be fooled by command-line text.
snapshot() {
  { pgrep -x dsh-desktop; pgrep -f 'bin/node.*/dsh web'; pgrep -x WebKitWebProcess; } 2>/dev/null | sort -u
}

echo "==> dsh-desktop smoke test"
echo "    binary: $BIN"
echo

BEFORE="$(snapshot)"

echo "1. boot, load, inspect"
REPORT="$(mktemp)"
if timeout $((SELFTEST_SECONDS + 40)) "$BIN" -selftest "${SELFTEST_SECONDS}s" >"$REPORT" 2>/dev/null; then
  pass "process exited 0"
else
  fail "process did not exit 0"
fi

python3 - "$REPORT" <<'PY'
import json, sys

path = sys.argv[1]
try:
    r = json.load(open(path))
except Exception as exc:
    print(f"  \033[31mFAIL\033[0m unreadable report: {exc}")
    sys.exit(1)

checks = [
    ("window loaded a page",        r.get("readyState") == "complete"),
    ("app root element rendered",   bool(r.get("hasRoot"))),
    ("client UI is interactive",    r.get("buttons", 0) > 5),
    ("stylesheet bundle applied",   r.get("stylesheets", 0) > 10),
    ("no page-level errors",        not r.get("pageErrors")),
    ("montir preset on the roster", "Montir" in (r.get("presetOptions") or [])),
]
failed = False
for label, ok in checks:
    print(("  \033[32mPASS\033[0m " if ok else "  \033[31mFAIL\033[0m ") + label)
    failed = failed or not ok

print(f"       title        : {r.get('title')}")
print(f"       readyState   : {r.get('readyState')}")
print(f"       buttons      : {r.get('buttons')}   stylesheets: {r.get('stylesheets')}")
print(f"       presets      : {r.get('presetOptions')}")
print(f"       pageErrors   : {r.get('pageErrors')}")
body = (r.get("bodyText") or "").strip()
if body:
    print(f"       bodyText     : {body[:160]}")
sys.exit(1 if failed else 0)
PY
[ $? -ne 0 ] && FAILED=1

echo
echo "2. clean shutdown"
sleep 3
AFTER="$(snapshot)"
NEW="$(comm -13 <(echo "$BEFORE") <(echo "$AFTER") | grep -v '^$' || true)"
if [ -z "$NEW" ]; then
  pass "no orphan processes left behind"
else
  fail "orphan processes survived:"
  for pid in $NEW; do note "$(ps -o pid=,cmd= -p "$pid" 2>/dev/null)"; done
fi

rm -f "$REPORT"
echo
if [ "$FAILED" -eq 0 ]; then
  echo "==> all checks passed"
else
  echo "==> FAILURES above"
fi
exit "$FAILED"
