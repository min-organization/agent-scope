// agent_mon.bpf.c — eBPF 全行为采集
// 挂载多个 syscall tracepoint, 按 agent 进程树 pid 集合过滤,
// 将最近一次行为事件写入按 pid 索引的 hash map(替代 ringbuf, 规避部分内核 ringbuf 创建限制)。
// 仅元数据: 命令/文件名 basename, 域名:端口。绝不采集文件内容或 pty 原始字节。
//
// 编译: clang -O2 -g -target bpf -c agent_mon.bpf.c -o agent_mon.bpf

#include "bpf_types.h"

#define TASK_COMM_LEN 16
#define ARG_LEN 64

struct beh {
    __u8  type;     // 0=write 1=execve 2=openat 3=connect
    __u8  wr_only;
    __u16 port;     // 网络字节序
    __u32 daddr;    // 网络字节序 IPv4
    __u64 ts;       // 事件时间戳(纳秒)
    char  comm[TASK_COMM_LEN];
    char  arg[ARG_LEN];
};

struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 4096);
    __uint(key_size, 4);
    __uint(value_size, 1);
} agent_pids SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 4096);
    __uint(key_size, 4);
    __type(value, struct beh);
} beh_map SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 4096);
    __uint(key_size, 4);
    __uint(value_size, 8);
} last_active SEC(".maps");

struct sys_enter_ctx {
    unsigned long long unused;
    unsigned long long id;
    unsigned long long args[6];
};

static __always_inline int is_agent(__u32 pid)
{
	__u8 *v = bpf_map_lookup_elem(&agent_pids, &pid, 0);
	return v != NULL;
}

static __always_inline void mark_active(__u32 pid)
{
    __u64 now = bpf_ktime_get_ns();
    bpf_map_update_elem(&last_active, &pid, &now, BPF_ANY);
}

static __always_inline void emit(__u32 pid, __u8 type, __u8 wr, __u16 port, __u32 daddr, const char *arg)
{
    struct beh b = {};
    b.type = type;
    b.wr_only = wr;
    b.port = port;
    b.daddr = daddr;
    b.ts = bpf_ktime_get_ns();
    bpf_get_current_comm(&b.comm, sizeof(b.comm));
    if (arg) {
        // 用 probe_read_user(逐字节) 而非 probe_read_user_str, 兼容性更好
        bpf_probe_read_user(&b.arg, sizeof(b.arg), arg);
        b.arg[sizeof(b.arg) - 1] = 0;
    }
    bpf_map_update_elem(&beh_map, &pid, &b, BPF_ANY);
}

static __always_inline void basename_of(const char *path, char *dst, __u32 len)
{
    if (bpf_probe_read_user_str(dst, len, path) < 0)
        return;
    int last = -1;
    #pragma unroll
    for (int i = 0; i < ARG_LEN; i++) {
        if (dst[i] == 0) break;
        if (dst[i] == '/') last = i;
    }
    if (last >= 0) {
        int j = 0;
        for (int i = last + 1; i < ARG_LEN; i++) {
            dst[j++] = dst[i];
            if (dst[i] == 0) break;
        }
    }
}

SEC("tracepoint/syscalls/sys_enter_write")
int on_write(struct sys_enter_ctx *ctx)
{
    __u32 pid = bpf_get_current_pid_tgid() >> 32;
    if (!is_agent(pid)) return 0;
    mark_active(pid);
    return 0;
}

// 行为事件(execve/openat/connect)系统级采集, 不做 pid 过滤:
// 子进程可能在被加入 agent_pids 前就已生灭(timing gap), 故在 Go 侧按进程树过滤。
SEC("tracepoint/syscalls/sys_enter_execve")
int on_execve(struct sys_enter_ctx *ctx)
{
    __u32 pid = bpf_get_current_pid_tgid() >> 32;
    mark_active(pid);
    emit(pid, 1, 0, 0, 0, (const char *)ctx->args[0]);
    return 0;
}

SEC("tracepoint/syscalls/sys_enter_openat")
int on_openat(struct sys_enter_ctx *ctx)
{
    __u32 pid = bpf_get_current_pid_tgid() >> 32;
    mark_active(pid);
    long flags = (long)ctx->args[2];
    __u8 wr = 0;
    if ((flags & 0x1) || (flags & 0x2)) wr = 1;
    emit(pid, 2, wr, 0, 0, (const char *)ctx->args[1]);
    return 0;
}

// renameat/renameat2(olddirfd, oldpath, newdirfd, newpath):
// 现代内核无 sys_enter_rename, rename 经 renameat/renameat2 实现。
// 仅关心 newpath(args[3], 最终真实文件名), 用于捕获 copilot 等代理
// "写临时文件 -> rename 成真实文件" 的最终落盘名。
// 两者并存: 不同内核版本可能只提供其一, 由 Go 侧挂载时按可用性单事件兼容降级。
SEC("tracepoint/syscalls/sys_enter_renameat")
int on_renameat(struct sys_enter_ctx *ctx)
{
    __u32 pid = bpf_get_current_pid_tgid() >> 32;
    mark_active(pid);
    emit(pid, 4, 1, 0, 0, (const char *)ctx->args[3]);
    return 0;
}

SEC("tracepoint/syscalls/sys_enter_renameat2")
int on_renameat2(struct sys_enter_ctx *ctx)
{
    __u32 pid = bpf_get_current_pid_tgid() >> 32;
    mark_active(pid);
    emit(pid, 4, 1, 0, 0, (const char *)ctx->args[3]);
    return 0;
}

SEC("tracepoint/syscalls/sys_enter_connect")
int on_connect(struct sys_enter_ctx *ctx)
{
    __u32 pid = bpf_get_current_pid_tgid() >> 32;
    mark_active(pid);

    struct sockaddr_in {
        __u16 sa_family;
        __u16 sin_port;
        __u32 sin_addr;
    } __attribute__((packed)) *sa;
    sa = (void *)ctx->args[1];

    __u16 fam;
    if (bpf_probe_read_user(&fam, sizeof(fam), &sa->sa_family) != 0)
        return 0;
    if (fam != 2) return 0;

    __u16 port = 0;
    __u32 daddr = 0;
    bpf_probe_read_user(&port, sizeof(port), &sa->sin_port);
    bpf_probe_read_user(&daddr, sizeof(daddr), &sa->sin_addr);
    emit(pid, 3, 0, port, daddr, NULL);
    return 0;
}

char _license[] SEC("license") = "GPL";
