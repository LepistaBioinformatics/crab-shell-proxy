#!/usr/bin/env bash
# Persona propagation diagnostic — run on the HOST that runs the containers.
#
# Three different faults produce the same symptom ("saving does not reach the
# instances, even after a restart") and each is fixed in a different place. This
# says which one it is. Read-only: it inspects, it never changes anything.
#
#   usage:  ./diagnose-persona.sh <DATA_ROOT> [AGENT]
#   e.g.    ./diagnose-persona.sh /opt/zombie-crab/data alpha
#
# DATA_ROOT is the host path behind the proxy's data root (the one holding
# `tenants/` and `effective-persona/`).

set -uo pipefail
ROOT="${1:?usage: $0 <DATA_ROOT> [AGENT]}"
AGENT="${2:-alpha}"
FILE="AGENT.md"

echo "== 1. what the admin scope holds (the injection itself) =="
find "$ROOT/tenants" -path "*/shared/agents/$AGENT/persona/$FILE" -printf '%TY-%Tm-%Td %TH:%TM  %s bytes  %p\n' 2>/dev/null \
  || echo "   (none — no injection written at any scope for agent $AGENT)"

echo
echo "== 2. what the effective dir holds (the bind-mount SOURCE) =="
echo "   If the mtime here is older than the injection above, the write never"
echo "   propagated: the fault is in the sync, not in the mounts."
find "$ROOT/effective-persona" -name "$FILE" -printf '%TY-%Tm-%Td %TH:%TM  %s bytes  %p\n' 2>/dev/null \
  || echo "   (none — nothing was ever materialized for this agent)"

echo
echo "== 3. do the containers actually MOUNT the file? =="
echo "   A bind is fixed when the container is created and a restart never adds"
echo "   one. No line ending in /workspace/$FILE = the injection has no path in,"
echo "   whatever the effective dir says."
for c in $(docker ps -a --filter "label=crab-shell.agent=$AGENT" --format '{{.Names}}'); do
  echo "-- $c"
  docker inspect "$c" --format '{{range .HostConfig.Binds}}{{println "   " .}}{{end}}' 2>/dev/null \
    | grep -E "persona|workspace/[A-Z]+\.md" || echo "    NO persona bind"
done

echo
echo "== 4. what the agent actually reads inside the container =="
echo "   Content matching the injection here, with 2 and 3 healthy, means"
echo "   delivery works and what is stale is the agent's own session."
for c in $(docker ps --filter "label=crab-shell.agent=$AGENT" --format '{{.Names}}'); do
  echo "-- $c"
  docker exec "$c" sh -lc 'cat "$HOME/.picoclaw/workspace/'"$FILE"'" 2>/dev/null | head -8' \
    || echo "    (could not exec)"
done
