import { ref } from 'vue'

// 多语言字典。key 语义化; 所有用户可见文案集中于此, 模板/JS 均通过 t(key) 取用。
// 新增文案只需在 zh/en 两处各加一项, 缺失时回退到 key 本身(便于发现遗漏)。
export const messages = {
  zh: {
    'app.title': 'agent-scope',
    'app.subtitle': 'CLI 编码代理工作状态监控 · 实时推送(WebSocket) · 进程树 / 子代理',
    'app.version': '版本',

    'alert.globalInput': '有 {n} 个代理正在等待你的输入 / 确认,请及时处理',
    'alert.globalCrit': '有 {n} 个异常告警(卡死/错误):',
    'alert.backendFail': '后端连接失败: {msg} ·',
    'alert.retry': '重试',

    'btn.tree': '进程树',
    'btn.board': '状态看板',
    'btn.focus': '仅看活跃',
    'btn.expand': '展开',
    'btn.collapse': '折叠',
    'btn.clearFilter': '清除筛选',

    'search.placeholder': '筛选工具 / pid / 文件 / 连接 / 任务…',
    'search.clear': '清除搜索',

    'filter.all': '全部 {n}',
    'filter.status': '显示 {n} / 共 {m} 个代理',

    'empty.loading': '加载中…',
    'empty.noAgent': '未检测到匹配的代理进程（claude / codex / copilot / aider…）',
    'empty.noEvent': '暂无行为事件',
    'empty.timelineLoading': '加载中…',

    'sub.transcript': '同会话子代理',
    'sub.proc': '子进程',
    'warn.waitInput': '⚠ 等待输入',

    'detail.info': '信息',
    'detail.timeline': '时间线',
    'detail.status': '状态',
    'detail.cmd': '命令',
    'detail.file': '文件',
    'detail.conn': '连接',
    'detail.task': '任务',
    'detail.detail': '详情',
    'detail.timelineTitle': '行为时间线',
    'detail.onlyUser': '只看业务文件',
    'detail.agentInternal': 'agent 内部',

    'chev.collapse': '折叠子树',
    'chev.expand': '展开子树',
    'chev.leaf': '无子节点',

    'state.running': '执行中',
    'state.thinking': '推理中',
    'state.editing': '编辑中',
    'state.waiting': '等待确认',
    'state.idle': '空闲',
    'state.error': '错误',

    'detail.awaiting_approval': '等待用户授权 / 确认',
    'detail.awaiting_input': '等待用户输入',
    'detail.thinking_llm': '调用 LLM / 推理中',
    'detail.thinking_user': '处理用户输入中',
    'detail.idle': '空闲',
    'detail.llm_error': 'LLM 接口错误',

    'kind.cmd': '命令',
    'kind.edit': '编辑',
    'kind.conn': '连接',
    'kind.state': '状态',

    'alert.stuck': '卡死/无响应',
    'alert.wait_unhandled': '等待输入未处理',
    'alert.llm_error': 'LLM/运行错误',
    'alert.proc_gone': '进程残留',
    'alert.secret_leak': '凭据泄露',
    'alert.destructive_cmd': '破坏性命令',

    'board.running': '执行中',
    'board.thinking': '推理中',
    'board.editing': '编辑中',
    'board.waiting': '等待确认',
    'board.idle': '空闲',
    'board.error': '错误',
    'board.anomaly': '异常',

    'theme.toLight': '切换到亮色',
    'theme.toDark': '切换到暗色',
    'lang.switch': '切换语言',
    'lang.zh': '中',
    'lang.en': 'EN',
  },
  en: {
    'app.title': 'agent-scope',
    'app.subtitle': 'CLI coding-agent work-state monitor · realtime push (WebSocket) · process tree / sub-agents',
    'app.version': 'v',

    'alert.globalInput': '{n} agent(s) are waiting for your input / confirmation — please handle promptly',
    'alert.globalCrit': '{n} critical alert(s) (stuck/error):',
    'alert.backendFail': 'Backend connection failed: {msg} ·',
    'alert.retry': 'Retry',

    'btn.tree': 'Tree',
    'btn.board': 'Board',
    'btn.focus': 'Active only',
    'btn.expand': 'Expand',
    'btn.collapse': 'Collapse',
    'btn.clearFilter': 'Clear',

    'search.placeholder': 'Filter tool / pid / file / conn / task…',
    'search.clear': 'Clear',

    'filter.all': 'All {n}',
    'filter.status': 'Showing {n} / {m} agents',

    'empty.loading': 'Loading…',
    'empty.noAgent': 'No matching agent process detected (claude / codex / copilot / aider…)',
    'empty.noEvent': 'No behavior events yet',
    'empty.timelineLoading': 'Loading…',

    'sub.transcript': 'same-session sub-agent',
    'sub.proc': 'child process',
    'warn.waitInput': '⚠ waiting for input',

    'detail.info': 'Info',
    'detail.timeline': 'Timeline',
    'detail.status': 'Status',
    'detail.cmd': 'Command',
    'detail.file': 'File',
    'detail.conn': 'Connection',
    'detail.task': 'Task',
    'detail.detail': 'Detail',
    'detail.timelineTitle': 'Behavior timeline',
    'detail.onlyUser': 'Business files only',
    'detail.agentInternal': 'agent internal',

    'chev.collapse': 'Collapse subtree',
    'chev.expand': 'Expand subtree',
    'chev.leaf': 'no children',

    'state.running': 'Running',
    'state.thinking': 'Thinking',
    'state.editing': 'Editing',
    'state.waiting': 'Waiting',
    'state.idle': 'Idle',
    'state.error': 'Error',

    'detail.awaiting_approval': 'Awaiting approval / confirmation',
    'detail.awaiting_input': 'Waiting for input',
    'detail.thinking_llm': 'Calling LLM / reasoning',
    'detail.thinking_user': 'Processing user input',
    'detail.idle': 'Idle',
    'detail.llm_error': 'LLM API error',

    'kind.cmd': 'Command',
    'kind.edit': 'Edit',
    'kind.conn': 'Conn',
    'kind.state': 'State',

    'alert.stuck': 'Stuck/Unresponsive',
    'alert.wait_unhandled': 'Input pending',
    'alert.llm_error': 'LLM/runtime error',
    'alert.proc_gone': 'Process leftover',
    'alert.secret_leak': 'Secret leaked',
    'alert.destructive_cmd': 'Destructive command',

    'board.running': 'Running',
    'board.thinking': 'Thinking',
    'board.editing': 'Editing',
    'board.waiting': 'Waiting',
    'board.idle': 'Idle',
    'board.error': 'Error',
    'board.anomaly': 'Anomaly',

    'theme.toLight': 'Switch to light',
    'theme.toDark': 'Switch to dark',
    'lang.switch': 'Switch language',
    'lang.zh': '中',
    'lang.en': 'EN',
  },
}

export const locale = ref('zh')

// 初始化语言: 优先 localStorage, 否则跟随浏览器(navigator.language 含 zh → 中文, 否则英文)
export function initLang() {
  const saved = localStorage.getItem('agent-scope-lang')
  if (saved === 'zh' || saved === 'en') {
    locale.value = saved
  } else {
    const nav = (navigator.language || 'en').toLowerCase()
    locale.value = nav.startsWith('zh') ? 'zh' : 'en'
  }
}

export function setLang(l) {
  if (l !== 'zh' && l !== 'en') return
  locale.value = l
  localStorage.setItem('agent-scope-lang', l)
}

// t(key, params?) — 支持 {n}/{m} 占位替换。读响应式 locale, 切语言时模板自动重算。
export function t(key, params) {
  const dict = messages[locale.value] || messages.zh
  let s = dict[key]
  if (s === undefined) s = key // 缺失回退到 key 本身, 便于发现遗漏
  if (params) {
    for (const k in params) s = s.replace('{' + k + '}', params[k])
  }
  return s
}
