#!/usr/bin/env bash
set -e

# Setup standalone yt-dlp binary in ./bin/
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(dirname "$SCRIPT_DIR")"
BIN_DIR="$ROOT_DIR/bin"

mkdir -p "$BIN_DIR"

OS="$(uname -s)"
case "$OS" in
  Linux*)
    URL="https://github.com/yt-dlp/yt-dlp/releases/latest/download/yt-dlp"
    OUT="$BIN_DIR/yt-dlp"
    ;;
  Darwin*)
    URL="https://github.com/yt-dlp/yt-dlp/releases/latest/download/yt-dlp_macos"
    OUT="$BIN_DIR/yt-dlp"
    ;;
  MINGW*|MSYS*|CYGWIN*)
    URL="https://github.com/yt-dlp/yt-dlp/releases/latest/download/yt-dlp.exe"
    OUT="$BIN_DIR/yt-dlp.exe"
    ;;
  *)
    echo "Unknown OS: $OS"
    exit 1
    ;;
esac

echo "[*] Downloading latest standalone yt-dlp from $URL ..."
curl -L --fail --progress-bar "$URL" -o "$OUT"
chmod +x "$OUT"

echo "[+] Successfully installed yt-dlp to $OUT"
"$OUT" --version
