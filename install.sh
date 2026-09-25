#!/usr/bin/env bash
# codehamr installer: fetch the latest release binary and install it into a
# user writable prefix so sudo is never needed for the default flow.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/codehamr/codehamr/main/install.sh | bash
#   curl -fsSL https://raw.githubusercontent.com/codehamr/codehamr/main/install.sh | PREFIX=/usr/local bash

set -euo pipefail

clear

REPO="codehamr/codehamr"

os="$(uname -s | tr '[:upper:]' '[:lower:]')"
case "$os" in
  linux)  ;;
  darwin) os=macos ;;
  *) echo "codehamr: unsupported OS: $os (need linux or darwin)" >&2; exit 1 ;;
esac

arch="$(uname -m)"
case "$arch" in
  x86_64|amd64)  arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) echo "codehamr: unsupported arch: $arch (need amd64 or arm64)" >&2; exit 1 ;;
esac

# Pick install dir. Explicit PREFIX wins. Otherwise prefer a directory that
# is ALREADY on PATH and writable, so `codehamr` works in the current shell
# without re sourcing anything. Only when no such dir exists do we fall back
# to ~/.local/bin and plumb PATH via the user's shell rc files.
on_path() { case ":${PATH}:" in *":$1:"*) return 0 ;; *) return 1 ;; esac; }
writable_or_creatable() {
  [ -d "$1" ] && [ -w "$1" ] && return 0
  [ ! -e "$1" ] && mkdir -p "$1" 2>/dev/null && return 0
  return 1
}

if [ -n "${PREFIX:-}" ]; then
  bindir="${PREFIX}/bin"
else
  bindir=""
  for d in "$HOME/.local/bin" "/opt/homebrew/bin" "/usr/local/bin" "$HOME/bin"; do
    if on_path "$d" && writable_or_creatable "$d"; then
      bindir="$d"; break
    fi
  done
  [ -z "$bindir" ] && bindir="$HOME/.local/bin"
fi

binary="codehamr-${os}-${arch}"
url="https://github.com/${REPO}/releases/latest/download/${binary}"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

echo "▶ codehamr · ${os}/${arch}"
curl -fsSL "$url" -o "${tmp}/codehamr"

install -d "${bindir}" 2>/dev/null \
  || { echo "codehamr: cannot create ${bindir} · pick a writable PREFIX, e.g. PREFIX=\$HOME/.local bash" >&2; exit 1; }
install -m 0755 "${tmp}/codehamr" "${bindir}/codehamr" 2>/dev/null \
  || { echo "codehamr: cannot write ${bindir}/codehamr · pick a writable PREFIX, e.g. PREFIX=\$HOME/.local bash" >&2; exit 1; }

echo "✓ installed → ${bindir}/codehamr"

# If bindir isn't on PATH, append an export to existing shell rc files so
# future shells pick it up, idempotent via a fixed marker line. The current
# shell can't be mutated from this child process, so we additionally print
# one paste ready line that activates the install without a terminal restart.
if ! on_path "$bindir"; then
  line="export PATH=\"${bindir}:\$PATH\""
  marker="# codehamr-path"
  touched=0
  for rc in "$HOME/.zshrc" "$HOME/.bashrc" "$HOME/.bash_profile" "$HOME/.profile"; do
    [ -f "$rc" ] || continue
    grep -qsF "$marker" "$rc" && continue
    grep -qsF "${bindir}" "$rc" && continue
    printf '\n%s\n%s\n' "$marker" "$line" >> "$rc"
    echo "  ↳ added PATH entry to ${rc}"
    touched=1
  done
  echo ""
  if [ "$touched" = 1 ]; then
    echo "  new shells will pick this up automatically."
    echo "  for THIS shell, paste:"
  else
    echo "  ${bindir} is not on PATH. paste this into your shell:"
  fi
  echo "    ${line}"
  echo ""
  echo "  then type 'codehamr' to start hammering"
else
  echo ""
  echo "  type 'codehamr' to start hammering"
fi
echo ""
