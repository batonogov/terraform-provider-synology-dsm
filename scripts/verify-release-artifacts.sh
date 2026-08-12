#!/usr/bin/env bash
set -euo pipefail

fail() {
  echo "release artifacts: $*" >&2
  exit 1
}

[[ $# -eq 2 || ( $# -eq 3 && $3 == "--skip-signature" ) ]] ||
  fail "usage: verify-release-artifacts.sh DIST_DIR VERSION [--skip-signature]"

dist_dir=$1
version=$2
project=terraform-provider-synology-dsm
repository_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
checksum_name="${project}_${version}_SHA256SUMS"
checksum_path="$dist_dir/$checksum_name"
manifest_name="${project}_${version}_manifest.json"
manifest_source="$repository_dir/terraform-registry-manifest.json"

[[ -d "$dist_dir" ]] || fail "distribution directory does not exist: $dist_dir"
[[ "$version" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z][0-9A-Za-z.-]*)?$ ]] ||
  fail "version is not strict semantic versioning: $version"
[[ -f "$checksum_path" ]] || fail "missing $checksum_name"
[[ -f "$manifest_source" ]] || fail "missing terraform-registry-manifest.json"

jq -e '
  .version == 1
  and .metadata.protocol_versions == ["6.0"]
' "$manifest_source" >/dev/null || fail "invalid Terraform Registry manifest"

temporary_dir=$(mktemp -d "${TMPDIR:-/tmp}/synology-dsm-release.XXXXXX")
trap 'rm -rf "$temporary_dir"' EXIT

expected_archives="$temporary_dir/expected-archives.txt"
actual_archives="$temporary_dir/actual-archives.txt"
expected_checksum_assets="$temporary_dir/expected-checksum-assets.txt"
actual_checksum_assets="$temporary_dir/actual-checksum-assets.txt"

for goos in darwin freebsd linux windows; do
  for goarch in amd64 arm64; do
    printf '%s_%s_%s_%s.zip\n' "$project" "$version" "$goos" "$goarch"
  done
done | LC_ALL=C sort >"$expected_archives"

find "$dist_dir" -maxdepth 1 -type f \
  -name "${project}_${version}_*.zip" \
  -exec basename {} \; | LC_ALL=C sort >"$actual_archives"

diff -u "$expected_archives" "$actual_archives" ||
  fail "release archive set is incomplete or contains unexpected files"

{
  cat "$expected_archives"
  printf '%s\n' "$manifest_name"
} | LC_ALL=C sort >"$expected_checksum_assets"

awk '
  NF != 2 || length($1) != 64 || $1 !~ /^[0-9a-f]+$/ { exit 1 }
  { print $2 }
' "$checksum_path" | LC_ALL=C sort >"$actual_checksum_assets" ||
  fail "$checksum_name contains malformed entries"

diff -u "$expected_checksum_assets" "$actual_checksum_assets" ||
  fail "$checksum_name must cover exactly the archives and Registry manifest"

hash_file() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

while IFS= read -r asset_name; do
  asset_path="$dist_dir/$asset_name"
  [[ "$asset_name" == "$manifest_name" ]] && asset_path="$manifest_source"
  [[ -f "$asset_path" ]] || fail "missing checksummed asset $asset_name"
  expected=$(awk -v name="$asset_name" '$2 == name {print $1}' "$checksum_path")
  actual=$(hash_file "$asset_path")
  [[ "$actual" == "$expected" ]] || fail "checksum mismatch for $asset_name"
done <"$expected_checksum_assets"

while IFS= read -r archive_name; do
  binary_name="${project}_v${version}"
  [[ "$archive_name" == *"_windows_"* ]] && binary_name="${binary_name}.exe"
  count=$(unzip -Z1 "$dist_dir/$archive_name" | awk -v name="$binary_name" '$0 == name {n++} END {print n + 0}')
  [[ "$count" -eq 1 ]] || fail "$archive_name must contain exactly one $binary_name"
done <"$expected_archives"

if [[ ${3:-} != "--skip-signature" ]]; then
  [[ -f "${checksum_path}.sig" ]] || fail "missing detached GPG signature ${checksum_name}.sig"
fi

echo "release artifacts: verified 8 archives, manifest, and checksums"
