#!/usr/bin/env bash
set -euo pipefail

target_goroot="${1:-${MARTEN_ANDROID_BOOTTIME_GOROOT:-$HOME/.cache/marten/go-android-boottime/$(go env GOVERSION)}}"
source_goroot="${MARTEN_BASE_GOROOT:-$(go env GOROOT)}"
source_go="$source_goroot/bin/go"

if [[ ! -x "$source_go" ]]; then
  echo "source Go toolchain not found at $source_go" >&2
  exit 1
fi

source_version="$("$source_go" version)"
marker="$target_goroot/.marten-boottime-version"

is_patched() {
  [[ -x "$target_goroot/bin/go" ]] &&
    [[ -f "$marker" ]] &&
    grep -Fxq "$source_version" "$marker" &&
    grep -q 'CLOCK_BOOTTIME' "$target_goroot/src/runtime/sys_linux_arm64.s" &&
    grep -q 'CLOCK_BOOTTIME' "$target_goroot/src/runtime/sys_linux_arm.s" &&
    grep -q 'CLOCK_BOOTTIME' "$target_goroot/src/runtime/sys_linux_amd64.s" &&
    grep -q 'CLOCK_BOOTTIME' "$target_goroot/src/runtime/sys_linux_386.s"
}

if is_patched; then
  echo "Using patched Android Go runtime: $target_goroot"
  exit 0
fi

tmp_goroot="$target_goroot.tmp.$$"
rm -rf "$tmp_goroot"
mkdir -p "$(dirname "$target_goroot")"

if command -v rsync >/dev/null 2>&1; then
  rsync -a --delete "$source_goroot"/ "$tmp_goroot"/
else
  mkdir -p "$tmp_goroot"
  (cd "$source_goroot" && tar cf - .) | (cd "$tmp_goroot" && tar xf -)
fi

# Toolchains downloaded by GOTOOLCHAIN are stored in the module cache with
# read-only source files. The Android CLOCK_BOOTTIME patch needs a private,
# writable copy regardless of how the selected toolchain was installed.
chmod -R u+w "$tmp_goroot"

patch_runtime() {
  local file="$1"
  shift
  perl -0pi -e "$*" "$file"
}

# Android can suspend CLOCK_MONOTONIC while the device sleeps. WireGuard's
# timers need elapsed boottime so persistent keepalive/rekey timers catch up
# immediately after wake, without holding a permanent PARTIAL_WAKE_LOCK.
patch_runtime "$tmp_goroot/src/runtime/sys_linux_arm64.s" \
  's/#define CLOCK_MONOTONIC\s+1/#define CLOCK_BOOTTIME 7/; s/\$CLOCK_MONOTONIC/\$CLOCK_BOOTTIME/g'
patch_runtime "$tmp_goroot/src/runtime/sys_linux_arm.s" \
  's/#define CLOCK_MONOTONIC\s+1/#define CLOCK_BOOTTIME 7/; s/\$CLOCK_MONOTONIC/\$CLOCK_BOOTTIME/g'
patch_runtime "$tmp_goroot/src/runtime/sys_linux_amd64.s" \
  's/\$1, DI \/\/ CLOCK_MONOTONIC/\$7, DI \/\/ CLOCK_BOOTTIME/g'
patch_runtime "$tmp_goroot/src/runtime/sys_linux_386.s" \
  's/\$1, 0\(SP\)\s+\/\/ CLOCK_MONOTONIC/\$7, 0(SP)\t\/\/ CLOCK_BOOTTIME/g; s/\$1, BX\s+\/\/ CLOCK_MONOTONIC/\$7, BX\t\t\/\/ CLOCK_BOOTTIME/g'

echo "$source_version" > "$tmp_goroot/.marten-boottime-version"
rm -rf "$target_goroot"
mv "$tmp_goroot" "$target_goroot"
echo "Prepared patched Android Go runtime: $target_goroot"
