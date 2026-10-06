#!/usr/bin/env bash
# e2fsprogs'tan statik libext2fs + libcom_err derler.
#   build/e2fs/<hedef>/include  ->  ext2fs/*.h, et/com_err.h
#   build/e2fs/<hedef>/lib      ->  libext2fs.a, libcom_err.a
#
# Kullanım: scripts/build-libext2fs.sh [linux|windows]...   (varsayılan: ikisi de)
#
# Not: Yardımcı araçlar (subst, compile_et, gen_crc32ctable) her zaman yerel gcc
# ile derlenir. Aksi halde mingw .exe üretilir ve binfmt üzerinden wine tetiklenir.
set -euo pipefail

VER=1.47.4
SHA256=fd5bf388cbdbe006a3d3b318d983b2948382440acc85a87f1e7d108653e8db0b
URL=https://mirrors.edge.kernel.org/pub/linux/kernel/people/tytso/e2fsprogs/v$VER/e2fsprogs-$VER.tar.xz

ROOT=$(cd "$(dirname "$0")/.." && pwd)
OUT=$ROOT/build/e2fs
# Kaynak ve ara dosyalar /tmp'de (/home dolu)
WORK=${E2FS_WORK:-/tmp/luksweb-e2fs}
SRC=$WORK/e2fsprogs-$VER
JOBS=$(nproc)

mkdir -p "$WORK" "$OUT"
if [ ! -d "$SRC" ]; then
	tarball=$WORK/e2fsprogs-$VER.tar.xz
	[ -f "$tarball" ] || curl -fsSL -o "$tarball" "$URL"
	echo "$SHA256  $tarball" | sha256sum -c -
	tar -C "$WORK" -xf "$tarball"
fi

CONF_OPTS=(--disable-nls --disable-fuse2fs --disable-uuidd --disable-e2initrd-helper
	--disable-debugfs --disable-imager --disable-resizer --disable-defrag
	--disable-tdb --disable-backtrace --disable-rpath --without-libarchive
	--disable-elf-shlibs --disable-bsd-shlibs)

build() {
	local target=$1 host=()
	case $target in
	linux) ;;
	windows) host=(--host=x86_64-w64-mingw32) ;;
	*) echo "bilinmeyen hedef: $target" >&2; exit 1 ;;
	esac
	local obj=$WORK/obj-$target dst=$OUT/$target
	rm -rf "$obj" "$dst"
	mkdir -p "$obj" "$dst/include/ext2fs" "$dst/include/et" "$dst/lib"
	echo ">> $target: configure"
	(cd "$obj" && "$SRC/configure" "${host[@]}" "${CONF_OPTS[@]}" >configure.log 2>&1)

	local mk=(make -C "$obj" "BUILD_CC=gcc"
		"BUILD_CFLAGS=-O2 -I$obj/lib -I$SRC/lib"
		"BUILD_LDFLAGS=" "BUILD_LDLIBS=")
	echo ">> $target: make"
	"${mk[@]}" -C util subst >"$obj/make.log" 2>&1
	"${mk[@]}" lib/dirpaths.h >>"$obj/make.log" 2>&1
	"${mk[@]}" -C lib/et compile_et >>"$obj/make.log" 2>&1
	"${mk[@]}" -C lib/et -j"$JOBS" libcom_err.a >>"$obj/make.log" 2>&1
	"${mk[@]}" -C lib/ext2fs ext2_err.h crc32c_table.h >>"$obj/make.log" 2>&1
	"${mk[@]}" -C lib/ext2fs -j"$JOBS" libext2fs.a >>"$obj/make.log" 2>&1

	cp "$obj/lib/ext2fs/libext2fs.a" "$obj/lib/et/libcom_err.a" "$dst/lib/"
	for h in bitops.h ext2fs.h ext2_io.h ext2_fs.h ext2_ext_attr.h ext3_extents.h \
		tdb.h qcow2.h hashmap.h; do
		cp "$SRC/lib/ext2fs/$h" "$dst/include/ext2fs/"
	done
	cp "$obj/lib/ext2fs/ext2_err.h" "$obj/lib/ext2fs/ext2_types.h" "$dst/include/ext2fs/"
	cp "$SRC/lib/et/com_err.h" "$dst/include/et/"
	# mingw: e2fsprogs'un POSIX uyumluluk başlıkları (dev_t vb.)
	if [ "$target" = windows ]; then
		cp -r "$SRC/include/mingw" "$dst/include/mingw"
	fi
	echo ">> $target: tamam -> $dst"
}

targets=("$@")
[ ${#targets[@]} -gt 0 ] || targets=(linux windows)
for t in "${targets[@]}"; do build "$t"; done
