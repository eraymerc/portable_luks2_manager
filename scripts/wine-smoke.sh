#!/usr/bin/env bash
# Windows derlemesini wine altında uçtan uca sınar (root gerekmez):
#   1. /tmp'de LUKS2+ext4 test imajı (cryptsetup reencrypt --encrypt, çevrimdışı)
#   2. build/luks-web.exe'yi ayrı bir WINEPREFIX ile çalıştırır
#   3. API: aç, listele, indir, yükle, klasör, taşı, sil, çık
#   4. İmajı yalnız cryptsetup ile bağımsız çözer, e2fsck -fn + debugfs ile doğrular
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/.." && pwd)
W=${WORK:-/tmp/luksweb-wine}
PASS='test parola ğüşıöç'
PORT=${PORT:-18765}
export WINEPREFIX=$W/prefix WINEDEBUG=-all WINEDLLOVERRIDES="mscoree,mshtml="

[ -f "$ROOT/build/luks-web.exe" ] || "$ROOT/scripts/build.sh" windows
rm -rf "$W/img" && mkdir -p "$W/img/src/klasör"
cd "$W/img"
echo "windows testi" >src/klasör/not.txt
head -c 3000000 /dev/urandom >src/r.bin
head -c 30000000 /dev/urandom >up.bin
printf '%s' "$PASS" >key
truncate -s 96M enc.img
mkfs.ext4 -q -F -b 4096 -L WINTEST -d src enc.img 80M
cryptsetup reencrypt --disable-locks --encrypt --type luks2 --reduce-device-size 16M \
	--sector-size 4096 --pbkdf argon2id --pbkdf-memory 65536 --pbkdf-force-iterations 4 \
	--key-file key --batch-mode enc.img

[ -d "$WINEPREFIX" ] || timeout 180 wineboot -i >/dev/null 2>&1 || true
cp "$ROOT/build/luks-web.exe" "$W/"
timeout 300 wine "$W/luks-web.exe" -no-browser -port "$PORT" 'Z:'"${W//\//\\}"'\img\enc.img' >run.log 2>&1 &
for _ in $(seq 1 60); do grep -q token= run.log 2>/dev/null && break; sleep 1; done
TOKEN=$(grep -o 'token=[0-9a-f]*' run.log | cut -d= -f2)
B=http://127.0.0.1:$PORT
C=(curl -sf -b jar -c jar)
H='X-LuksWeb: 1'
ok() { echo "  ✓ $*"; }

"${C[@]}" -o /dev/null "$B/?token=$TOKEN"
"${C[@]}" "$B/api/devices" | grep -q '"id":"0"' && ok aygıt bulundu
"${C[@]}" -H "$H" -d "{\"id\":\"0\",\"password\":\"$PASS\"}" "$B/api/unlock" >/dev/null && ok kilit açıldı
"${C[@]}" "$B/api/state" | grep -q '"readOnly":false' && ok yazılabilir
"${C[@]}" "$B/api/download?path=/r.bin" | cmp - src/r.bin && ok indirme
"${C[@]}" -H "$H" --data-binary @up.bin "$B/api/upload?path=/y%C3%BCkleme/alt/up.bin&mkdirs=1" >/dev/null && ok yükleme
"${C[@]}" "$B/api/download?path=/y%C3%BCkleme/alt/up.bin" | cmp - up.bin && ok yükleneni geri okuma
"${C[@]}" -H "$H" -d '{"path":"/silinecek"}' "$B/api/mkdir" >/dev/null && ok klasör
"${C[@]}" -H "$H" -d '{"from":"/r.bin","to":"/klasör/r-taşındı.bin"}' "$B/api/rename" >/dev/null && ok taşıma
"${C[@]}" -H "$H" -d '{"path":"/silinecek"}' "$B/api/delete" >/dev/null && ok silme
"${C[@]}" -H "$H" -X POST "$B/api/quit" >/dev/null && ok çıkış
sleep 2

# Bağımsız doğrulama: ana anahtarı dışa al, veriyi offset 0'lı ayrık başlıkla sarıp çöz.
off=$(cryptsetup luksDump --disable-locks --dump-json-metadata enc.img | python3 -c 'import json,sys;print(json.load(sys.stdin)["segments"]["0"]["offset"])')
cryptsetup luksDump --disable-locks --dump-volume-key --volume-key-file mk --key-file key --batch-mode enc.img >/dev/null
dd if=enc.img of=dec.img bs=1M skip=$((off >> 20)) status=none
rm -f hdr && truncate -s 16M hdr
cryptsetup luksFormat --disable-locks --type luks2 --header hdr --offset 0 --cipher aes-xts-plain64 \
	--key-size 512 --sector-size 4096 --volume-key-file mk --key-file key \
	--pbkdf pbkdf2 --pbkdf-force-iterations 1000 --batch-mode dec.img
cryptsetup reencrypt --disable-locks --force-offline-reencrypt --decrypt --header hdr \
	--key-file key --batch-mode dec.img
rm -f mk
e2fsck -fn dec.img >/dev/null && ok "e2fsck -fn temiz"
debugfs -R 'cat "/yükleme/alt/up.bin"' dec.img 2>/dev/null | cmp - up.bin && ok "debugfs: yüklenen dosya"
debugfs -R 'cat "/klasör/r-taşındı.bin"' dec.img 2>/dev/null | cmp - src/r.bin && ok "debugfs: taşınan dosya"
debugfs -R stats dec.img 2>/dev/null | grep -q 'Filesystem state: *clean$' && ok "süper blok: clean"
echo "wine testi BAŞARILI"
