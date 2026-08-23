#!/bin/sh
# sign-macos.sh -- sign, notarize, and re-pack macOS turingtap-agent tarballs.
#
# Takes goreleaser darwin *.tar.gz archives, extracts them, signs the
# turingtap-agent binary with a Developer ID Application certificate
# (hardened runtime enabled), submits the signed binary to Apple's notary
# service, verifies the signature, and re-creates the tarball in place.
#
# Notarization note: tickets for bare Mach-O binaries cannot be stapled
# (stapling only applies to bundles, dmgs, and pkgs). Gatekeeper finds the
# ticket via an online lookup against Apple's servers on first run. This is
# expected and fine for CLI binaries distributed as tarballs.
#
# Usage:
#   scripts/sign-macos.sh [--skip-notarize] <tarball> [<tarball>...]
#
# Environment:
#   DEVID_KEY        path to PEM private key   (default: ~/.turingtap/devid.key)
#   DEVID_CERT       path to PEM certificate   (default: ~/.turingtap/devid.crt)
#   ASC_API_KEY_JSON path to unified App Store Connect API key JSON, produced
#                    by `rcodesign encode-app-store-connect-api-key`.
#                    Required unless --skip-notarize is given. Create it with:
#                      rcodesign encode-app-store-connect-api-key \
#                        -o key.json <ISSUER_ID> <KEY_ID> <AuthKey.p8>
#   RCODESIGN        rcodesign binary (default: rcodesign from PATH)
set -eu

DEVID_KEY=${DEVID_KEY:-"$HOME/.turingtap/devid.key"}
DEVID_CERT=${DEVID_CERT:-"$HOME/.turingtap/devid.crt"}
ASC_API_KEY_JSON=${ASC_API_KEY_JSON:-}
RCODESIGN=${RCODESIGN:-rcodesign}
BINARY_NAME="turingtap-agent"

skip_notarize=0
if [ "${1:-}" = "--skip-notarize" ]; then
  skip_notarize=1
  shift
fi

if [ "$#" -eq 0 ]; then
  echo "usage: $0 [--skip-notarize] <darwin tarball> [...]" >&2
  exit 2
fi

command -v "$RCODESIGN" >/dev/null 2>&1 || {
  echo "error: rcodesign not found (set RCODESIGN or install apple-codesign)" >&2
  exit 1
}
[ -f "$DEVID_KEY" ] || { echo "error: missing key: $DEVID_KEY" >&2; exit 1; }
[ -f "$DEVID_CERT" ] || { echo "error: missing cert: $DEVID_CERT" >&2; exit 1; }
if [ "$skip_notarize" -eq 0 ]; then
  if [ -z "$ASC_API_KEY_JSON" ] || [ ! -f "$ASC_API_KEY_JSON" ]; then
    echo "error: ASC_API_KEY_JSON not set/found (or pass --skip-notarize)" >&2
    exit 1
  fi
fi

for tarball in "$@"; do
  case "$tarball" in
    *darwin*.tar.gz) ;;
    *) echo "skip (not a darwin tarball): $tarball"; continue ;;
  esac
  [ -f "$tarball" ] || { echo "error: no such file: $tarball" >&2; exit 1; }

  echo "==> $tarball"
  work=$(mktemp -d)
  # shellcheck disable=SC2064  # expand $work now, not at trap time
  trap "rm -rf '$work'" EXIT

  tar -xzf "$tarball" -C "$work"
  [ -f "$work/$BINARY_NAME" ] || {
    echo "error: $BINARY_NAME not found inside $tarball" >&2
    exit 1
  }

  echo "--> signing (hardened runtime)"
  # --binary-identifier: Go linker-signed binaries carry identifier "a.out",
  # which rcodesign would otherwise preserve.
  "$RCODESIGN" sign \
    --pem-file "$DEVID_KEY" \
    --pem-file "$DEVID_CERT" \
    --binary-identifier "ai.turingtap.agent" \
    --code-signature-flags runtime \
    "$work/$BINARY_NAME"

  echo "--> verifying signature"
  "$RCODESIGN" verify "$work/$BINARY_NAME"

  if [ "$skip_notarize" -eq 0 ]; then
    echo "--> notarizing (zip upload; ticket via online lookup, no staple for bare binaries)"
    (cd "$work" && zip -q -X "$BINARY_NAME.zip" "$BINARY_NAME")
    # Submit without --wait: bare binaries cannot be stapled, so nothing in
    # this pipeline depends on the result, and Apple's queue can hold
    # first-time submissions InProgress for hours. The ticket attaches
    # server-side when processing finishes; check later with
    # `rcodesign notary-log <submission id>`.
    "$RCODESIGN" notary-submit --api-key-file "$ASC_API_KEY_JSON" \
      "$work/$BINARY_NAME.zip"
    rm -f "$work/$BINARY_NAME.zip"
  else
    echo "--> notarization skipped (--skip-notarize)"
  fi

  echo "--> re-packing"
  # Deterministic-ish tar: fixed owner, sorted names, no gzip timestamp.
  (cd "$work" && find . -mindepth 1 -maxdepth 1 -printf '%P\n' | LC_ALL=C sort | \
    tar --owner=0 --group=0 --numeric-owner -cf - -T -) | gzip -n > "$tarball.new"
  mv "$tarball.new" "$tarball"

  rm -rf "$work"
  trap - EXIT
  echo "ok: $tarball"
done

echo "done."
