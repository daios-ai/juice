#!/usr/bin/env bash
# snapshot.sh — concatenate all .go files in the repo into a single file.
# Usage: ./snapshot.sh [output_file]
# Default output: snapshot.go.txt

set -euo pipefail

OUT="${1:-snapshot.go.txt}"
REPO_ROOT="$(cd "$(dirname "$0")" && pwd)"

> "$OUT"

find "$REPO_ROOT" -name "*.go" | sort | while read -r f; do
    rel="${f#"$REPO_ROOT/"}"
    printf '// ===== %s =====\n\n' "$rel" >> "$OUT"
    cat "$f" >> "$OUT"
    printf '\n\n' >> "$OUT"
done

echo "Snapshot written to $OUT ($(wc -l < "$OUT") lines)"
