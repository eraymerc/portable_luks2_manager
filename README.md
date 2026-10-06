# luks_web

Windows'ta LUKS2 + ext4 şifreli harici diske web arayüzünden erişmek için taşınabilir araç.

Diskin küçük exFAT bölümünde (`TOOLS`) duran `luks-web.exe` çalıştırılır, tarayıcıda
`http://127.0.0.1:<port>/?token=...` açılır, parola girilir, dosyalar gezilir/indirilir/yüklenir.

## Durum

| Aşama | Durum |
|---|---|
| 1. LUKS2 açma (başlık, argon2id/argon2i/pbkdf2, AF-merge, digest) | ✅ cryptsetup'a karşı test edildi |
| 2. Sektör çözme (aes-xts-plain64, 512/4096 sektör) | ✅ |
| 3. libext2fs (Linux + mingw, özel io_manager) | ✅ |
| 4. Web arayüzü, Windows ham disk erişimi, wine testi | ✅ (wine'da imaj dosyasıyla) |
| 5. Yazma (yükle, klasör, taşı, sil) | ✅ her testten sonra `e2fsck -fn` temiz |
| 6. Gerçek Windows'ta `\\.\PhysicalDriveN` testi | ⏳ yapılmadı — disk bölümlenmeden önce şart |

## Kullanım

**Windows:** `luks-web.exe`'ye çift tıklayın (yönetici izni ister; ham disk erişimi için
gerekli). Tarayıcı açılır; diski seçip parolayı girin. İşiniz bitince arayüzde **Çık**'a basın.

```
luks-web [-port N] [-no-browser] [imaj-veya-aygıt ...]
```

Yol verilmezse Windows'ta tüm fiziksel diskler taranır ve LUKS2 bölümleri listelenir.
Linux'ta imaj ya da aygıt yolu verilir (`luks-web disk.img`, `luks-web /dev/sdb`).

Arayüz: klasör gezme, dosya indirme (Range/devam desteği), klasörü zip indirme, dosya ve
klasör yükleme (sürükle-bırak dahil), yeni klasör, yeniden adlandırma/taşıma, (özyinelemeli) silme.

## Mimari

```
Tarayıcı (127.0.0.1 + rastgele token)
   │
Web sunucusu (Go)                 internal/web     — HTTP API + gömülü arayüz
   │
libext2fs (e2fsprogs, C, cgo)     internal/ext2fs  — ext4 mantığı e2fsprogs'un
   │
Özel io_manager + önbellek        internal/ext2fs  — lw.c, cache.go
   │
LUKS2: AES-XTS şifrele/çöz        internal/luks2
   │
Ham disk / bölüm / imaj           internal/blockdev — \\.\PhysicalDriveN, GPT/MBR
```

## Hedef disk düzeni (araç Windows'ta test edilene kadar diske DOKUNULMAYACAK)

| Bölüm | Boyut | İçerik |
|---|---|---|
| sda1 | 128 MB | exFAT `TOOLS`: luks-web.exe + README |
| sda2 | kalan | LUKS2 → ext4 |

Hazırlık (ileride, **diskteki her şeyi siler**, `sdX`'i iki kez kontrol edin):

```sh
sudo sfdisk /dev/sdX <<'EOF'
label: gpt
size=128MiB, type=EBD0A0A2-B9E5-4433-87C0-68B6B72699C7, name=TOOLS
type=0FC63DAF-8483-4772-8E79-3D69D8477DE4, name=VERI
EOF
sudo mkfs.exfat -L TOOLS /dev/sdX1
sudo cryptsetup luksFormat --type luks2 --sector-size 4096 --pbkdf argon2id /dev/sdX2
sudo cryptsetup open /dev/sdX2 veri && sudo mkfs.ext4 -L VERI /dev/mapper/veri && sudo cryptsetup close veri
```

İkinci bölümün türü "Linux filesystem" olduğu için Windows ona sürücü harfi atamaz ve
"diski biçimlendirin" uyarısı çıkmaz. Argon2id bellek maliyeti cryptsetup varsayılanıyla
1 GB'a kadar çıkabilir; açılışta Windows makinede o kadar boş RAM gerekir.

## Güvenlik ve tutarlılık modeli

- Sunucu yalnızca `127.0.0.1`'e bağlanır. Konsoldaki bağlantıdaki token ilk istekte
  HttpOnly/SameSite=Strict çereze çevrilir ve adresten silinir.
- `Host` denetlenir (DNS rebinding). POST isteklerinde `X-LuksWeb` başlığı ve `Origin`
  denetlenir (CSRF). CSP ve `frame-ancestors 'none'` açık.
- Ana anahtar yalnızca bellekte tutulur. Kilitlenince disk kapatılır.
- **Yazma:** Bir işlem başlarken süper blok "temiz değil" işaretlenip diske yazılır.
  İşlem bitince tüm meta veri yazılır ve süper blok yeniden "temiz" işaretlenir. Böylece
  disk boştayken çekilirse sorun olmaz. Bir yükleme sürerken kesilirse Linux fsck ister.
- Şu durumlarda dosya sistemi **salt-okunur** açılır ve arayüzde sebebi gösterilir:
  - `needs_recovery` (günlük kurtarma bekliyor)
  - temiz kapatılmamış
  - hata kaydı var
  - MMP
  - fscrypt
  - bilinmeyen ro_compat özelliği
- Yarım kalan ya da başarısız yükleme silinir. Var olan dosyanın üzerine yazma, önce geçici
  bir ada yazılıp sonra yer değiştirerek yapılır. Kökün ayrılmış blokları kullanılmaz.
- Kapatma (Çık / Ctrl+C / konsol penceresini kapatma) süren yüklemelerin iptal edilip
  temizlenmesini bekler.

## Derleme

Gerekenler: Go ≥ 1.24, gcc, `x86_64-w64-mingw32-gcc` (Windows için).

```sh
scripts/build.sh            # build/luks-web ve build/luks-web.exe
scripts/build.sh windows    # yalnız exe
```

`scripts/build-libext2fs.sh`, e2fsprogs 1.47.4'ü indirir (sha256 denetimli), `/tmp`'de
derler ve `build/e2fs/<hedef>/` altına statik `libext2fs.a` + `libcom_err.a` koyar.
`build.sh` gerekirse bunu kendisi çağırır. Yardımcı araçlar (subst, compile_et vb.) her
zaman yerel gcc ile derlenir. Aksi halde mingw `.exe`'leri binfmt üzerinden wine'ı tetikler.

## Test

Test imajları `/tmp` altında oluşturulur (`/home` dolu). Root gerekmez. Doğrulamayı
bağımsız araçlar (hakemler) yapar:

- İmaj üretimi: `mkfs.ext4 -d` + `cryptsetup reencrypt --encrypt` (çevrimdışı).
- Çözme: cryptsetup gömülü başlığı yerinde çözmüyor. Bu yüzden ana anahtar
  `luksDump --dump-volume-key` ile alınır, veri offset 0'lı ayrık bir başlıkla yeniden
  sarılır ve `reencrypt --decrypt` ile çözülür.
- Her yazma testinden sonra `e2fsck -fn` temiz demeli. İçerik `debugfs` ile karşılaştırılır.

```sh
go test ./...                 # luks2, ext2fs (5 FS varyantı), blockdev (GPT düzeni), web (uçtan uca)
scripts/wine-smoke.sh         # Windows exe'si wine altında, ayrı WINEPREFIX ile
go test -run x -bench . ./internal/ext2fs   # hız (tmpfs'te ~700 MB/s yükleme, ~540 MB/s indirme)
```

## Yol boyunca öğrenilenler

- **IV birimi:** LUKS2, 4096 baytlık sektörde de IV'yi **512 baytlık birimde** sayar
  (`iv_large_sectors` kullanılmaz). Testle doğrulandı.
- **inline_data dizinleri:** libext2fs `..` girdisini `DIRENT_DOT_DOT_FILE` olarak değil
  sıradan bir girdi olarak veriyor. Nokta girdileri ada göre ayıklanıyor. Aksi halde
  özyinelemeli silme `..`'ya inerdi.
- **inline_data okuma:** `ext2fs_file_read` dosya sonunu aşan isteği sıfırla doldurup
  okunmuş sayıyor. İstekler dosya boyutuyla sınırlanıyor (`debugfs cat` de bundan etkileniyor).
- **Zaman damgası:** e2fsprogs 1.47.4'teki `ext2fs_inode_xtime_set` geniş inode'larda
  saniyeyi 28 bitle maskeliyor. `lw.c` kendi makrosunu kullanıyor.
- **Dizin büyütme:** `ext2fs_link` düz dizini ancak `EXT2FS_LINK_EXPAND` verilirse büyütüyor.
- **Hız:** libext2fs her veri bloğundan sonra inode tablosunu yazıyor (4K/8K dönüşümlü
  yazmalar). Geri-yazmalı önbellek bunları birleştiriyor (86 → ~700 MB/s). AES-XTS
  çekirdeklere bölünüyor.

## Bilinen sınırlar

- Yalnız `aes-xts-plain64` / `aes-xts-plain` ve tek segmentli LUKS2. Bunlar yok: dm-integrity,
  yarım kalmış yeniden şifreleme, LUKS1.
- Günlük (journal) kullanılmaz. Yazmalar doğrudan yapılır, tutarlılığı yukarıdaki kirli
  işaret modeli korur.
- Sembolik bağlar gösterilir ve izlenir ama oluşturulmaz. İzinler/sahiplik değiştirilmez.
  Yeni dosyalar üst dizinin sahibini alır (0644/0755).
- Gerçek `\\.\PhysicalDriveN` erişimi henüz yalnızca kodda var, Windows'ta denenmedi (aşama 6).
