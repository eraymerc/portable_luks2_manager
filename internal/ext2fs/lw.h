#ifndef LUKS_WEB_LW_H
#define LUKS_WEB_LW_H

#ifdef _WIN32
/* e2fsprogs'un include/mingw uyumluluk başlıkları (dev_t, uid_t ...);
 * ext2_types.h'den önce gelmeli. */
#include <sys/types.h>
#endif
#include <stdint.h>
#include <stdlib.h>
#include <ext2fs/ext2fs.h>

/* Go tarafına çağrı yapan özel io_manager. Kanal adı "lw:<handle>". */
extern io_manager lw_io_manager;

/* Dizin listeleme: her giriş için Go'daki lwDirEntry çağrılır. */
errcode_t lw_dir_list(ext2_filsys fs, ext2_ino_t dir, uintptr_t priv);

/* Dizin boş mu? (yalnız "." ve "..") */
errcode_t lw_dir_is_empty(ext2_filsys fs, ext2_ino_t dir, int *empty);

typedef struct {
	uint32_t mode;
	uint32_t uid, gid;
	uint32_t links;
	uint32_t flags;
	uint64_t size;
	int64_t  mtime, atime, ctime;
} lw_stat;

errcode_t lw_stat_ino(ext2_filsys fs, ext2_ino_t ino, lw_stat *st);

/* Sembolik bağ hedefi; buf boyutu bufsize, *len yazılan uzunluk. */
errcode_t lw_readlink(ext2_filsys fs, ext2_ino_t ino, char *buf, size_t bufsize, size_t *len);

/* Boş bir düzenli dosya oluşturup parent'a bağlar. */
errcode_t lw_create_file(ext2_filsys fs, ext2_ino_t parent, const char *name,
			 uint32_t mode, uint32_t uid, uint32_t gid, ext2_ino_t *ino);

errcode_t lw_mkdir(ext2_filsys fs, ext2_ino_t parent, const char *name,
		   uint32_t mode, uint32_t uid, uint32_t gid, ext2_ino_t *ino);

/* Girişi siler; son bağsa inode ve blokları serbest bırakır.
 * Dizinler önceden boş olmalı (çağıran denetler). */
errcode_t lw_remove(ext2_filsys fs, ext2_ino_t parent, const char *name, ext2_ino_t ino);

/* ino'yu (oldparent, oldname)'den (newparent, newname)'e taşır. */
errcode_t lw_rename(ext2_filsys fs, ext2_ino_t oldparent, const char *oldname,
		    ext2_ino_t newparent, const char *newname, ext2_ino_t ino);

/* Dosyanın ".." girişinin gösterdiği inode. */
errcode_t lw_dotdot(ext2_filsys fs, ext2_ino_t dir, ext2_ino_t *parent);

void lw_set_mtime(ext2_filsys fs, ext2_ino_t ino, int64_t mtime);

/* Süper blok yardımcıları (makrolar Go'dan çağrılamıyor). */
typedef struct {
	uint64_t blocks, free_blocks, reserved_blocks;
	uint32_t block_size, inodes, free_inodes;
	uint16_t state;
	uint32_t feature_compat, feature_incompat, feature_ro_compat;
	char     label[17];
	uint8_t  uuid[16];
	int64_t  mtime, wtime;
} lw_info;

void lw_get_info(ext2_filsys fs, lw_info *info);
void lw_set_state(ext2_filsys fs, uint16_t state);

#endif
