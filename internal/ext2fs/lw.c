#include <errno.h>
#include <string.h>
#include <time.h>
#include "lw.h"
#include "_cgo_export.h"

/*
 * e2fsprogs 1.47.4'teki ext2fs_inode_xtime_set makrosu genişletilmiş alanlı
 * inode'larda saniyeyi 0xfffffff (28 bit) ile maskeliyor; bugünkü tarihler
 * bozuluyor. Kendi sürümümüz:
 */
#define LW_XTIME_SET(inode, field, sec)						\
do {										\
	int64_t __s = (sec);							\
	(inode)->field = (__u32)__s;						\
	if (ext2fs_inode_includes(ext2fs_inode_actual_size(inode),		\
				  field ## _extra))				\
		((struct ext2_inode_large *)(inode))->field ## _extra =		\
			__encode_extra_time(__s, 0);				\
} while (0)

/* ---------------- io_manager ---------------- */

struct lw_private {
	uintptr_t handle;
};

static errcode_t lw_open(const char *name, int flags, io_channel *channel);

static errcode_t lw_close(io_channel channel)
{
	if (--channel->refcount > 0)
		return 0;
	free(channel->private_data);
	free(channel->name);
	free(channel);
	return 0;
}

static errcode_t lw_set_blksize(io_channel channel, int blksize)
{
	channel->block_size = blksize;
	return 0;
}

static size_t lw_size(io_channel channel, int count)
{
	if (count < 0)
		return (size_t)(-count);
	return (size_t)count * channel->block_size;
}

static errcode_t lw_read_blk64(io_channel channel, unsigned long long block,
			       int count, void *data)
{
	struct lw_private *p = channel->private_data;
	size_t size = lw_size(channel, count);
	errcode_t err = lwRead(p->handle, block * channel->block_size, data, size);

	if (err && channel->read_error)
		err = (channel->read_error)(channel, block, count, data, size, 0, err);
	return err;
}

static errcode_t lw_read_blk(io_channel channel, unsigned long block,
			     int count, void *data)
{
	return lw_read_blk64(channel, block, count, data);
}

static errcode_t lw_write_blk64(io_channel channel, unsigned long long block,
				int count, const void *data)
{
	struct lw_private *p = channel->private_data;
	size_t size = lw_size(channel, count);
	errcode_t err = lwWrite(p->handle, block * channel->block_size, (void *)data, size);

	if (err && channel->write_error)
		err = (channel->write_error)(channel, block, count, data, size, 0, err);
	return err;
}

static errcode_t lw_write_blk(io_channel channel, unsigned long block,
			      int count, const void *data)
{
	return lw_write_blk64(channel, block, count, data);
}

static errcode_t lw_write_byte(io_channel channel, unsigned long offset,
			       int size, const void *data)
{
	struct lw_private *p = channel->private_data;

	if (size < 0)
		return EXT2_ET_INVALID_ARGUMENT;
	return lwWrite(p->handle, offset, (void *)data, (size_t)size);
}

static errcode_t lw_flush(io_channel channel)
{
	struct lw_private *p = channel->private_data;

	return lwFlush(p->handle);
}

static errcode_t lw_set_option(io_channel channel, const char *option,
			       const char *arg)
{
	return EXT2_ET_INVALID_ARGUMENT;
}

static struct struct_io_manager lw_struct_manager = {
	.magic		= EXT2_ET_MAGIC_IO_MANAGER,
	.name		= "luks_web I/O Manager",
	.open		= lw_open,
	.close		= lw_close,
	.set_blksize	= lw_set_blksize,
	.read_blk	= lw_read_blk,
	.write_blk	= lw_write_blk,
	.flush		= lw_flush,
	.write_byte	= lw_write_byte,
	.set_option	= lw_set_option,
	.read_blk64	= lw_read_blk64,
	.write_blk64	= lw_write_blk64,
};

io_manager lw_io_manager = &lw_struct_manager;

static errcode_t lw_open(const char *name, int flags, io_channel *channel)
{
	io_channel io;
	struct lw_private *p;
	char *end;
	unsigned long long h;

	if (!name || strncmp(name, "lw:", 3) != 0)
		return EXT2_ET_BAD_DEVICE_NAME;
	h = strtoull(name + 3, &end, 10);
	if (*end || h == 0)
		return EXT2_ET_BAD_DEVICE_NAME;

	io = calloc(1, sizeof(*io));
	p = calloc(1, sizeof(*p));
	if (!io || !p) {
		free(io);
		free(p);
		return EXT2_ET_NO_MEMORY;
	}
	io->name = strdup(name);
	if (!io->name) {
		free(io);
		free(p);
		return EXT2_ET_NO_MEMORY;
	}
	p->handle = (uintptr_t)h;
	io->magic = EXT2_ET_MAGIC_IO_CHANNEL;
	io->manager = lw_io_manager;
	io->block_size = 1024;
	io->refcount = 1;
	io->flags = CHANNEL_FLAGS_BLOCK_DEVICE;
	io->private_data = p;
	*channel = io;
	return 0;
}

/* ---------------- dizinler ---------------- */

/*
 * "." ve ".." ada göre tanınır: inline_data dizinlerinde libext2fs ".."
 * girdisini DIRENT_DOT_DOT_FILE yerine DIRENT_OTHER_FILE olarak veriyor.
 */
static int lw_is_dot(struct ext2_dir_entry *d)
{
	int len = ext2fs_dirent_name_len(d);
	return (len == 1 && d->name[0] == '.') ||
	       (len == 2 && d->name[0] == '.' && d->name[1] == '.');
}

static int lw_is_dotdot(struct ext2_dir_entry *d)
{
	return ext2fs_dirent_name_len(d) == 2 && d->name[0] == '.' && d->name[1] == '.';
}

static int lw_dir_cb(ext2_ino_t dir, int entry, struct ext2_dir_entry *dirent,
		     int offset, int blocksize, char *buf, void *priv)
{
	int len = ext2fs_dirent_name_len(dirent);

	if (dirent->inode == 0 || lw_is_dot(dirent))
		return 0;
	return lwDirEntry((uintptr_t)priv, dirent->name, len, dirent->inode,
			  ext2fs_dirent_file_type(dirent));
}

errcode_t lw_dir_list(ext2_filsys fs, ext2_ino_t dir, uintptr_t priv)
{
	return ext2fs_dir_iterate2(fs, dir, 0, NULL, lw_dir_cb, (void *)priv);
}

static int lw_empty_cb(ext2_ino_t dir, int entry, struct ext2_dir_entry *dirent,
		       int offset, int blocksize, char *buf, void *priv)
{
	if (dirent->inode == 0 || lw_is_dot(dirent))
		return 0;
	*(int *)priv = 0;
	return DIRENT_ABORT;
}

errcode_t lw_dir_is_empty(ext2_filsys fs, ext2_ino_t dir, int *empty)
{
	*empty = 1;
	return ext2fs_dir_iterate2(fs, dir, 0, NULL, lw_empty_cb, empty);
}

struct dotdot_ctx {
	ext2_ino_t parent;
	ext2_ino_t newval; /* 0 ise yalnız oku */
	int found;
};

static int lw_dotdot_cb(ext2_ino_t dir, int entry, struct ext2_dir_entry *dirent,
			int offset, int blocksize, char *buf, void *priv)
{
	struct dotdot_ctx *c = priv;

	if (dirent->inode == 0 || !lw_is_dotdot(dirent))
		return 0;
	c->found = 1;
	c->parent = dirent->inode;
	if (c->newval) {
		dirent->inode = c->newval;
		return DIRENT_CHANGED | DIRENT_ABORT;
	}
	return DIRENT_ABORT;
}

errcode_t lw_dotdot(ext2_filsys fs, ext2_ino_t dir, ext2_ino_t *parent)
{
	struct dotdot_ctx c = {0};
	errcode_t err = ext2fs_dir_iterate2(fs, dir, DIRENT_FLAG_INCLUDE_EMPTY,
					    NULL, lw_dotdot_cb, &c);
	if (err)
		return err;
	if (!c.found)
		return EXT2_ET_FILE_NOT_FOUND;
	*parent = c.parent;
	return 0;
}

static errcode_t lw_set_dotdot(ext2_filsys fs, ext2_ino_t dir, ext2_ino_t newparent)
{
	struct dotdot_ctx c = {0};
	errcode_t err;

	c.newval = newparent;
	err = ext2fs_dir_iterate2(fs, dir, DIRENT_FLAG_INCLUDE_EMPTY, NULL,
				  lw_dotdot_cb, &c);
	if (err)
		return err;
	return c.found ? 0 : EXT2_ET_FILE_NOT_FOUND;
}

/* ---------------- inode bilgisi ---------------- */

static uint32_t lw_uid(struct ext2_inode_large *in)
{
	return inode_uid(*in);
}

static uint32_t lw_gid(struct ext2_inode_large *in)
{
	return inode_gid(*in);
}

static void lw_set_owner(struct ext2_inode_large *in, uint32_t uid, uint32_t gid)
{
	in->i_uid = uid & 0xffff;
	ext2fs_set_i_uid_high(*in, uid >> 16);
	in->i_gid = gid & 0xffff;
	ext2fs_set_i_gid_high(*in, gid >> 16);
}

static errcode_t lw_read_large(ext2_filsys fs, ext2_ino_t ino, struct ext2_inode_large *in)
{
	memset(in, 0, sizeof(*in));
	return ext2fs_read_inode_full(fs, ino, (struct ext2_inode *)in, sizeof(*in));
}

static errcode_t lw_write_large(ext2_filsys fs, ext2_ino_t ino, struct ext2_inode_large *in)
{
	return ext2fs_write_inode_full(fs, ino, (struct ext2_inode *)in, sizeof(*in));
}

errcode_t lw_stat_ino(ext2_filsys fs, ext2_ino_t ino, lw_stat *st)
{
	struct ext2_inode_large in;
	errcode_t err = lw_read_large(fs, ino, &in);

	if (err)
		return err;
	st->mode = in.i_mode;
	st->uid = lw_uid(&in);
	st->gid = lw_gid(&in);
	st->links = in.i_links_count;
	st->flags = in.i_flags;
	st->size = EXT2_I_SIZE(&in);
	st->mtime = ext2fs_inode_xtime_get(&in, i_mtime);
	st->atime = ext2fs_inode_xtime_get(&in, i_atime);
	st->ctime = ext2fs_inode_xtime_get(&in, i_ctime);
	return 0;
}

void lw_set_mtime(ext2_filsys fs, ext2_ino_t ino, int64_t mtime)
{
	struct ext2_inode_large in;

	if (lw_read_large(fs, ino, &in))
		return;
	LW_XTIME_SET(&in, i_mtime, mtime);
	lw_write_large(fs, ino, &in);
}

errcode_t lw_readlink(ext2_filsys fs, ext2_ino_t ino, char *buf, size_t bufsize, size_t *len)
{
	struct ext2_inode_large in;
	errcode_t err = lw_read_large(fs, ino, &in);
	uint64_t size;

	if (err)
		return err;
	if (!LINUX_S_ISLNK(in.i_mode))
		return EXT2_ET_INVALID_ARGUMENT;
	size = EXT2_I_SIZE(&in);
	if (size >= bufsize)
		return EXT2_ET_INVALID_ARGUMENT;

	if (in.i_flags & EXT4_INLINE_DATA_FL) {
		size_t sz = 0;
		char *tmp = calloc(1, EXT2_INODE_SIZE(fs->super) + EXT4_MIN_INLINE_DATA_SIZE);
		if (!tmp)
			return EXT2_ET_NO_MEMORY;
		err = ext2fs_inline_data_get(fs, ino, (struct ext2_inode *)&in, tmp, &sz);
		if (!err) {
			if (sz > size)
				sz = size;
			memcpy(buf, tmp, sz);
			*len = sz;
		}
		free(tmp);
		return err;
	}
	if (ext2fs_is_fast_symlink((struct ext2_inode *)&in)) {
		memcpy(buf, (char *)in.i_block, size);
		*len = size;
		return 0;
	}
	{
		ext2_file_t f;
		unsigned int got = 0;
		err = ext2fs_file_open2(fs, ino, (struct ext2_inode *)&in, 0, &f);
		if (err)
			return err;
		err = ext2fs_file_read(f, buf, size, &got);
		ext2fs_file_close(f);
		*len = got;
		return err;
	}
}

/* ---------------- oluşturma ---------------- */

errcode_t lw_create_file(ext2_filsys fs, ext2_ino_t parent, const char *name,
			 uint32_t mode, uint32_t uid, uint32_t gid, ext2_ino_t *ret)
{
	struct ext2_inode_large in;
	ext2_ino_t ino;
	errcode_t err;
	time_t now = fs->now ? fs->now : time(NULL);

	err = ext2fs_new_inode(fs, parent, LINUX_S_IFREG | 0644, 0, &ino);
	if (err)
		return err;
	err = ext2fs_link(fs, parent, name, ino, EXT2_FT_REG_FILE | EXT2FS_LINK_EXPAND);
	if (err)
		return err;
	ext2fs_inode_alloc_stats2(fs, ino, +1, 0);

	memset(&in, 0, sizeof(in));
	in.i_mode = LINUX_S_IFREG | (mode & 07777);
	lw_set_owner(&in, uid, gid);
	LW_XTIME_SET(&in, i_atime, now);
	LW_XTIME_SET(&in, i_ctime, now);
	LW_XTIME_SET(&in, i_mtime, now);
	in.i_links_count = 1;

	if (ext2fs_has_feature_inline_data(fs->super)) {
		in.i_flags |= EXT4_INLINE_DATA_FL;
	} else if (ext2fs_has_feature_extents(fs->super)) {
		ext2_extent_handle_t handle;

		in.i_flags &= ~EXT4_EXTENTS_FL;
		err = ext2fs_extent_open2(fs, ino, (struct ext2_inode *)&in, &handle);
		if (err)
			goto undo;
		ext2fs_extent_free(handle);
	}
	err = ext2fs_write_new_inode(fs, ino, (struct ext2_inode *)&in);
	if (err)
		goto undo;
	if (in.i_flags & EXT4_INLINE_DATA_FL) {
		err = ext2fs_inline_data_init(fs, ino);
		if (err)
			goto undo;
	}
	*ret = ino;
	return 0;
undo:
	ext2fs_unlink(fs, parent, name, ino, 0);
	ext2fs_inode_alloc_stats2(fs, ino, -1, 0);
	return err;
}

errcode_t lw_mkdir(ext2_filsys fs, ext2_ino_t parent, const char *name,
		   uint32_t mode, uint32_t uid, uint32_t gid, ext2_ino_t *ret)
{
	struct ext2_inode_large in, pin;
	ext2_ino_t ino = 0;
	errcode_t err;
	int overflow;

	err = lw_read_large(fs, parent, &pin);
	if (err)
		return err;
	/* ext2fs_mkdir2 üst dizinin bağ sayısını dir_nlink taşmasını
	 * gözetmeden artırıyor; taşacaksa sonradan 1 yap. */
	overflow = pin.i_links_count == 1 || pin.i_links_count + 1 >= EXT2_LINK_MAX;
	if (overflow && !ext2fs_has_feature_dir_nlink(fs->super))
		return EMLINK;

	err = ext2fs_mkdir2(fs, parent, 0, 0, EXT2FS_LINK_EXPAND, name, &ino);
	if (err)
		return err;
	if (overflow) {
		err = lw_read_large(fs, parent, &pin);
		if (!err) {
			pin.i_links_count = 1;
			err = lw_write_large(fs, parent, &pin);
		}
		if (err)
			return err;
	}
	err = lw_read_large(fs, ino, &in);
	if (err)
		return err;
	in.i_mode = LINUX_S_IFDIR | (mode & 07777);
	lw_set_owner(&in, uid, gid);
	err = lw_write_large(fs, ino, &in);
	if (err)
		return err;
	*ret = ino;
	return 0;
}

/* ---------------- silme / taşıma ---------------- */

/* Dizin bağ sayısı: 2 + alt dizin sayısı; dir_nlink ile taşınca 1 kalır. */
static errcode_t lw_adjust_links(ext2_filsys fs, ext2_ino_t ino, int delta)
{
	struct ext2_inode_large in;
	errcode_t err = lw_read_large(fs, ino, &in);
	time_t now = fs->now ? fs->now : time(NULL);

	if (err)
		return err;
	if (delta < 0) {
		if (in.i_links_count > 2)
			in.i_links_count--;
	} else if (in.i_links_count != 1) {
		if (in.i_links_count + 1 >= EXT2_LINK_MAX)
			in.i_links_count = 1;
		else
			in.i_links_count++;
	}
	LW_XTIME_SET(&in, i_mtime, now);
	LW_XTIME_SET(&in, i_ctime, now);
	return lw_write_large(fs, ino, &in);
}

static errcode_t lw_touch_dir(ext2_filsys fs, ext2_ino_t ino)
{
	struct ext2_inode_large in;
	errcode_t err = lw_read_large(fs, ino, &in);
	time_t now = fs->now ? fs->now : time(NULL);

	if (err)
		return err;
	LW_XTIME_SET(&in, i_mtime, now);
	LW_XTIME_SET(&in, i_ctime, now);
	return lw_write_large(fs, ino, &in);
}

errcode_t lw_remove(ext2_filsys fs, ext2_ino_t parent, const char *name, ext2_ino_t ino)
{
	struct ext2_inode_large in;
	errcode_t err;
	int isdir;
	blk64_t ea;
	time_t now = fs->now ? fs->now : time(NULL);

	err = lw_read_large(fs, ino, &in);
	if (err)
		return err;
	isdir = LINUX_S_ISDIR(in.i_mode);

	err = ext2fs_unlink(fs, parent, name, ino, 0);
	if (err)
		return err;
	if (isdir)
		err = lw_adjust_links(fs, parent, -1);
	else
		err = lw_touch_dir(fs, parent);
	if (err)
		return err;

	if (!isdir && in.i_links_count > 1) {
		in.i_links_count--;
		LW_XTIME_SET(&in, i_ctime, now);
		return lw_write_large(fs, ino, &in);
	}

	/* Son bağ: blokları, xattr bloğunu ve inode'u serbest bırak. */
	if (ext2fs_inode_has_valid_blocks2(fs, (struct ext2_inode *)&in) ||
	    (in.i_flags & EXT4_INLINE_DATA_FL)) {
		err = ext2fs_punch(fs, ino, (struct ext2_inode *)&in, NULL, 0, ~0ULL);
		if (err)
			return err;
	}
	ea = ext2fs_file_acl_block(fs, (struct ext2_inode *)&in);
	if (ea) {
		__u32 count = 0;
		err = ext2fs_adjust_ea_refcount3(fs, ea, NULL, -1, &count, ino);
		if (err)
			return err;
		if (count == 0)
			ext2fs_block_alloc_stats2(fs, ea, -1);
		ext2fs_file_acl_block_set(fs, (struct ext2_inode *)&in, 0);
	}
	in.i_links_count = 0;
	in.i_dtime = now;
	err = lw_write_large(fs, ino, &in);
	if (err)
		return err;
	ext2fs_inode_alloc_stats2(fs, ino, -1, isdir);
	return 0;
}

static int lw_file_type(unsigned int mode)
{
	if (LINUX_S_ISREG(mode))  return EXT2_FT_REG_FILE;
	if (LINUX_S_ISDIR(mode))  return EXT2_FT_DIR;
	if (LINUX_S_ISCHR(mode))  return EXT2_FT_CHRDEV;
	if (LINUX_S_ISBLK(mode))  return EXT2_FT_BLKDEV;
	if (LINUX_S_ISLNK(mode))  return EXT2_FT_SYMLINK;
	if (LINUX_S_ISFIFO(mode)) return EXT2_FT_FIFO;
	if (LINUX_S_ISSOCK(mode)) return EXT2_FT_SOCK;
	return 0;
}

errcode_t lw_rename(ext2_filsys fs, ext2_ino_t oldparent, const char *oldname,
		    ext2_ino_t newparent, const char *newname, ext2_ino_t ino)
{
	struct ext2_inode_large in;
	errcode_t err;
	int isdir, ft;

	err = lw_read_large(fs, ino, &in);
	if (err)
		return err;
	isdir = LINUX_S_ISDIR(in.i_mode);
	ft = lw_file_type(in.i_mode);

	err = ext2fs_link(fs, newparent, newname, ino, ft | EXT2FS_LINK_EXPAND);
	if (err)
		return err;
	err = ext2fs_unlink(fs, oldparent, oldname, ino, 0);
	if (err) {
		ext2fs_unlink(fs, newparent, newname, ino, 0);
		return err;
	}
	if (isdir && oldparent != newparent) {
		err = lw_set_dotdot(fs, ino, newparent);
		if (err)
			return err;
		err = lw_adjust_links(fs, oldparent, -1);
		if (err)
			return err;
		err = lw_adjust_links(fs, newparent, +1);
		if (err)
			return err;
	} else {
		err = lw_touch_dir(fs, oldparent);
		if (!err && newparent != oldparent)
			err = lw_touch_dir(fs, newparent);
		if (err)
			return err;
	}
	/* Yeniden oku: inline_data dizinlerinde ".." inode'un içinde ve az önce değişti. */
	err = lw_read_large(fs, ino, &in);
	if (err)
		return err;
	LW_XTIME_SET(&in, i_ctime, fs->now ? fs->now : time(NULL));
	return lw_write_large(fs, ino, &in);
}

/* ---------------- süper blok ---------------- */

void lw_get_info(ext2_filsys fs, lw_info *info)
{
	struct ext2_super_block *sb = fs->super;

	memset(info, 0, sizeof(*info));
	info->blocks = ext2fs_blocks_count(sb);
	info->free_blocks = ext2fs_free_blocks_count(sb);
	info->reserved_blocks = ext2fs_r_blocks_count(sb);
	info->block_size = fs->blocksize;
	info->inodes = sb->s_inodes_count;
	info->free_inodes = sb->s_free_inodes_count;
	info->state = sb->s_state;
	info->feature_compat = sb->s_feature_compat;
	info->feature_incompat = sb->s_feature_incompat;
	info->feature_ro_compat = sb->s_feature_ro_compat;
	memcpy(info->label, sb->s_volume_name, 16);
	memcpy(info->uuid, sb->s_uuid, 16);
	info->mtime = sb->s_mtime;
	info->wtime = sb->s_wtime;
}

void lw_set_state(ext2_filsys fs, uint16_t state)
{
	fs->super->s_state = state;
	ext2fs_mark_super_dirty(fs);
}
