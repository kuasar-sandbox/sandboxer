/*
 * Fork immediately at ELF entry, before libc or a language runtime can run.
 * Each child records its cgroup view and, in shared-PID cases, verifies its
 * real membership through sandbox-init's guest-global cgroupfs mount. The
 * parent then execs argv[1]. This test fixture is built during the source
 * build/prepare stage and is never compiled by product E2E execution.
 *
 * It is intentionally x86_64 and libc-free: sandboxer KVM product E2E is an
 * x86_64 lane today, and keeping the injected launch binary tiny preserves the
 * immediate-fork placement race exercised by the legacy test.
 */

typedef unsigned long usize;

enum {
	SYS_read = 0,
	SYS_write = 1,
	SYS_close = 3,
	SYS_getpid = 39,
	SYS_fork = 57,
	SYS_execve = 59,
	SYS_exit = 60,
	SYS_wait4 = 61,
	SYS_openat = 257,
	AT_FDCWD = -100,
	O_WRONLY = 1,
	O_CREAT = 0100,
	O_APPEND = 02000,
};

static long syscall0(long number)
{
	long result;
	__asm__ volatile("syscall"
		: "=a"(result)
		: "a"(number)
		: "rcx", "r11", "memory");
	return result;
}

static long syscall1(long number, long a1)
{
	long result;
	__asm__ volatile("syscall"
		: "=a"(result)
		: "a"(number), "D"(a1)
		: "rcx", "r11", "memory");
	return result;
}

static long syscall3(long number, long a1, long a2, long a3)
{
	long result;
	__asm__ volatile("syscall"
		: "=a"(result)
		: "a"(number), "D"(a1), "S"(a2), "d"(a3)
		: "rcx", "r11", "memory");
	return result;
}

static long syscall4(long number, long a1, long a2, long a3, long a4)
{
	register long r10 __asm__("r10") = a4;
	long result;
	__asm__ volatile("syscall"
		: "=a"(result)
		: "a"(number), "D"(a1), "S"(a2), "d"(a3), "r"(r10)
		: "rcx", "r11", "memory");
	return result;
}

static usize append_string(char *out, usize offset, const char *value)
{
	usize i;
	for (i = 0; value[i] != '\0'; i++)
		out[offset + i] = value[i];
	return offset + i;
}

static long open_read(const char *path)
{
	return syscall4(SYS_openat, AT_FDCWD, (long)path, 0, 0);
}

static long read_file(const char *path, char *body, usize capacity)
{
	long fd = open_read(path);
	long count;
	if (fd < 0)
		return fd;
	count = syscall3(SYS_read, fd, (long)body, capacity);
	syscall1(SYS_close, fd);
	return count;
}

static int pid_is_listed(const char *path, long pid)
{
	char body[16384];
	char digits[24];
	usize digit_count = 0;
	long count = read_file(path, body, sizeof(body));
	long value = pid;
	usize i;

	if (count <= 0)
		return 0;
	do {
		digits[digit_count++] = (char)('0' + value % 10);
		value /= 10;
	} while (value != 0);

	for (i = 0; i < (usize)count; i++) {
		usize j;
		if (i != 0 && body[i - 1] != '\n')
			continue;
		for (j = 0; j < digit_count; j++) {
			if (i + j >= (usize)count || body[i + j] != digits[digit_count - j - 1])
				break;
		}
		if (j == digit_count && (i + j == (usize)count || body[i + j] == '\n'))
			return 1;
	}
	return 0;
}

static void append_result(const char *role, const char *body, usize length)
{
	char path[96];
	usize offset = 0;
	long fd;

	offset = append_string(path, offset, "/tmp/cg-");
	offset = append_string(path, offset, role);
	offset = append_string(path, offset, ".fast-paths");
	path[offset] = '\0';
	fd = syscall4(SYS_openat, AT_FDCWD, (long)path,
		O_WRONLY | O_CREAT | O_APPEND, 0644);
	if (fd < 0)
		return;
	syscall3(SYS_write, fd, (long)body, length);
	syscall1(SYS_close, fd);
}

static void record_child(const char *role, int control, int real_check)
{
	static const char cgroup_path[] = "/proc/self/cgroup";
	static const char real_root[] = "/proc/1/root/sys/fs/cgroup/app/cgroup.procs";
	static const char real_init[] = "/proc/1/root/sys/fs/cgroup/app/init/cgroup.procs";
	static const char read_failure[] = "READ-FAIL\n";
	static const char membership_failure[] = "REAL-MISS\n";
	char body[256];
	long count;
	usize start;
	usize end;

	count = read_file(cgroup_path, body, sizeof(body));
	if (count <= 0) {
		append_result(role, read_failure, sizeof(read_failure) - 1);
		return;
	}
	for (start = 0; start + 2 < (usize)count; start++) {
		if (body[start] == '0' && body[start + 1] == ':' && body[start + 2] == ':')
			break;
	}
	if (start + 2 >= (usize)count) {
		append_result(role, read_failure, sizeof(read_failure) - 1);
		return;
	}
	for (end = start; end < (usize)count && body[end] != '\n'; end++)
		;
	if (end < sizeof(body))
		body[end++] = '\n';
	if (real_check && !pid_is_listed(control ? real_init : real_root,
			syscall0(SYS_getpid))) {
		append_result(role, membership_failure, sizeof(membership_failure) - 1);
		return;
	}
	append_result(role, body + start, end - start);
}

__attribute__((used)) static long probe_main(long *initial_stack)
{
	long argc = initial_stack[0];
	char **argv = (char **)&initial_stack[1];
	char **envp;
	long spawned = 0;
	long reaped = 0;
	long i;

	if (argc < 7)
		return 125;
	envp = &argv[argc + 1];
	for (i = 0; i < 128; i++) {
		long pid = syscall0(SYS_fork);
		if (pid == 0) {
			record_child(argv[2], argv[5][0] == 't', argv[6][0] == '1');
			syscall1(SYS_exit, 0);
		}
		if (pid > 0)
			spawned++;
		else {
			static const char fork_failure[] = "FORK-FAIL\n";
			append_result(argv[2], fork_failure, sizeof(fork_failure) - 1);
		}
	}
	while (reaped < spawned) {
		if (syscall4(SYS_wait4, -1, 0, 0, 0) > 0)
			reaped++;
	}
	syscall3(SYS_execve, (long)argv[1], (long)&argv[1], (long)envp);
	return 127;
}

__asm__(
	".global _start\n"
	"_start:\n"
	"mov %rsp, %rdi\n"
	"and $-16, %rsp\n"
	"call probe_main\n"
	"mov %rax, %rdi\n"
	"mov $60, %rax\n"
	"syscall\n"
	"hlt\n");
