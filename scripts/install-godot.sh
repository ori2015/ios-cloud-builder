#!/usr/bin/env bash
# Installs the Godot editor and export templates for macOS from the official
# GitHub release, verifying both downloads against the release's SHA512-SUMS.
#
#   scripts/install-godot.sh <version> <destination> [--resolve-only]
#
# <version> is x.y.z, x.y.z-stable, or x.y (the newest stable patch of that
# minor release). Layout under <destination>:
#   Godot.app/                 the editor
#   templates/                 the export templates (templates/version.txt)
# Prints "tag=<release tag>" and "templates_version=<name>" on success.
set -euo pipefail

version="${1:?usage: install-godot.sh <version> <destination> [--resolve-only]}"
dest="${2:?usage: install-godot.sh <version> <destination> [--resolve-only]}"
resolve_only="${3:-}"
version="${version%-stable}"

if ! [[ "$version" =~ ^[0-9]+\.[0-9]+(\.[0-9]+)?$ ]]; then
  echo "install-godot: '$version' is not a Godot version (x.y or x.y.z)" >&2
  exit 1
fi

api_curl() {
  local args=(-fsSL -H "Accept: application/vnd.github+json")
  # the token only raises the API rate limit; it is sent to api.github.com alone
  if [ -n "${GITHUB_TOKEN:-}" ]; then args+=(-H "Authorization: Bearer $GITHUB_TOKEN"); fi
  curl "${args[@]}" "$1"
}

resolve_tag() {
  if [[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    echo "${version}-stable"
    return
  fi
  api_curl "https://api.github.com/repos/godotengine/godot/releases?per_page=100" | python3 -c '
import json, re, sys
minor = sys.argv[1]
best = None
for release in json.load(sys.stdin):
    tag = release["tag_name"]
    m = re.fullmatch(re.escape(minor) + r"(?:\.(\d+))?-stable", tag)
    if m and not release.get("prerelease"):
        patch = int(m.group(1) or 0)
        if best is None or patch > best[0]:
            best = (patch, tag)
if best is None:
    sys.exit("no stable Godot release found for " + minor)
print(best[1])
' "$version"
}

tag="$(resolve_tag)"
echo "tag=$tag"
[ "$resolve_only" = "--resolve-only" ] && exit 0

base="https://github.com/godotengine/godot/releases/download/$tag"
editor="Godot_v${tag}_macos.universal.zip"
templates="Godot_v${tag}_export_templates.tpz"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
curl -fsSL -o "$work/SHA512-SUMS.txt" "$base/SHA512-SUMS.txt"
curl -fsSL -o "$work/$editor" "$base/$editor"
curl -fsSL -o "$work/$templates" "$base/$templates"

for file in "$editor" "$templates"; do
  expected="$(awk -v f="$file" '$2 == f || $2 == "*" f {print $1}' "$work/SHA512-SUMS.txt")"
  if [ -z "$expected" ]; then echo "install-godot: $file is not listed in SHA512-SUMS.txt" >&2; exit 1; fi
  actual="$(shasum -a 512 "$work/$file" | awk '{print $1}')"
  if [ "$expected" != "$actual" ]; then echo "install-godot: checksum mismatch for $file" >&2; exit 1; fi
done

rm -rf "$dest"
mkdir -p "$dest"
unzip -q "$work/$editor" -d "$dest"
unzip -q "$work/$templates" -d "$work/tpl"
mv "$work/tpl/templates" "$dest/templates"
echo "templates_version=$(tr -d '[:space:]' < "$dest/templates/version.txt")"
