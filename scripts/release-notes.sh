#!/bin/sh
# Print the CHANGELOG.md section for one version: the text under
# "## [VERSION]" up to the next "## " heading or the link references at the
# end. Fails when the section is missing or empty, so a release cannot go out
# without notes.
#
#   scripts/release-notes.sh 0.1.4 [CHANGELOG.md]
set -eu

version=${1:?usage: release-notes.sh VERSION [CHANGELOG]}
version=${version#v}
file=${2:-CHANGELOG.md}

if [ ! -f "$file" ]; then
	echo "release-notes: $file not found" >&2
	exit 1
fi

notes=$(awk -v want="## [$version]" '
	index($0, want) == 1 { found = 1; next }
	found && (/^## / || /^\[[^]]+\]: /) { exit }
	found { print }
' "$file" | awk '
	# Drop leading and trailing blank lines.
	NF { started = 1; for (; blank > 0; blank--) print ""; print; next }
	started { blank++ }
')

if [ -z "$notes" ]; then
	echo "release-notes: $file has no section for $version (add \"## [$version] - YYYY-MM-DD\")" >&2
	exit 1
fi
printf '%s\n' "$notes"
