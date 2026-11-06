#!/usr/bin/env bash
set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(dirname "$SCRIPT_DIR")"
cd "$ROOT_DIR"

echo "[*] Building GrabAll daemon (static, stripped binary)..."
CGO_ENABLED=0 go build -ldflags="-s -w" -o graball ./daemon

echo "[+] Build completed successfully!"
ls -lh graball

if [ "$1" == "--all" ] || [ "$1" == "--windows" ]; then
  if [ ! -f "daemon/rsrc_windows_amd64.syso" ] && [ -f "assets/icons/graball.ico" ]; then
    echo "[*] Generating Windows PE resource object from assets/icons/graball.ico..."
    go run github.com/akavel/rsrc@latest -arch amd64 -ico assets/icons/graball.ico -o daemon/rsrc_windows_amd64.syso
  fi
  echo "[*] Cross-compiling Native Win32 GUI app for Windows (x64, no CMD window)..."
  GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="-s -w -H=windowsgui" -o graball.exe ./daemon
  echo "[+] Windows GUI binary created: graball.exe"
  ls -lh graball.exe
fi
