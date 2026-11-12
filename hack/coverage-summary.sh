#!/usr/bin/env bash
# Prints a Markdown coverage table per file from a Go coverage profile, for $GITHUB_STEP_SUMMARY.
# With -coverpkg every test binary reports every block, so a block counts as covered when any binary hit it.
set -euo pipefail

profile="${1:-cover.out}"

echo "### Coverage: $(go tool cover -func="$profile" | awk '/^total:/ {print $NF}') of statements"
echo
echo "Unit and envtest suites; the kind end-to-end suite is not included."
echo
echo "| File | Statements | Covered |"
echo "| --- | ---: | ---: |"
awk 'NR > 1 {
    block = $1; stmts[block] = $2
    if ($3 > 0) hit[block] = 1
    split(block, parts, ":"); file[block] = parts[1]
}
END {
    for (b in stmts) {
        total[file[b]] += stmts[b]
        if (b in hit) covered[file[b]] += stmts[b]
    }
    for (f in total) printf "| `%s` | %d | %.1f%% |\n", f, total[f], 100 * covered[f] / total[f]
}' "$profile" | sed "s#\`$(go list -m)/#\`#" | sort
