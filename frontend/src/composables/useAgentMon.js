import { ref, reactive, computed, onMounted, onUnmounted } from 'vue'
import { t, locale, initLang } from '../i18n.js'

// ---- 全局单例共享状态(模块级, 所有组件共享) ----
const roots = ref([])
const q = ref('')
const filterState = ref('')
const open = ref(0)
const tab = ref('info')
const onlyUser = ref(true)
const events = ref([])
const eventsLoading = ref(false)
const clock = ref('')
const loading = ref(true)
const loadError = ref('')
const wsConnected = ref(false)
const flashSet = reactive(new Set())
const prevStates = {}
const alerts = ref([])
const focusMode = ref(false)  // 默认关闭: 打开页面显示全部 agent, 避免空闲 agent 被隐藏造成"agent 消失"的困惑
const expanded = ref(new Set())
const viewMode = ref('tree')  // 视图模式: 'tree'(进程树) / 'board'(Kanban 状态看板)

const states = ['running', 'thinking', 'editing', 'waiting', 'idle', 'error']

// 状态/类型/告警标签: 文案统一走 i18n(t()), 随语言切换自动更新。
const labelOf = (s) => t('state.' + s) || s
const kindLabel = (k) => t('kind.' + k) || k
const alertLabel = (k) => t('alert.' + k) || k

const ATTENTION = new Set(['thinking', 'editing', 'waiting', 'error'])

// ---- 派生状态 ----
const criticalAlerts = computed(() => alerts.value.filter((a) => a.level === 'critical'))
const anyAlert = computed(() => alerts.value.length > 0)
const alertCount = computed(() => alerts.value.length)

const alertsByPid = computed(() => {
  const m = {}
  for (const a of alerts.value) m[a.pid] = a
  return m
})

const byPid = computed(() => {
  const m = {}
  for (const n of roots.value) if (n) m[n.pid] = n
  return m
})
const allNodes = computed(() => roots.value.filter(Boolean))
// 主 agent 维度(只数 root 节点): 顶部筛选组件的所有数字按"主 agent"统计,
// 子进程(派生的 bash/node 等工具执行体)只在树里展开可见, 不计入顶部计数。
// 这符合用户心智——用户关心的是自己启动的几个主 agent 的状态, 而非其内部子进程。
const rootNodes = computed(() => allNodes.value.filter((n) => n.parent_pid === 0))

const countByState = computed(() => {
  const m = {}
  for (const a of rootNodes.value) m[a.state] = (m[a.state] || 0) + 1
  return m
})
// Kanban 看板列(方案 A1: 细分状态列 + 异常列)。
// 异常列优先: 凡有 active 告警(任意 level)的 root 归异常列, 突出需关注; 其余按状态分列。
// 看板忽略 filterState(状态筛选 chip 已由分列表达), 但尊重搜索词 q 与聚焦模式 focusMode(与树形一致)。
const boardDef = [
  { key: 'running', labelKey: 'board.running' },
  { key: 'thinking', labelKey: 'board.thinking' },
  { key: 'editing', labelKey: 'board.editing' },
  { key: 'waiting', labelKey: 'board.waiting' },
  { key: 'idle', labelKey: 'board.idle' },
  { key: 'error', labelKey: 'board.error' },
  { key: 'anomaly', labelKey: 'board.anomaly' },
]
const boardNodes = computed(() => {
  const kw = q.value.toLowerCase()
  const match = (n) => {
    if (!kw) return true
    return [n.tool, String(n.pid), n.last_file, n.last_conn, n.last_cmd, n.task]
      .some((v) => v && String(v).toLowerCase().includes(kw))
  }
  let rs = rootNodes.value.filter(match)
  if (focusMode.value) rs = rs.filter((n) => hasAttention(n))
  return rs
})
const boardColumns = computed(() => boardDef.map((col) => ({
  ...col,
  label: t(col.labelKey),
  count: boardNodes.value.filter((n) => col.key === 'anomaly'
    ? !!alertsByPid.value[n.pid]
    : !alertsByPid.value[n.pid] && n.state === col.key).length,
  nodes: boardNodes.value.filter((n) => col.key === 'anomaly'
    ? !!alertsByPid.value[n.pid]
    : !alertsByPid.value[n.pid] && n.state === col.key),
})))
const boardNodesTotal = computed(() => boardNodes.value.length)
const anyNeedsInput = computed(() => rootNodes.value.some((a) => a.needs_input))
const needsInputCount = computed(() => rootNodes.value.filter((a) => a.needs_input).length)
const attentionCount = computed(() => rootNodes.value.filter((n) => hasAttention(n)).length)

function hasAttention(n) {
  if (n.needs_input) return true
  if (alertsByPid.value[n.pid]) return true
  return ATTENTION.has(n.state)
}

// 筛选 + 聚焦
const filteredRoots = computed(() => {
  const kw = q.value.toLowerCase()
  const fs = filterState.value
  const match = (n) => {
    if (fs && n.state !== fs) return false
    if (!kw) return true
    return [n.tool, String(n.pid), n.last_file, n.last_conn, n.last_cmd, n.task]
      .some((v) => v && String(v).toLowerCase().includes(kw))
  }
  const keep = (n) => {
    if (match(n)) return true
    return (n.children || []).some((cid) => { const c = byPid.value[cid]; return c && keep(c) })
  }
  let rs = allNodes.value.filter((n) => n.parent_pid === 0 && keep(n))
  if (focusMode.value) {
    const subtreeHasAttention = (n) => {
      if (hasAttention(n)) return true
      return (n.children || []).some((cid) => { const c = byPid.value[cid]; return c && subtreeHasAttention(c) })
    }
    rs = rs.filter((n) => subtreeHasAttention(n))
  }
  // 保持"已展开详情"的节点始终可见: 否则聚焦/状态筛选/搜索会把它从树中移除,
  // 导致该节点的详情面板(渲染在树节点内部)被意外合起。强制把其 root 纳入结果。
  if (open.value) {
    const on = byPid.value[open.value]
    const rootPid = on ? (on.root_pid || on.pid) : open.value
    if (rootPid && !rs.some((r) => r.pid === rootPid)) {
      const root = allNodes.value.find((n) => n.pid === rootPid && n.parent_pid === 0)
      if (root) rs.push(root)
    }
  }
  return rs
})

// ---- 工具函数 ----
// 统一显示值: 后端可能返回字符串 'null' 或空串, 归一为空(配合 || '—' 显示占位)
function dv(v) {
  return (v === null || v === undefined || v === '' || v === 'null') ? '' : v
}

function fmtTime(ts) {
  if (!ts) return '—'
  try { return new Date(ts * 1000).toLocaleTimeString() } catch { return '—' }
}

function applySnapshot(data, ad) {
  if (Array.isArray(data)) {
    for (const a of data) {
      if (prevStates[a.pid] !== undefined && prevStates[a.pid] !== a.state) {
        flashSet.add(a.pid)
        setTimeout(() => flashSet.delete(a.pid), 1200)
      }
      prevStates[a.pid] = a.state
    }
    roots.value = data
  }
  if (Array.isArray(ad)) alerts.value = ad
}

async function load() {
  try {
    const [r, ar] = await Promise.all([fetch('/api/agents'), fetch('/api/alerts?limit=200')])
    if (!r.ok || !ar.ok) throw new Error(`后端返回 ${r.status} / ${ar.status}`)
    applySnapshot(await r.json(), await ar.json())
    loadError.value = ''
  } catch (e) {
    loadError.value = e.message || '无法连接后端'
  } finally {
    loading.value = false
  }
}

let ws = null
let wsRetry = 0
function connectWS() {
  try {
    ws = new WebSocket((location.protocol === 'https:' ? 'wss://' : 'ws://') + location.host + '/ws')
    ws.onopen = () => { wsConnected.value = true; wsRetry = 0 }
    ws.onmessage = (ev) => {
      try {
        const m = JSON.parse(ev.data)
        if (m.type === 'tree' || m.type === 'snapshot') {
          // 后端 wss.Msg 的 agents/alerts 字段是 json.RawMessage, 经 json.Marshal 后会被直接嵌入为
          // JSON 数组(而非字符串)。前端需兼容两种格式: 已是数组则直接用, 否则 JSON.parse。
          const agents = Array.isArray(m.agents) ? m.agents : JSON.parse(m.agents || '[]')
          const alerts = Array.isArray(m.alerts) ? m.alerts : JSON.parse(m.alerts || '[]')
          applySnapshot(agents, alerts)
        }
      } catch (e) { console.warn('解析 WS 消息失败:', e) }
    }
    ws.onclose = () => {
      wsConnected.value = false
      wsRetry++
      setTimeout(connectWS, Math.min(1000 * wsRetry, 5000))
    }
    ws.onerror = () => { wsConnected.value = false; try { ws.close() } catch {} }
  } catch (e) {
    wsConnected.value = false
    setTimeout(connectWS, 2000)
  }
}

async function loadEvents(a, userVal) {
  const reqPid = a.pid
  tab.value = 'timeline'
  eventsLoading.value = true
  const ou = userVal !== undefined ? userVal : onlyUser.value
  try {
    const r = await fetch('/api/events?pid=' + a.pid + '&limit=50&only_user=' + (ou ? '1' : '0'))
    if (!r.ok) throw new Error('HTTP ' + r.status)
    const data = await r.json()
    // pid 守卫: 快速切换节点时, 只接受"当前仍打开节点"的响应, 避免旧响应覆盖新节点的时间线
    if (open.value === reqPid) events.value = data
  } catch (e) {
    console.warn('加载事件失败 (pid ' + reqPid + '):', e)
    if (open.value === reqPid) events.value = []
  }
  if (open.value === reqPid) eventsLoading.value = false
}

function toggle(a) {
  if (open.value === a.pid) { open.value = 0; return }
  open.value = a.pid
  tab.value = 'info'
  events.value = []
  // 强制展开该节点所在的 root: 子树默认可能折叠, 导致子进程节点(及其详情面板)不在 DOM 中。
  // 展开 root 保证打开详情时节点一定可见, 且后续筛选/聚焦不会再把它从树中移除。
  const rootPid = a.root_pid || a.pid
  if (rootPid) expanded.value.add(rootPid)
}

function toggleOnlyUser(node) {
  onlyUser.value = !onlyUser.value
  loadEvents(node, onlyUser.value)
}

function isExpanded(n) {
  if (n.depth === 0) {
    if (expanded.value.has(-n.pid)) return false // 显式折叠优先
    if (expanded.value.has(n.pid)) return true
    return hasAttention(n) // 默认: 有关注的 root 自动展开
  }
  return !expanded.value.has(-n.pid)
}
function toggleExpand(n) {
  if (n.depth === 0) {
    if (isExpanded(n)) { expanded.value.delete(n.pid); expanded.value.add(-n.pid) }
    else { expanded.value.delete(-n.pid); expanded.value.add(n.pid) }
  } else {
    if (expanded.value.has(-n.pid)) expanded.value.delete(-n.pid)
    else expanded.value.add(-n.pid)
  }
}
function expandAll() { expanded.value = new Set(allNodes.value.map((n) => n.pid)) }
function collapseAll() { expanded.value = new Set(allNodes.value.map((n) => -n.pid)) }

// ---- 生命周期 ----
let t1, t2
function useAgentMon() {
  onMounted(() => {
    initLang()
    load()
    connectWS()
    t1 = setInterval(load, 10000)
    t2 = setInterval(() => { clock.value = new Date().toLocaleTimeString() }, 1000)
    clock.value = new Date().toLocaleTimeString()
  })
  onUnmounted(() => { clearInterval(t1); clearInterval(t2); if (ws) try { ws.close() } catch {} })
}

export {
  roots, q, filterState, open, tab, onlyUser, events, eventsLoading,
  clock, loading, loadError, wsConnected, flashSet, alerts,
  focusMode, expanded, states, viewMode, boardColumns, boardNodesTotal,
  criticalAlerts, anyAlert, alertCount, alertsByPid,
  byPid, allNodes, rootNodes, countByState, anyNeedsInput, needsInputCount, attentionCount,
  filteredRoots, ATTENTION,
  labelOf, kindLabel, alertLabel, hasAttention, dv, fmtTime,
  load, connectWS, loadEvents, toggle, toggleOnlyUser,
  isExpanded, toggleExpand, expandAll, collapseAll,
  useAgentMon,
}