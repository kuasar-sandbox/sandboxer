/* Guest-only diagnostic. Read a 1280 KiB span of the disposable data disk,
 * require one block request, and verify each page against a separate pread.
 * Build a static binary for the Guest ISA and run without other disk users.
 */
#define _GNU_SOURCE
#include <fcntl.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <sys/uio.h>
#include <unistd.h>

enum { block = 4096, pages = 320, pool_pages = 8192 };
struct page { uint64_t pfn; char *addr; };

static int order(const void *a, const void *b)
{
	uint64_t x = ((const struct page *)a)->pfn, y = ((const struct page *)b)->pfn;
	return (x > y) - (x < y);
}

static int counters(unsigned long long *reads, unsigned long long *sectors)
{
	FILE *f = fopen("/sys/block/vdc/stat", "r");
	if (!f)
		return -1;
	int n = fscanf(f, "%llu %*u %llu", reads, sectors);
	fclose(f);
	return n == 2 ? 0 : -1;
}

/* Pick the most contiguous 320-page interval, then put its last page first.
 * A fully contiguous selection now starts with a 4 KiB SG and can end
 * the first 1 MiB window partway through the next SG. The final disk
 * counters decide whether the kernel actually submitted a single request. */
static int vectors(struct iovec *vec)
{
	static struct page slots[pool_pages];
	static uint64_t entries[pool_pages];
	char *pool = mmap(NULL, pool_pages * block, PROT_READ | PROT_WRITE,
	                  MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
	if (pool == MAP_FAILED || sysconf(_SC_PAGESIZE) != block)
		return -1;
	memset(pool, 0x5a, pool_pages * block);
	int fd = open("/proc/self/pagemap", O_RDONLY);
	if (fd < 0 || pread(fd, entries, sizeof(entries),
	                    (off_t)((uintptr_t)pool / block * 8)) != sizeof(entries))
		return -1;
	close(fd);
	for (int i = 0; i < pool_pages; i++) {
		slots[i].pfn = entries[i] & ((1ULL << 55) - 1);
		if (!(entries[i] >> 63) || !slots[i].pfn)
			return -1;
		slots[i].addr = pool + i * block;
	}
	qsort(slots, pool_pages, sizeof(slots[0]), order);
	int runs = 1, best = pages + 1, start = 0;
	for (int i = 1; i < pool_pages; i++) {
		runs += slots[i].pfn != slots[i-1].pfn + 1;
		if (i >= pages)
			runs -= slots[i-pages+1].pfn != slots[i-pages].pfn + 1;
		if (i >= pages-1 && runs < best) {
			best = runs;
			start = i-pages+1;
		}
	}
	printf("selected_pages=%d sorted_physical_runs=%d\n", pages, best);
	for (int i = 0; i < pages; i++)
		vec[i] = (struct iovec){slots[start+(i+pages-1)%pages].addr, block};
	return best + 1 <= 64 ? 0 : -1;
}

int main(void)
{
	struct iovec vec[pages];
	if (vectors(vec)) {
		fprintf(stderr, "cannot establish physical segment layout\n");
		return 77;
	}
	int fd = open("/dev/vdc", O_DIRECT | O_RDONLY);
	const off_t offset = 256LL << 20;
	unsigned long long before, after, sectors0, sectors1;
	if (fd < 0 || counters(&before, &sectors0) ||
	    preadv(fd, vec, pages, offset) != pages * block ||
	    counters(&after, &sectors1)) {
		perror("large read");
		return 1;
	}
	char reference[block] __attribute__((aligned(block)));
	int distinct = 0;
	for (int i = 0; i < pages; i++) {
		if (pread(fd, reference, block, offset + i * block) != block ||
		    memcmp(reference, vec[i].iov_base, block)) {
			fprintf(stderr, "large read mismatch at page %d\n", i);
			return 1;
		}
		distinct |= memcmp(reference, vec[0].iov_base, block) != 0;
	}
	if (!distinct) {
		fprintf(stderr, "probe needs distinct data in the tested disk span\n");
		return 1;
	}
	printf("bytes=%d reads=%llu sectors=%llu content=verified\n",
	       pages * block, after-before, sectors1-sectors0);
	return after-before == 1 && sectors1-sectors0 == pages * block/512 ? 0 : 77;
}
