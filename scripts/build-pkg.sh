#!/bin/sh
# build-pkg.sh -- build a macOS .pkg installer from a goreleaser darwin tarball.
#
# Payload:
#   /usr/local/bin/turingtap-agent
#   /Library/LaunchAgents/ai.turingtap.agent.plist
#     (a LaunchAgent, not a LaunchDaemon: the agent drives the logged-in
#     user's browser, so it must run in the user's GUI session)
#
# A postinstall script bootstraps the LaunchAgent for the current console
# user (best-effort; it loads at next login otherwise).
#
# If DEVID_KEY and DEVID_CERT are set (paths to a Developer ID Application
# PEM key/cert), the binary is rcodesign-signed (hardened runtime,
# identifier ai.turingtap.agent) before packaging. The output .pkg itself is
# NOT signed here; the caller signs it with a Developer ID Installer cert.
#
# Usage: VERSION=x.y.z scripts/build-pkg.sh <darwin tarball> <output.pkg>
# Requires macOS (pkgbuild, productbuild).
set -eu

usage="usage: VERSION=x.y.z $0 <darwin tarball> <output.pkg>"
TARBALL=${1:?$usage}
OUT=${2:?$usage}
VERSION=${VERSION:?VERSION must be set (x.y.z, no leading v)}
RCODESIGN=${RCODESIGN:-rcodesign}
BINARY_NAME="turingtap-agent"
IDENTIFIER="ai.turingtap.agent"
PKGDIR="$(cd "$(dirname "$0")/.." && pwd)/packaging/macos"

case "$TARBALL" in
  *darwin_arm64*) HOST_ARCH=arm64 ;;
  *darwin_amd64*) HOST_ARCH=x86_64 ;;
  *) echo "error: cannot infer arch from tarball name: $TARBALL" >&2; exit 2 ;;
esac
[ -f "$TARBALL" ] || { echo "error: no such file: $TARBALL" >&2; exit 1; }

work=$(mktemp -d)
# shellcheck disable=SC2064  # expand $work now, not at trap time
trap "rm -rf '$work'" EXIT

tar -xzf "$TARBALL" -C "$work"
[ -f "$work/$BINARY_NAME" ] || {
  echo "error: $BINARY_NAME not found inside $TARBALL" >&2
  exit 1
}

if [ -n "${DEVID_KEY:-}" ] && [ -n "${DEVID_CERT:-}" ]; then
  echo "--> signing binary (hardened runtime)"
  "$RCODESIGN" sign \
    --pem-file "$DEVID_KEY" \
    --pem-file "$DEVID_CERT" \
    --binary-identifier "$IDENTIFIER" \
    --code-signature-flags runtime \
    "$work/$BINARY_NAME"
fi

root="$work/root"
mkdir -p "$root/usr/local/bin" "$root/Library/LaunchAgents"
install -m 0755 "$work/$BINARY_NAME" "$root/usr/local/bin/$BINARY_NAME"
install -m 0644 "$PKGDIR/$IDENTIFIER.plist" \
  "$root/Library/LaunchAgents/$IDENTIFIER.plist"

scriptsdir="$work/scripts"
mkdir -p "$scriptsdir"
install -m 0755 "$PKGDIR/postinstall" "$scriptsdir/postinstall"

pkgbuild \
  --root "$root" \
  --scripts "$scriptsdir" \
  --identifier "$IDENTIFIER" \
  --version "$VERSION" \
  --install-location / \
  "$work/component.pkg"

sed -e "s/@VERSION@/$VERSION/g" -e "s/@ARCH@/$HOST_ARCH/g" \
  "$PKGDIR/distribution.xml.in" > "$work/distribution.xml"

productbuild \
  --distribution "$work/distribution.xml" \
  --package-path "$work" \
  "$OUT"

echo "ok: $OUT"
