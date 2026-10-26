#!/usr/bin/env bash
# Prints the next release version: <VERSION file>.<patch>
#
# VERSION holds MAJOR.MINOR (e.g. 2.2). The patch number is one more than the
# highest existing tag v<MAJOR.MINOR>.<n>, or 0 when there is none yet, so
# releases count 2.2.0, 2.2.1, 2.2.2 ... and restart at 2.3.0 when VERSION
# is changed to 2.3. Needs the repository's tags (git fetch --tags).
set -euo pipefail
cd "$(dirname "$0")/.."

base="$(tr -d ' \t\r\n' <VERSION)"
if ! [[ "$base" =~ ^[0-9]+\.[0-9]+$ ]]; then
	echo "VERSION must contain MAJOR.MINOR (e.g. 2.2), found '$base'" >&2
	exit 1
fi

last=-1
while read -r tag; do
	n="${tag#"v${base}."}"
	if [[ "$n" =~ ^[0-9]+$ ]] && ((n > last)); then
		last=$n
	fi
done < <(git tag --list "v${base}.*")

echo "${base}.$((last + 1))"
