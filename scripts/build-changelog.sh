#!/usr/bin/env bash
# Builds changelog.html in <out-dir> from the published GitHub releases and the
# git history between them, so the page updates itself on every release (it runs
# in apt.yml, which follows each Release). A tag that was never published (a
# failed release) has no section; its commits show up under the next release.
# Needs gh (logged in or GH_TOKEN) and a clone that has all tags and history
# (actions/checkout with fetch-depth: 0).
#
#   scripts/build-changelog.sh <out-dir>              e.g. site
set -euo pipefail

if [ $# -ne 1 ]; then
  echo "usage: $0 <out-dir>" >&2
  exit 2
fi
out=$1
root="$(cd "$(dirname "$0")/.." && pwd)"
tpl="$root/website/changelog.template.html"
tag_re='^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$'

# Published releases, newest first: "<tag><TAB><yyyy-mm-dd>".
# (A read loop instead of mapfile, so it also runs on macOS's bash 3.2.)
releases=()
while IFS= read -r line; do releases+=("$line"); done < <(gh release list --limit 200 --json tagName,publishedAt,isDraft \
  --jq 'map(select(.isDraft | not)) | sort_by(.publishedAt) | reverse | .[] | "\(.tagName)\t\(.publishedAt[:10])"')
if [ ${#releases[@]} -eq 0 ]; then
  echo "build-changelog: no published releases" >&2
  exit 1
fi

# Subjects go into HTML.
escape() { sed -e 's/&/\&amp;/g' -e 's/</\&lt;/g' -e 's/>/\&gt;/g'; }

entries=$(mktemp)
trap 'rm -f "$entries"' EXIT

latest=""
for i in "${!releases[@]}"; do
  IFS=$'\t' read -r tag date <<<"${releases[$i]}"
  if ! [[ $tag =~ $tag_re ]]; then
    echo "build-changelog: skipping odd tag '$tag'" >&2
    continue
  fi
  [ -n "$latest" ] || latest=$tag
  prev=""
  if [ $((i + 1)) -lt ${#releases[@]} ]; then
    IFS=$'\t' read -r prev _ <<<"${releases[$((i + 1))]}"
  fi
  {
    printf '<section class="release" id="%s">\n' "$tag"
    printf '<h2><a href="https://github.com/avero-it/laker/releases/tag/%s">%s</a> <span class="date">%s</span></h2>\n' "$tag" "$tag" "$date"
    if [ -z "$prev" ]; then
      echo '<p>First release.</p>'
    elif ! git -C "$root" rev-parse -q --verify "refs/tags/$tag^{commit}" >/dev/null ||
      ! git -C "$root" rev-parse -q --verify "refs/tags/$prev^{commit}" >/dev/null; then
      echo '<p>See the release notes on GitHub.</p>'
    else
      echo '<ul>'
      git -C "$root" log --no-merges --reverse --pretty='%s' "$prev..$tag" | escape | sed 's|^\(.*\)$|<li>\1</li>|'
      echo '</ul>'
    fi
    echo '</section>'
  } >>"$entries"
done

if ! [[ $latest =~ $tag_re ]]; then
  echo "build-changelog: no usable release tag" >&2
  exit 1
fi
mkdir -p "$out"
# Put the entries in place of the placeholder line, then fill in the version.
awk -v entries="$entries" '
  /__ENTRIES__/ { while ((getline line < entries) > 0) print line; next }
  { print }' "$tpl" | sed "s/__VERSION__/$latest/g" >"$out/changelog.html"
if grep -q '__ENTRIES__\|__VERSION__' "$out/changelog.html"; then
  echo "build-changelog: placeholder left over" >&2
  exit 1
fi
echo "built changelog ($latest, ${#releases[@]} releases) in $out"
