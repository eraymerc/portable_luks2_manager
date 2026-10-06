#!/usr/bin/env bash
# luks-web'i derler.
#   scripts/build.sh            -> build/luks-web (Linux) ve build/luks-web.exe (Windows)
#   scripts/build.sh linux|windows
# libext2fs yoksa önce scripts/build-libext2fs.sh çalıştırılır.
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/.." && pwd)
cd "$ROOT"
VERSION=$(git describe --always --dirty 2>/dev/null || date +%Y%m%d)
LDFLAGS="-s -w -X main.version=$VERSION"

targets=("$@")
[ ${#targets[@]} -gt 0 ] || targets=(linux windows)
mkdir -p build

for t in "${targets[@]}"; do
	[ -f "build/e2fs/$t/lib/libext2fs.a" ] || scripts/build-libext2fs.sh "$t"
	case $t in
	linux)
		CGO_ENABLED=1 go build -trimpath -ldflags "$LDFLAGS" -o build/luks-web ./cmd/luks-web
		echo "build/luks-web"
		;;
	windows)
		# Yönetici izni isteyen manifest'i kaynak (.syso) olarak göm.
		syso=cmd/luks-web/rsrc_windows_amd64.syso
		printf '1 24 "luks-web.manifest"\n' >cmd/luks-web/rsrc.rc
		(cd cmd/luks-web && x86_64-w64-mingw32-windres -O coff -i rsrc.rc -o "$(basename "$syso")")
		rm -f cmd/luks-web/rsrc.rc
		CGO_ENABLED=1 GOOS=windows GOARCH=amd64 CC=x86_64-w64-mingw32-gcc \
			go build -trimpath -ldflags "$LDFLAGS -extldflags=-static" -o build/luks-web.exe ./cmd/luks-web
		echo "build/luks-web.exe"
		;;
	*) echo "bilinmeyen hedef: $t" >&2; exit 1 ;;
	esac
done
