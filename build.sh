#!/usr/bin/env bash
# Compila el binario (uno por SO) en dist/, con la VERSION en el nombre. Requiere
# Go SOLO en la maquina de build; el usuario final NO compila: descarga el binario
# de su SO y lo corre. La version se toma de main.go (const version).
set -e
cd "$(dirname "$0")"
mkdir -p dist

VERSION=$(grep -oE 'const version = "[^"]+"' main.go | sed -E 's/.*"([^"]+)".*/\1/')
[ -z "$VERSION" ] && VERSION="dev"
echo "Version: v$VERSION"

# Limpiamos binarios viejos para no dejar versiones anteriores mezcladas.
rm -f dist/yogabench-* dist/SHA256SUMS.txt

L="dist/yogabench-linux-amd64-v$VERSION"
W="dist/yogabench-windows-amd64-v$VERSION.exe"
M="dist/yogabench-darwin-arm64-v$VERSION"

echo "Linux   x64  ..."; GOOS=linux   GOARCH=amd64 go build -o "$L" .
echo "Windows x64  ..."; GOOS=windows GOARCH=amd64 go build -o "$W" .
echo "macOS   arm64..."; GOOS=darwin  GOARCH=arm64 go build -o "$M" .

# Checksums (portable: sha256sum en Linux, shasum en macOS).
echo "SHA256SUMS.txt ..."
( cd dist && (command -v sha256sum >/dev/null && sha256sum yogabench-* || shasum -a 256 yogabench-*) > SHA256SUMS.txt )

echo "Listo -> dist/"
ls -lh dist
