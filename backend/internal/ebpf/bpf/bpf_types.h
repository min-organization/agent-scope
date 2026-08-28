// 最小 BPF 类型与 helper 声明(不依赖系统内核头, 纯自包含)
// 仅声明 agent_mon.bpf.c 用到的符号, 让 clang -target bpf 能独立编译。
#ifndef __AGENT_BPF_TYPES_H
#define __AGENT_BPF_TYPES_H

typedef unsigned char  __u8;
typedef unsigned short __u16;
typedef unsigned int   __u32;
typedef unsigned long long __u64;
typedef signed char    __s8;
typedef signed short   __s16;
typedef signed int     __s32;
typedef signed long long __s64;

#define __always_inline __attribute__((always_inline))
#define NULL ((void *)0)

// map 类型常量
#define BPF_MAP_TYPE_HASH     1
#define BPF_MAP_TYPE_RINGBUF  9

#define BPF_ANY    0
#define BPF_NOEXIST 1
#define BPF_EXIST  2

#ifndef __section
#define __section(NAME) __attribute__((section(NAME), used))
#endif
#define SEC(NAME) __section(NAME)

// libbpf 风格 map 声明宏(字段名须为 type/key/value/max_entries, 供 cilium/ebpf BTF 解析)
#define __uint(name, val) int (*name)[val]
#define __type(name, val) typeof(val) *name
#define __array(name, val)

// helper 函数指针(编号参考 kernel bpf.h)
// 注意: bpf_map_lookup_elem 真实签名为 (map, key, flags) 三参数, flags 传 0
static void *(*bpf_map_lookup_elem)(void *map, const void *key, __u64 flags) = (void *)1;
static long (*bpf_map_update_elem)(void *map, const void *key, const void *value, __u64 flags) = (void *)2;
static long (*bpf_map_delete_elem)(void *map, const void *key) = (void *)3;
static void *(*bpf_ringbuf_reserve)(void *ringbuf, __u64 size, __u64 flags) = (void *)135;
static void (*bpf_ringbuf_submit)(void *data, __u64 flags) = (void *)136;
static __u64 (*bpf_ktime_get_ns)(void) = (void *)5;
static __u64 (*bpf_get_current_pid_tgid)(void) = (void *)14;
static long (*bpf_get_current_comm)(void *buf, __u32 size) = (void *)16;
static long (*bpf_probe_read_user)(void *dst, __u32 size, const void *unsafe_ptr) = (void *)112;
static long (*bpf_probe_read_user_str)(void *dst, __u32 size, const void *unsafe_ptr) = (void *)113;

#endif
