#include <linux/types.h>
#include <linux/bpf.h>
#include <bpf/bpf_helpers.h>

#define LINE_MAX 200

struct line_t { char data[LINE_MAX]; };

struct {
  __uint(type, BPF_MAP_TYPE_HASH);
  __uint(max_entries, 2048);
  __type(key, __u32);
  __type(value, __u8);
} agent_pids SEC(".maps");

struct {
  __uint(type, BPF_MAP_TYPE_HASH);
  __uint(max_entries, 2048);
  __type(key, __u32);
  __type(value, __u64);
} last_write SEC(".maps");

struct {
  __uint(type, BPF_MAP_TYPE_HASH);
  __uint(max_entries, 2048);
  __type(key, __u32);
  __type(value, struct line_t);
} last_line SEC(".maps");

/* tracepoint syscalls/sys_enter_write 实际布局 (来自 tracefs format):
   common_type/flags/preempt_count/pid = 8B (@0)
   __syscall_nr = 4B (@8) + 4B padding (@12)
   fd = 8B (@16), buf = 8B (@24), count = 8B (@32) */
struct syscalls__sys_enter_write {
	__u64 pad0;       // common_* (8B)
	__u32 nr;         // __syscall_nr (4B)
	__u32 pad1;       // padding (4B)
	__u64 fd;         // @16
	__u64 buf;        // @24
	__u64 count;      // @32
};

SEC("tracepoint/syscalls/sys_enter_write")
int on_write(struct syscalls__sys_enter_write *ctx) {
  __u32 pid = bpf_get_current_pid_tgid() >> 32;
  __u8 *ok = bpf_map_lookup_elem(&agent_pids, &pid);
  if (!ok) return 0;
  __u64 ts = bpf_ktime_get_ns();
  bpf_map_update_elem(&last_write, &pid, &ts, BPF_ANY);
  struct line_t line = {};
  /* sys_enter_write 时 buf 仍在 user 空间; 只读本次 write 实际长度(count), 避免 200B 缓冲残留 */
  __u64 len = ctx->count;
  if (len > LINE_MAX) {
    len = LINE_MAX;
  }
  bpf_probe_read_user(&line, len, (void *)(unsigned long)ctx->buf);
  bpf_map_update_elem(&last_line, &pid, &line, BPF_ANY);
  return 0;
}

char LICENSE[] SEC("license") = "GPL";
