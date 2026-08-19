#!/usr/bin/env bash
#
# Prints the CHANGELOG section for one version, for use as GitHub release
# notes.
#
#   release-notes.sh <version> [changelog]
#
# Headings are matched as "## <version>" or "## v<version>". If the version has
# no section yet the "Unreleased" section is used, and if there is no such
# section either the whole changelog is printed - a release never fails for
# want of notes.

set -euo pipefail

version="${1:?usage: $0 <version> [changelog]}"
changelog="${2:-CHANGELOG.md}"

section() {
    awk -v want="$1" '
        /^## / {
            title = substr($0, 4)
            sub(/[ \t]+$/, "", title)
            found = (title == want)
            if (found) next
            if (printing) exit
        }
        found { printing = 1; print }
    ' "$changelog"
}

for heading in "v${version}" "${version}" "Unreleased"; do
    body="$(section "$heading")"
    if [ -n "${body//[[:space:]]/}" ]; then
        printf '%s\n' "$body"
        exit 0
    fi
done

cat "$changelog"
