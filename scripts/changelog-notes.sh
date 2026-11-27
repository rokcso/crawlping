#!/bin/sh
#
# changelog-notes.sh <tag> — print the CHANGELOG.md section for a release tag.
#
# The release workflow uses this output as the GitHub release body, so a version
# that is not written in CHANGELOG.md fails here. That is deliberate: the
# alternative, generating notes from commit subjects, cannot say "this breaks your
# monitoring", and somebody upgrading reads the release rather than the log.
#
# Exit codes: 0 the section was printed, 1 the section is missing or empty,
# 2 the usage was wrong.

set -eu

tag="${1:-}"
if [ -z "$tag" ]; then
	echo "usage: $0 <tag>   for example: $0 v0.2.0" >&2
	exit 2
fi

# Tags carry the v, headings do not: `git tag v0.2.0` is `## [0.2.0]` here.
version="${tag#v}"
file="${CHANGELOG:-CHANGELOG.md}"

if [ ! -f "$file" ]; then
	echo "$file not found" >&2
	exit 1
fi

# From the exact version heading to the next level-2 heading. Matching the whole
# bracket keeps 0.2.0 from also matching 0.20.0.
body=$(
	awk -v want="$version" '
		/^## \[/ {
			if (inside) exit
			line = $0
			sub(/^## \[/, "", line)
			sub(/\].*$/, "", line)
			if (line == want) { inside = 1; next }
		}
		inside { print }
	' "$file" | sed -e '/./,$!d'
)

if [ -z "$body" ]; then
	echo "CHANGELOG.md has no section for $version." >&2
	echo "Add '## [$version] - $(date +%Y-%m-%d)' with what changed for its readers." >&2
	exit 1
fi

printf '%s\n' "$body"
