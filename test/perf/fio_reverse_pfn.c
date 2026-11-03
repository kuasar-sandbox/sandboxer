/* Diagnostic-only fio buffer allocator. Linux x86_64/aarch64, 4 KiB pages.
 * cc -O2 -Wall -Wextra -Werror -fPIC -shared -o libfio_reverse_pfn.so fio_reverse_pfn.c
 * FIO_GEOMETRY_BUFFER=/tmp/fio-geometry-buffer LD_PRELOAD=... fio ...
 *   --iomem=mmap:/tmp/fio-geometry-buffer
 *
 * Only mmap/mmap64 calls on the exact named fd are intercepted. The original
 * file/fd remain owned by fio; actual buffer pages live in an internal memfd.
 * All MAP_FIXED operations replace pages inside our own PROT_NONE reservation.
 * Failure is explicit (MAP_FAILED + diagnostic), never an ordinary-buffer
 * fallback. PFNs are checked again after mapping, before returning to fio.
 */
#define _GNU_SOURCE
#define _LARGEFILE64_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <inttypes.h>
#include <limits.h>
#include <linux/memfd.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <sys/stat.h>
#include <sys/syscall.h>
#include <sys/types.h>
#include <unistd.h>

#define GEOMETRY_PAGE 4096u
#define GEOMETRY_MAX_BYTES (128u * 1024u * 1024u)
#define PAGEMAP_PFN_MASK ((UINT64_C(1) << 55) - 1)

struct page_slot {
	uint64_t pfn;
	off_t file_offset;
};

static void *raw_mmap(void *addr, size_t length, int prot, int flags, int fd, off_t off)
{
	return (void *)syscall(SYS_mmap, addr, length, prot, flags, fd, off);
}

static int target_fd(int fd)
{
	const char *target = getenv("FIO_GEOMETRY_BUFFER");
	char procpath[64], resolved[PATH_MAX + 1];
	ssize_t n;
	if (target == NULL)
		target = "/tmp/fio-geometry-buffer";
	if (fd < 0 || target[0] != '/')
		return 0;
	if (snprintf(procpath, sizeof(procpath), "/proc/self/fd/%d", fd) >= (int)sizeof(procpath))
		return 0;
	n = readlink(procpath, resolved, sizeof(resolved) - 1);
	if (n < 0 || (size_t)n >= sizeof(resolved) - 1)
		return 0;
	resolved[n] = '\0';
	return strcmp(resolved, target) == 0;
}

static int read_pfn(int pagemap, const void *address, uint64_t *pfn)
{
	uint64_t entry = 0;
	off_t pos = (off_t)(((uintptr_t)address / GEOMETRY_PAGE) * sizeof(entry));
	ssize_t got;
	do {
		got = pread(pagemap, &entry, sizeof(entry), pos);
	} while (got < 0 && errno == EINTR);
	if (got != (ssize_t)sizeof(entry)) {
		if (got >= 0)
			errno = EIO;
		return -1;
	}
	if ((entry & (UINT64_C(1) << 63)) == 0 ||
	    (entry & (UINT64_C(1) << 62)) != 0) {
		errno = EFAULT;
		return -1;
	}
	*pfn = entry & PAGEMAP_PFN_MASK;
	/* Linux masks PFNs without CAP_SYS_ADMIN. Do not pretend that zero is
	 * an observed physical layout. A genuine physical page zero is also
	 * conservatively rejected; normal allocatable Guest pages are nonzero. */
	if (*pfn == 0) {
		errno = EACCES;
		return -1;
	}
	return 0;
}

static int descending_pfn(const void *left, const void *right)
{
	const struct page_slot *a = left, *b = right;
	return a->pfn < b->pfn ? 1 : a->pfn > b->pfn ? -1 : 0;
}

static void *controlled_mmap(void *addr, size_t length, int prot, int flags, int original_fd, off_t off)
{
	void *source = MAP_FAILED, *dest = MAP_FAILED;
	struct page_slot *slots = NULL;
	const char *reason = "invalid request";
	int backing = -1, pagemap = -1, saved_errno;
	size_t rounded = 0, pages = 0, forward = 0;
	uint64_t hash = UINT64_C(1469598103934665603), previous = 0;

	if (!target_fd(original_fd))
		return raw_mmap(addr, length, prot, flags, original_fd, off);
	if (sysconf(_SC_PAGESIZE) != GEOMETRY_PAGE || addr != NULL || off != 0 ||
	    length == 0 || length > GEOMETRY_MAX_BYTES ||
	    (flags & (MAP_ANONYMOUS | MAP_FIXED)) != 0 ||
	    (flags & (MAP_PRIVATE | MAP_SHARED)) == 0 ||
	    prot != (PROT_READ | PROT_WRITE)) {
		errno = EINVAL;
		goto fail;
	}
	rounded = (length + GEOMETRY_PAGE - 1) & ~(size_t)(GEOMETRY_PAGE - 1);
	pages = rounded / GEOMETRY_PAGE;
	if (pages < 2) {
		errno = EINVAL;
		reason = "at least two pages required";
		goto fail;
	}

	reason = "memfd_create";
	backing = (int)syscall(SYS_memfd_create, "fio-geometry-pages", MFD_CLOEXEC);
	if (backing < 0)
		goto fail;
	reason = "ftruncate internal memfd";
	if (ftruncate(backing, (off_t)rounded) != 0)
		goto fail;
	reason = "map source pages";
	source = raw_mmap(NULL, rounded, PROT_READ | PROT_WRITE, MAP_SHARED, backing, 0);
	if (source == MAP_FAILED)
		goto fail;
	/* No global THP/sysctl changes. EINVAL permits kernels without THP;
	 * the page-by-page destination is still checked below. */
	if (madvise(source, rounded, MADV_NOHUGEPAGE) != 0 && errno != EINVAL) {
		reason = "disable source hugepage promotion";
		goto fail;
	}
	for (size_t i = 0; i < pages; ++i)
		((volatile unsigned char *)source)[i * GEOMETRY_PAGE] = 0x5a;
	reason = "allocate page index";
	slots = calloc(pages, sizeof(*slots));
	if (slots == NULL)
		goto fail;
	reason = "open pagemap (CAP_SYS_ADMIN required for PFNs)";
	pagemap = open("/proc/self/pagemap", O_RDONLY | O_CLOEXEC);
	if (pagemap < 0)
		goto fail;
	for (size_t i = 0; i < pages; ++i) {
		reason = "read visible present source PFN (zero/masked PFNs rejected)";
		if (read_pfn(pagemap, (char *)source + i * GEOMETRY_PAGE, &slots[i].pfn) != 0)
			goto fail;
		slots[i].file_offset = (off_t)(i * GEOMETRY_PAGE);
	}
	qsort(slots, pages, sizeof(*slots), descending_pfn);
	for (size_t i = 1; i < pages; ++i) {
		if (slots[i - 1].pfn <= slots[i].pfn) {
			errno = EINVAL;
			reason = "nonunique or unsortable backing PFNs";
			goto fail;
		}
	}
	reason = "reserve private destination address range";
	dest = raw_mmap(NULL, rounded, PROT_NONE, MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
	if (dest == MAP_FAILED)
		goto fail;
	for (size_t i = 0; i < pages; ++i) {
		void *wanted = (char *)dest + i * GEOMETRY_PAGE;
		reason = "map one memfd page inside reserved range";
		if (raw_mmap(wanted, GEOMETRY_PAGE, prot, MAP_SHARED | MAP_FIXED,
		             backing, slots[i].file_offset) != wanted)
			goto fail;
		/* Populate a writable PTE without COW. MAP_PRIVATE would invalidate
		 * the controlled layout when fio or Direct READ writes the buffer. */
		((volatile unsigned char *)wanted)[0] = 0x5a;
	}
	if (madvise(dest, rounded, MADV_NOHUGEPAGE) != 0 && errno != EINVAL) {
		reason = "disable destination hugepage promotion";
		goto fail;
	}
	for (size_t i = 0; i < pages; ++i) {
		uint64_t pfn;
		reason = "verify final visible destination PFN";
		if (read_pfn(pagemap, (char *)dest + i * GEOMETRY_PAGE, &pfn) != 0)
			goto fail;
		if (pfn != slots[i].pfn || (i > 0 && previous <= pfn)) {
			errno = ESTALE;
			reason = "PFN migrated or final descending layout invalid";
			goto fail;
		}
		if (i > 0 && pfn == previous + 1)
			++forward;
		previous = pfn;
		/* Hash the offset permutation, without printing physical addresses. */
		hash ^= (uint64_t)slots[i].file_offset / GEOMETRY_PAGE;
		hash *= UINT64_C(1099511628211);
	}
	if (forward != 0) {
		errno = EINVAL;
		reason = "forward contiguous physical pages remain";
		goto fail;
	}
	(void)munmap(source, rounded);
	source = MAP_FAILED;
	close(pagemap);
	close(backing);
	free(slots);
	fprintf(stderr, "FIO_GEOMETRY_VERIFIED requested_bytes=%zu mapped_bytes=%zu page_size=%u pages=%zu adjacent_forward_pairs=%zu descending_pairs=%zu permutation_hash=%016" PRIx64 " storage=memfd_shared\n",
	        length, rounded, GEOMETRY_PAGE, pages, forward, pages - 1, hash);
	return dest;

fail:
	saved_errno = errno ? errno : EIO;
	if (dest != MAP_FAILED)
		(void)munmap(dest, rounded);
	if (source != MAP_FAILED)
		(void)munmap(source, rounded);
	if (pagemap >= 0)
		close(pagemap);
	if (backing >= 0)
		close(backing);
	free(slots);
	fprintf(stderr, "FIO_GEOMETRY_FAILED requested_bytes=%zu reason=%s errno=%d\n", length, reason, saved_errno);
	errno = saved_errno;
	return MAP_FAILED;
}

void *mmap(void *addr, size_t length, int prot, int flags, int fd, off_t off)
{
	return controlled_mmap(addr, length, prot, flags, fd, off);
}

void *mmap64(void *addr, size_t length, int prot, int flags, int fd, off64_t off)
{
	/* Only 64-bit Linux ABIs are supported by this diagnostic library. */
	_Static_assert(sizeof(off_t) == sizeof(off64_t), "64-bit off_t required");
	return controlled_mmap(addr, length, prot, flags, fd, (off_t)off);
}
