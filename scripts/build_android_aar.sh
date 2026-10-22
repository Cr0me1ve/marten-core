#!/bin/sh
set -eu

: "${MARTEN_ANDROID_CORE_DIR:?missing core directory}"
: "${MARTEN_ANDROID_SING_BOX_DIR:?missing sing-box directory}"
: "${MARTEN_ANDROID_TURNCOAT_DIR:?missing TURNcoat directory}"
: "${MARTEN_ANDROID_GOROOT:?missing Android Go root}"
: "${MARTEN_ANDROID_TARGET:?missing Android target}"
: "${MARTEN_ANDROID_TAGS:?missing build tags}"
: "${MARTEN_ANDROID_LDFLAGS:?missing linker flags}"
: "${MARTEN_ANDROID_OUTPUT:?missing output path}"

android_workspace_dir=$(mktemp -d "${TMPDIR:-/tmp}/marten-android-work.XXXXXX")
android_go="$MARTEN_ANDROID_GOROOT/bin/go"
[ -x "$android_go" ] || { echo "missing Android Go toolchain: $android_go" >&2; exit 1; }
cleanup_android_workspace() {
  [ ! -e "$android_workspace_dir/go.work.sum" ] || unlink "$android_workspace_dir/go.work.sum"
  [ ! -e "$android_workspace_dir/go.work" ] || unlink "$android_workspace_dir/go.work"
  rmdir "$android_workspace_dir"
}
trap cleanup_android_workspace EXIT HUP INT TERM

cd "$android_workspace_dir"
GOTOOLCHAIN=local GOROOT="$MARTEN_ANDROID_GOROOT" GOWORK=off "$android_go" work init "$MARTEN_ANDROID_CORE_DIR"
GOTOOLCHAIN=local GOROOT="$MARTEN_ANDROID_GOROOT" GOWORK="$android_workspace_dir/go.work" "$android_go" work edit \
  -replace "github.com/sagernet/sing-box=$MARTEN_ANDROID_SING_BOX_DIR"
GOTOOLCHAIN=local GOROOT="$MARTEN_ANDROID_GOROOT" GOWORK="$android_workspace_dir/go.work" "$android_go" work edit \
  -replace "github.com/xtls/xray-core=$MARTEN_ANDROID_SING_BOX_DIR/third_party/xray-core"
GOTOOLCHAIN=local GOROOT="$MARTEN_ANDROID_GOROOT" GOWORK="$android_workspace_dir/go.work" "$android_go" work edit \
  -replace "github.com/cacggghp/turncoat=$MARTEN_ANDROID_TURNCOAT_DIR"
GOTOOLCHAIN=local GOROOT="$MARTEN_ANDROID_GOROOT" GOWORK="$android_workspace_dir/go.work" "$android_go" work edit \
  -replace "google.golang.org/genproto=google.golang.org/genproto@v0.0.0-20260406210006-6f92a3bedf2d"

cd "$MARTEN_ANDROID_CORE_DIR"
GOFLAGS="-buildvcs=false" \
GOTOOLCHAIN=local \
GOWORK="$android_workspace_dir/go.work" \
GOROOT="$MARTEN_ANDROID_GOROOT" \
PATH="$MARTEN_ANDROID_GOROOT/bin:$PATH" \
gomobile bind -v \
  -androidapi=21 \
  -javapkg=app.marten.core \
  -libname=marten-core \
  -tags="$MARTEN_ANDROID_TAGS" \
  -trimpath \
  -target="$MARTEN_ANDROID_TARGET" \
  -ldflags="$MARTEN_ANDROID_LDFLAGS -R 16384 -extldflags '-Wl,-z,max-page-size=16384 -Wl,-z,common-page-size=16384'" \
  -o "$MARTEN_ANDROID_OUTPUT" \
  github.com/sagernet/sing-box/experimental/libbox \
  ./platform/mobile
