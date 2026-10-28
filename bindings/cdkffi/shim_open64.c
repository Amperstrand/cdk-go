/* AMPESTRAND experimental/arch: aarch64-musl lacks the LFS64 symbol
 * variants (open64, stat64, mmap64, ...) that the bundled sqlite3.c in
 * libcdk_ffi.a references; on 64-bit targets they are identical to the
 * plain versions. The complete undefined set was enumerated from the
 * archive with nm. x86_64-musl exports these natively, so the shim is
 * a no-op there. */
#if defined(__linux__) && defined(__aarch64__)
#define _GNU_SOURCE
#include <fcntl.h>
#include <stdarg.h>
#include <sys/mman.h>
#include <sys/stat.h>
#include <sys/types.h>
#include <unistd.h>

int open64(const char *path, int flags, ...)
{
    int mode = 0;
    va_list ap;
    va_start(ap, flags);
    mode = va_arg(ap, int);
    va_end(ap);
    return open(path, flags, mode);
}

int fcntl64(int fd, int cmd, ...)
{
    void *arg = NULL;
    va_list ap;
    va_start(ap, cmd);
    arg = va_arg(ap, void *);
    va_end(ap);
    return fcntl(fd, cmd, arg);
}

int stat64(const char *path, struct stat *st) { return stat(path, st); }
int lstat64(const char *path, struct stat *st) { return lstat(path, st); }
int fstat64(int fd, struct stat *st) { return fstat(fd, st); }
int ftruncate64(int fd, off_t len) { return ftruncate(fd, len); }
ssize_t pread64(int fd, void *buf, size_t n, off_t off) { return pread(fd, buf, n, off); }
ssize_t pwrite64(int fd, const void *buf, size_t n, off_t off) { return pwrite(fd, buf, n, off); }
void *mmap64(void *addr, size_t len, int prot, int flags, int fd, off_t off)
{
    return mmap(addr, len, prot, flags, fd, off);
}
#endif
