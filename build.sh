#!/usr/bin/env bash
# Compila el binario (uno por SO) en dist/. Requiere Go SOLO en la maquina de
# build; el usuario final NO compila: descarga el binario de su SO y lo corre.
set -e
cd "$(dirname "$0")"
mkdir -p dist

echo "Linux   x64  ..."; GOOS=linux   GOARCH=amd64 go build -o dist/yogabench-linux-amd64      .
echo "Windows x64  ..."; GOOS=windows GOARCH=amd64 go build -o dist/yogabench-windows-amd64.exe .
echo "macOS   arm64..."; GOOS=darwin  GOARCH=arm64 go build -o dist/yogabench-darwin-arm64     .

echo "Listo -> dist/"
ls -lh dist
