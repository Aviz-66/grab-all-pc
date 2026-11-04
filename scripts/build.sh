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
  echo "[*] Cross-compiling for Windows (x64)..."
  GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="-s -w" -o graball.exe ./daemon
  echo "[+] Windows binary created: graball.exe"
  ls -lh graball.exe
fi
