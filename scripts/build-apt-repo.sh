#!/usr/bin/env bash
# Build a signed apt repository from a directory of .deb files (laker and laker-agent).
#
#   APT_GPG_PRIVATE_KEY=<armored key> scripts/build-apt-repo.sh <debs-dir> <out-dir> [base-url]
#
# Needs apt-ftparchive (package apt-utils) and gpg. The result in <out-dir> is what gets served
# from GitHub Pages: pool/, dists/stable/ (Release, InRelease, Release.gpg, Packages per
# architecture), the public key (laker.gpg binary, laker.asc armored) and a small index.html.
# See .github/workflows/apt.yml.
set -euo pipefail

debs=${1:?usage: build-apt-repo.sh <debs-dir> <out-dir> [base-url]}
out=${2:?usage: build-apt-repo.sh <debs-dir> <out-dir> [base-url]}
base_url=${3:-https://avero-it.github.io/laker}
: "${APT_GPG_PRIVATE_KEY:?APT_GPG_PRIVATE_KEY is not set}"

archs="amd64 arm64"

shopt -s nullglob
files=("$debs"/*.deb)
if [ ${#files[@]} -eq 0 ]; then
  echo "no .deb files in $debs" >&2
  exit 1
fi

export GNUPGHOME
GNUPGHOME=$(mktemp -d)
trap 'rm -rf "$GNUPGHOME"' EXIT
chmod 700 "$GNUPGHOME"
printf '%s\n' "$APT_GPG_PRIVATE_KEY" | gpg --batch --quiet --import
fpr=$(gpg --batch --list-secret-keys --with-colons | awk -F: '/^fpr:/ {print $10; exit}')
[ -n "$fpr" ] || { echo "no secret key imported" >&2; exit 1; }

rm -rf "$out"
mkdir -p "$out/pool/main/l/laker"
cp "${files[@]}" "$out/pool/main/l/laker/"

cd "$out"
for arch in $archs; do
  dir="dists/stable/main/binary-$arch"
  mkdir -p "$dir"
  apt-ftparchive --arch "$arch" packages pool > "$dir/Packages"
  gzip -9n -k -f "$dir/Packages"
done

apt-ftparchive \
  -o APT::FTPArchive::Release::Origin=laker \
  -o APT::FTPArchive::Release::Label=laker \
  -o APT::FTPArchive::Release::Suite=stable \
  -o APT::FTPArchive::Release::Codename=stable \
  -o APT::FTPArchive::Release::Architectures="$archs" \
  -o APT::FTPArchive::Release::Components=main \
  release dists/stable > dists/stable/Release

gpg --batch --yes --pinentry-mode loopback --default-key "$fpr" \
  --clearsign -o dists/stable/InRelease dists/stable/Release
gpg --batch --yes --pinentry-mode loopback --default-key "$fpr" \
  --armor --detach-sign -o dists/stable/Release.gpg dists/stable/Release

gpg --batch --export "$fpr" > laker.gpg
gpg --batch --armor --export "$fpr" > laker.asc

cat > index.html <<HTML
<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>Laker apt repository</title>
<meta name="viewport" content="width=device-width, initial-scale=1"></head>
<body style="font-family: system-ui, sans-serif; max-width: 46rem; margin: 2rem auto; padding: 0 1rem">
<h1>Laker apt repository</h1>
<p>Debian and Ubuntu packages for <a href="https://github.com/avero-it/laker">laker</a> and laker-agent (amd64, arm64).</p>
<pre>sudo curl -fsSL $base_url/laker.gpg -o /usr/share/keyrings/laker.gpg
echo "deb [signed-by=/usr/share/keyrings/laker.gpg] $base_url stable main" | sudo tee /etc/apt/sources.list.d/laker.list
sudo apt update &amp;&amp; sudo apt install laker laker-agent</pre>
</body></html>
HTML

echo "built apt repository in $out with $(ls pool/main/l/laker | wc -l) package file(s), key $fpr"
