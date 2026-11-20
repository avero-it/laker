#!/usr/bin/env bash
# Assembles the project website into <out-dir>: the files of website/ with the
# release tag filled in. Run it after build-apt-repo.sh, so the page lands in
# the same GitHub Pages site as the apt repository (a Pages deploy replaces the
# whole site).
#
#   scripts/build-website.sh <out-dir> <version>      e.g. site v0.9.0
set -euo pipefail

if [ $# -ne 2 ]; then
  echo "usage: $0 <out-dir> <version>" >&2
  exit 2
fi
out=$1
version=$2

# The version goes into sed and HTML: accept only a plain release tag.
if ! [[ $version =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$ ]]; then
  echo "build-website: '$version' is not a release tag like v1.2.3" >&2
  exit 2
fi

src="$(cd "$(dirname "$0")/.." && pwd)/website"
mkdir -p "$out"
cp -R "$src"/. "$out"/
# Templates are filled in by their own scripts (build-changelog.sh).
rm -f "$out"/*.template.html

if ! grep -rq '__VERSION__' "$out/index.html"; then
  echo "build-website: no __VERSION__ placeholder in index.html" >&2
  exit 1
fi
# BSD and GNU sed differ on -i, so write through a temporary file.
for f in "$out"/*.html; do
  sed "s/__VERSION__/$version/g" "$f" > "$f.tmp" && mv "$f.tmp" "$f"
done
if grep -rq '__VERSION__' "$out"/*.html; then
  echo "build-website: placeholder left over" >&2
  exit 1
fi
echo "built website $version in $out"
