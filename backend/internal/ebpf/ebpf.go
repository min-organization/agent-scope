package ebpf

import (
	"bytes"
	"embed"
	"encoding/binary"
	"fmt"
	"log"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"golang.org/x/sys/unix"
)

//go:embed bpf/agent_mon.bpf
var ebpfObj []byte

// 强制 embed 包被使用
var _ = embed.FS{}

const (
	EvWrite   = 0
	EvExecve  = 1
	EvOpenat  = 2
	EvConnect = 3
	EvRename  = 4 // newpath(最终真实文件名), 捕获 临时文件->rename 落盘
)

// Event 是 eBPF 上报的行为事件(仅元数据, 不含文件内容/pty 字节)
// 注意: 字段顺序/大小必须与 BPF struct beh 完全一致(96 字节), 否则 iter.Next 解析错位。
// BPF 布局: type(u8) wr_only(u8) port(u16) daddr(u32) ts(u64) comm[16] arg[64]
type Event struct {
	Type   uint8
	WrOnly uint8
	Port   uint16 // 网络字节序
	Daddr  uint32 // 网络字节序 IPv4
	Ts     uint64
	Comm   [16]byte
	Arg    [64]byte
}

// Monitor 封装 eBPF 全行为采集: 挂载 write/execve/openat/connect 四个 tracepoint,
// 行为事件写入按 pid 索引的 beh_map, Go 侧每次 scan 轮询。
type Monitor struct {
	coll       *ebpf.Collection
	links      []link.Link
	agentPids  *ebpf.Map
	behMap     *ebpf.Map
	lastActive *ebpf.Map
}

// New 加载 eBPF 程序并挂载全部 tracepoint。失败返回 error(调用方降级)。
func New() (*Monitor, error) {
	spec, err := ebpf.LoadCollectionSpecFromReader(bytes.NewReader(ebpfObj))
	if err != nil {
		return nil, fmt.Errorf("加载 eBPF spec: %w", err)
	}
	coll, err := ebpf.NewCollection(spec)
	if err != nil {
		return nil, fmt.Errorf("创建 eBPF collection: %w", err)
	}

	// 验证必需 map 存在
	for _, name := range []string{"agent_pids", "beh_map", "last_active"} {
		if coll.Maps[name] == nil {
			coll.Close()
			return nil, fmt.Errorf("eBPF map %q 缺失", name)
		}
	}

	var links []link.Link
	tps := []struct {
		cat, name, prog string
		required        bool // 挂载失败是否致命(可选 tracepoint 缺失则降级跳过)
	}{
		{"syscalls", "sys_enter_write", "on_write", true},
		{"syscalls", "sys_enter_execve", "on_execve", true},
		{"syscalls", "sys_enter_openat", "on_openat", true},
		{"syscalls", "sys_enter_renameat", "on_renameat", false},
		{"syscalls", "sys_enter_renameat2", "on_renameat2", false},
		{"syscalls", "sys_enter_connect", "on_connect", true},
	}
	for _, t := range tps {
		prog, ok := coll.Programs[t.prog]
		if !ok {
			if t.required {
				coll.Close()
				return nil, fmt.Errorf("eBPF 程序 %s 缺失(必需)", t.prog)
			}
			log.Printf("eBPF 程序 %s 未在对象中, 降级跳过", t.prog)
			continue
		}
		lnk, err := link.Tracepoint(t.cat, t.name, prog, nil)
		if err != nil {
			if t.required {
				for _, l := range links {
					if cerr := l.Close(); cerr != nil {
						log.Printf("eBPF 关闭 link 错误: %v", cerr)
					}
				}
				coll.Close()
				return nil, fmt.Errorf("挂载必需 tracepoint %s/%s: %w", t.cat, t.name, err)
			}
			// 可选 tracepoint(如 renameat/renameat2 在部分内核不存在): 单事件兼容降级,
			// 跳过该事件采集但不影响其他 tracepoint, 保障 eBPF 监控整体可用。
			log.Printf("eBPF 可选 tracepoint %s/%s 不可用, 降级跳过: %v", t.cat, t.name, err)
			continue
		}
		links = append(links, lnk)
	}

	m := &Monitor{
		coll:       coll,
		links:      links,
		agentPids:  coll.Maps["agent_pids"],
		behMap:     coll.Maps["beh_map"],
		lastActive: coll.Maps["last_active"],
	}
	return m, nil
}

// AddPID 将 pid 加入监控集合(Go 侧通常填入整棵 agent 进程树)
func (m *Monitor) AddPID(pid int) {
	if m.agentPids == nil {
		return
	}
	if err := m.agentPids.Put(uint32(pid), uint8(1)); err != nil {
		log.Printf("eBPF AddPID %d: %v", pid, err)
	}
}

// DelPID 移除监控
func (m *Monitor) DelPID(pid int) {
	if m.agentPids == nil {
		return
	}
	if err := m.agentPids.Delete(uint32(pid)); err != nil {
		log.Printf("eBPF DelPID %d: %v", pid, err)
	}
}

// LastActive 返回该 pid 最近一次活动(任一 syscall)的纳秒时间戳(壁钟, 与 Go time.Now() 可比); ok=false 表示无记录
func (m *Monitor) LastActive(pid int) (int64, bool) {
	var t uint64
	if err := m.lastActive.Lookup(uint32(pid), &t); err != nil {
		return 0, false
	}
	// bpf_ktime_get_ns() 返回 CLOCK_BOOTTIME(单调时钟, 自启动以来纳秒)。
	// 转换为壁钟(wall clock)以与 Go time.Now().UnixNano() 保持一致,
	// 避免 updateState 中 lastOut 与 now 混用两种时钟源导致时间差异常。
	var boot unix.Timespec
	unix.ClockGettime(unix.CLOCK_BOOTTIME, &boot)
	return int64(t) + (time.Now().UnixNano() - boot.Nano()), true
}

// PollEvents 返回 beh_map 中全部行为事件(系统级采集, 由调用方按 agent 进程树过滤)。
// 重要: 提取后删除 map 中的条目, 避免 4096 上限被已退出进程的残留事件填满导致新事件静默丢失。
func (m *Monitor) PollEvents() map[uint32]Event {
	out := make(map[uint32]Event)
	var pid uint32
	var ev Event
	iter := m.behMap.Iterate()
	for iter.Next(&pid, &ev) {
		out[pid] = ev
		if err := m.behMap.Delete(pid); err != nil {
			log.Printf("eBPF PollEvents Delete %d: %v", pid, err)
		}
	}
	return out
}

func keysOf(m map[uint32]Event) []uint32 {
	ks := make([]uint32, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

func (m *Monitor) Close() {
	for _, l := range m.links {
		if err := l.Close(); err != nil {
			log.Printf("eBPF 关闭 link: %v", err)
		}
	}
	m.coll.Close()
}

// 供 binary.Read 兼容: Event 已是小端布局, 这里仅做占位(实际由 BPF 直接写入同构 struct)
var _ = binary.LittleEndian
