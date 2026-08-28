<template>
  <div class="app">
    <header>
      <div class="title">
        <h1>{{ t('app.title') }}</h1>
        <div class="sub">{{ t('app.subtitle') }}</div>
      </div>
      <div class="head-right">
        <span class="ver">v{{ version }}</span>
        <span class="clock">{{ clock }}</span>
        <button class="theme-btn" :title="theme === 'dark' ? t('theme.toLight') : t('theme.toDark')" @click="toggleTheme" aria-label="切换主题">
          {{ theme === 'dark' ? '☀️' : '🌙' }}
        </button>
        <button class="lang-btn" :title="t('lang.switch')" @click="toggleLang" aria-label="切换语言">
          {{ locale === 'zh' ? t('lang.en') : t('lang.zh') }}
        </button>
        <span class="ws-dot" :class="{ connected: wsConnected }" title="WebSocket 连接状态"></span>
      </div>
    </header>

    <div v-if="anyNeedsInput" class="global-alert" role="alert">
      ⚠ {{ t('alert.globalInput', { n: needsInputCount }) }}
    </div>

    <div v-if="criticalAlerts.length" class="global-critical" role="alert">
      🔴 {{ t('alert.globalCrit', { n: criticalAlerts.length }) }}
      <span v-for="a in criticalAlerts.slice(0,3)" :key="a.id" class="crit-item">
        {{ a.tool }}#{{ a.pid }} {{ alertLabel(a.kind) }}
      </span>
      <span v-if="criticalAlerts.length > 3">等</span>
    </div>

    <div v-if="loadError" class="global-error" role="alert">
      🔴 {{ t('alert.backendFail', { msg: loadError }) }} <button class="retry-btn" @click="loading=true; load()">{{ t('alert.retry') }}</button>
    </div>

    <div v-if="anyAlert" class="alerts-panel">
      <div class="alerts-title">异常告警 ({{ alertCount }})</div>
      <div v-for="a in alerts" :key="a.id" class="alert-row" :class="a.level">
        <span class="a-time">{{ fmtTime(a.ts) }}</span>
        <span class="a-level">{{ a.level === 'critical' ? '严重' : '警告' }}</span>
        <span class="a-kind">{{ alertLabel(a.kind) }}</span>
        <span class="a-msg">{{ a.tool }}#{{ a.pid }} — {{ a.message }}</span>
      </div>
    </div>

    <div class="view-toggle">
      <button class="chip" :class="{ active: viewMode === 'tree' }" @click="viewMode = 'tree'">🌲 {{ t('btn.tree') }}</button>
      <button class="chip" :class="{ active: viewMode === 'board' }" @click="viewMode = 'board'">📋 {{ t('btn.board') }}</button>
    </div>

    <div class="toolbar">
      <div class="tb-row">
        <div class="search-wrap">
          <input v-model.trim="q" class="search" :placeholder="t('search.placeholder')" />
          <button v-if="q" class="search-x" @click="q = ''" :aria-label="t('search.clear')">✕</button>
        </div>
        <button class="chip focus" :class="{ active: focusMode }" @click="focusMode = !focusMode" :title="t('btn.focus')">⚡ {{ t('btn.focus') }}<span v-if="focusMode && attentionCount">{{ attentionCount }}</span></button>
        <button class="chip" @click="expandAll()" :aria-label="t('btn.expand')">{{ t('btn.expand') }}</button>
        <button class="chip" @click="collapseAll()" :aria-label="t('btn.collapse')">{{ t('btn.collapse') }}</button>
      </div>
      <div class="filters" role="group" :aria-label="t('btn.filter')">
        <button
          v-for="s in states"
          :key="s"
          class="chip"
          :class="[s, { active: filterState === s }]"
          @click="filterState = filterState === s ? '' : s"
        >{{ labelOf(s) }} <i v-if="countByState[s]">{{ countByState[s] }}</i></button>
        <button class="chip all" :class="{ active: filterState === '' }" @click="filterState = ''">{{ t('filter.all', { n: rootNodes.length }) }}</button>
      </div>
      <div v-if="isFiltering" class="filter-status">
        <span class="fs-text">{{ t('filter.status', { n: filteredRoots.length, m: rootNodes.length }) }}</span>
        <button class="chip clear" @click="clearFilters">{{ t('btn.clearFilter') }}</button>
      </div>
    </div>

    <main class="groups" v-if="viewMode === 'tree'">
      <AgentNode
        v-for="root in filteredRoots"
        :key="root.pid"
        :node="root"
        :depth="0"
        :by-pid="byPid"
      />
    </main>

    <main class="board" v-else>
      <section
        v-for="col in boardColumns"
        :key="col.key"
        class="board-col"
        :class="col.key"
      >
        <div class="board-col-head">
          <span class="col-dot" :class="col.key"></span>
          <span class="col-label">{{ col.label }}</span>
          <span class="col-count">{{ col.count }}</span>
        </div>
        <div class="board-col-body">
          <AgentNode
            v-for="n in col.nodes"
            :key="n.pid"
            :node="n"
            :depth="0"
            :by-pid="byPid"
            :board="true"
          />
          <div v-if="!col.nodes.length" class="board-empty">—</div>
        </div>
      </section>
    </main>

    <div v-if="loading" class="empty">{{ t('empty.loading') }}</div>
    <div v-else-if="loadError" class="empty error">⚠ {{ t('alert.backendFail', { msg: loadError }) }}</div>
    <div v-else-if="viewMode === 'tree' && !filteredRoots.length" class="empty">
      <template v-if="isFiltering">{{ t('empty.noAgent') }}<button class="chip clear" @click="clearFilters">{{ t('btn.clearFilter') }}</button></template>
      <template v-else>{{ t('empty.noAgent') }}</template>
    </div>
    <div v-else-if="viewMode === 'board' && !boardNodesTotal" class="empty">
      <template v-if="isFiltering">{{ t('empty.noAgent') }}<button class="chip clear" @click="clearFilters">{{ t('btn.clearFilter') }}</button></template>
      <template v-else>{{ t('empty.noAgent') }}</template>
    </div>
  </div>
</template>

<script setup>
import { computed, ref, onMounted } from 'vue'
import AgentNode from './components/AgentNode.vue'
import {
  roots, q, filterState, open, tab, onlyUser, events, eventsLoading,
  clock, loading, loadError, wsConnected, alerts,
  focusMode, expanded, states, viewMode, boardColumns,
  criticalAlerts, anyAlert, alertCount,
  rootNodes, countByState, anyNeedsInput, needsInputCount, attentionCount,
  filteredRoots, byPid, boardNodesTotal,
  labelOf, alertLabel, fmtTime,
  load, loadEvents, toggle,
  isExpanded, toggleExpand, expandAll, collapseAll,
  useAgentMon,
} from './composables/useAgentMon.js'
import { t, locale, setLang } from './i18n.js'

const version = import.meta.env.VITE_APP_VERSION || 'dev'
useAgentMon()

// 语言切换: 在 zh / en 间切换并持久化
function toggleLang() {
  setLang(locale.value === 'zh' ? 'en' : 'zh')
}

// 主题: 默认跟随系统偏好(prefers-color-scheme), 之后 localStorage 记忆用户选择。
// 应用到 <html data-theme>, CSS 变量据此切换暗/亮。
const theme = ref('dark')
function applyTheme(t) {
  document.documentElement.setAttribute('data-theme', t)
  theme.value = t
}
function initTheme() {
  const saved = localStorage.getItem('agent-scope-theme')
  if (saved === 'light' || saved === 'dark') {
    applyTheme(saved)
  } else {
    const prefersLight = window.matchMedia && window.matchMedia('(prefers-color-scheme: light)').matches
    applyTheme(prefersLight ? 'light' : 'dark')
  }
}
function toggleTheme() {
  const next = theme.value === 'dark' ? 'light' : 'dark'
  applyTheme(next)
  localStorage.setItem('agent-scope-theme', next)
}
onMounted(initTheme)

// 是否有任何筛选生效(搜索词 / 状态筛选 / 仅看活跃)
const isFiltering = computed(() => filterState.value !== '' || q.value !== '' || focusMode.value)
// 统一清除所有筛选(解决"agent 被隐藏却不知为何"的困惑)
function clearFilters() {
  q.value = ''
  filterState.value = ''
  focusMode.value = false
}
</script>

<style>
:root {
  --bg: #0f1115; --panel: #161b22; --border: #23272e; --fg: #e6e6e6;
  --muted: #8b949e; --muted2: #8b949e; --code: #c9d1d9;
  --running: #3fb950; --thinking: #58a6ff; --editing: #e3a857;
  --waiting: #d29922; --idle: #8b949e; --error: #f85149; --blocked: #f85149;
  /* 派生变量(暗色下为硬编码色的变量化版本, 亮色主题统一覆盖) */
  --hover-bg: #1b212a;        /* 卡片 hover 背景 */
  --raw-bg: #0d1117;          /* 原始 JSON 文本块背景 */
  --tl-border: #1c2128;       /* 时间线分隔线 */
  --tl-toggle-bg: #11161d;    /* 时间线折叠按钮背景 */
  --chev-bg: #11161d;         /* 树展开箭头背景 */
  --active-blue: #11233f;     /* chip 选中蓝底 */
  --focus-blue: #388bfd;      /* 聚焦/选中边框蓝 */
  --chip-green-bg: #14241a;   /* tl-toggle 选中绿底 */
  --sub-tag-bg: #1a2b4d;      /* 子代理标签底 */
  --sub-tag-border: #1f6feb;  /* 子代理标签边框 */
  --info-fg: #adbac7;         /* info-grid 文本 */
  --tl-detail: #adbac7;       /* 时间线详情文本 */
  --tl-kind-bg: var(--border);/* 时间线 kind 标签底 */
  --tl-tag-bg: #3a2d12;       /* 时间线 tag 底 */
  --tl-tag-border: #5c4717;   /* 时间线 tag 边框 */
}
/* 亮色主题: GitHub light 风格 */
[data-theme="light"] {
  --bg: #ffffff; --panel: #f6f8fa; --border: #d0d7de; --fg: #1f2328;
  --muted: #656d76; --muted2: #656d76; --code: #1f2328;
  --running: #1a7f37; --thinking: #0969da; --editing: #9a6700;
  --waiting: #9a6700; --idle: #656d76; --error: #cf222e; --blocked: #cf222e;
  --hover-bg: #eaeef2;
  --raw-bg: #f6f8fa;
  --tl-border: #d8dee4;
  --tl-toggle-bg: #eaeef2;
  --chev-bg: #eaeef2;
  --active-blue: #ddf4ff;
  --focus-blue: #0969da;
  --chip-green-bg: #dafbe1;
  --sub-tag-bg: #ddf4ff;
  --sub-tag-border: #54aeff;
  --info-fg: #59636e;
  --tl-detail: #59636e;
  --tl-kind-bg: #eaeef2;
  --tl-tag-bg: #fff8c5;
  --tl-tag-border: #d4a72c;
}

* { box-sizing: border-box; }
body { margin: 0; font-family: -apple-system, "Segoe UI", system-ui, sans-serif; background: var(--bg); color: var(--fg); }
.app { min-height: 100vh; }
header { padding: 16px 24px; border-bottom: 1px solid var(--border); display: flex; align-items: center; justify-content: space-between; }
h1 { font-size: 18px; margin: 0; }
.sub { color: var(--muted); font-size: 13px; margin-top: 2px; }
.head-right { display: flex; align-items: center; gap: 12px; color: var(--muted); font-size: 13px; }
.ver { background: var(--panel); border: 1px solid var(--border); padding: 2px 8px; border-radius: 6px; }
.ws-dot { width: 8px; height: 8px; border-radius: 50%; background: #da3633; transition: background .3s; flex: 0 0 auto; }
.ws-dot.connected { background: #3fb950; }
.theme-btn { background: var(--panel); border: 1px solid var(--border); color: var(--fg); border-radius: 6px; padding: 2px 8px; font-size: 14px; line-height: 1.4; cursor: pointer; transition: all .15s; }
.theme-btn:hover { border-color: var(--focus-blue); }
.lang-btn { background: var(--panel); border: 1px solid var(--border); color: var(--fg); border-radius: 6px; padding: 2px 8px; font-size: 12px; line-height: 1.4; cursor: pointer; transition: all .15s; min-width: 30px; }
.lang-btn:hover { border-color: var(--focus-blue); }

.global-alert { background: #3d3416; color: #f0c674; border: 1px solid #d29922; border-radius: 8px; padding: 10px 18px; margin: 10px 24px 0; font-size: 14px; font-weight: 600; }
.global-critical { background: #461c1c; color: #ff9a9a; border: 1px solid #da3633; border-radius: 8px; padding: 10px 18px; margin: 10px 24px 0; font-size: 14px; font-weight: 700; }
.global-critical .crit-item { display: inline-block; background: #5c2222; border: 1px solid #da3633; border-radius: 6px; padding: 2px 8px; margin-left: 8px; font-weight: 600; font-size: 12px; }
.global-error { background: #461c1c; color: #ff9a9a; border: 1px solid #da3633; border-radius: 8px; padding: 10px 18px; margin: 10px 24px 0; font-size: 14px; display: flex; align-items: center; gap: 8px; }
.global-error .retry-btn { background: #5c2222; border: 1px solid #da3633; color: #ff9a9a; border-radius: 6px; padding: 2px 12px; font-size: 12px; cursor: pointer; }

.alerts-panel { margin: 10px 24px 0; background: var(--panel); border: 1px solid var(--border); border-radius: 8px; padding: 10px 14px; }
.alerts-title { font-size: 13px; color: var(--muted); margin-bottom: 6px; font-weight: 600; }
.alert-row { display: flex; gap: 10px; align-items: baseline; font-size: 12px; padding: 4px 0; border-bottom: 1px solid var(--border); }
.alert-row:last-child { border-bottom: none; }
.alert-row.critical { color: #ff9a9a; }
.alert-row.warning { color: #f0c674; }
.a-time { color: var(--muted2); font-variant-numeric: tabular-nums; }
.a-level { font-weight: 700; }
.a-kind { font-weight: 600; }
.a-msg { color: var(--fg); flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.anomaly-banner { background: #461c1c; color: #ff9a9a; border: 1px solid #da3633; border-radius: 6px; padding: 4px 10px; font-size: 12px; font-weight: 600; margin-bottom: 8px; }
.anomaly-banner.warning { background: #3d3416; color: #f0c674; border-color: #d29922; }

.toolbar { padding: 14px 24px; display: flex; gap: 14px; flex-wrap: wrap; align-items: center; }
.tb-row { display: flex; gap: 6px; align-items: center; flex-wrap: wrap; }
.search { background: var(--panel); border: 1px solid var(--border); color: var(--fg); border-radius: 8px; padding: 8px 12px; width: 280px; font-size: 13px; outline: none; }
.search:focus { border-color: var(--focus-blue); }
.search-wrap { position: relative; display: flex; align-items: center; }
.search-x { position: absolute; right: 8px; width: 20px; height: 20px; display: inline-flex; align-items: center; justify-content: center; border: none; background: var(--border); color: var(--muted); border-radius: 50%; font-size: 11px; cursor: pointer; padding: 0; line-height: 1; }
.search-x:hover { background: #388bfd; color: #fff; }
.search { padding-right: 30px; }
.filters { display: flex; gap: 8px; flex-wrap: wrap; }
.chip { background: transparent; border: 1px solid var(--border); color: var(--muted); border-radius: 999px; padding: 4px 12px; font-size: 12px; cursor: pointer; transition: all .15s; }
.chip:hover { color: var(--fg); }
.chip.active { color: var(--fg); border-color: var(--focus-blue); background: var(--active-blue); }
.chip i { font-style: normal; opacity: .7; margin-left: 4px; }
.chip.all.active { border-color: #388bfd; }
.chip.focus.active { color: #f0c674; border-color: #d29922; background: #3d3416; }
/* 活动态状态筛选 chip: 轻微高亮(执行中/推理中/编辑中/等待确认) */
.chip.running.active { background: #11261a; border-color: #2ea043; color: #7ee787; }
.chip.thinking.active { background: #11233f; border-color: #388bfd; color: #79c0ff; }
.chip.editing.active { background: #2e2410; border-color: #d29922; color: #e3b341; }
.chip.waiting.active { background: #2e2410; border-color: #d29922; color: #e3b341; }
.chip.error.active { background: #461c1c; border-color: #da3633; color: #ff9a9a; }

.filter-status { display: flex; align-items: center; gap: 10px; color: var(--muted); font-size: 12px; padding: 2px 0; }
.filter-status .fs-text b { color: var(--fg); }
.chip.clear { border-color: #388bfd; color: #79c0ff; }
.chip.clear:hover { background: #11233f; color: var(--fg); }

.view-toggle { padding: 10px 24px 0; display: flex; gap: 8px; }
.view-toggle .chip.active { color: var(--fg); border-color: var(--focus-blue); background: var(--active-blue); }

/* Kanban 状态看板 */
.board { padding: 12px 24px 32px; display: flex; gap: 12px; align-items: flex-start; overflow-x: auto; }
.board-col { flex: 1 1 0; min-width: 180px; background: var(--panel); border: 1px solid var(--border); border-radius: 10px; display: flex; flex-direction: column; max-height: calc(100vh - 220px); }
.board-col.anomaly { border-color: #da3633; }
.board-col-head { display: flex; align-items: center; gap: 8px; padding: 10px 12px; border-bottom: 1px solid var(--border); font-weight: 600; font-size: 13px; position: sticky; top: 0; background: var(--panel); border-radius: 10px 10px 0 0; }
.col-dot { width: 9px; height: 9px; border-radius: 50%; background: var(--muted); flex: 0 0 auto; }
.col-dot.running { background: var(--running); }
.col-dot.thinking { background: var(--thinking); }
.col-dot.editing { background: var(--editing); }
.col-dot.waiting { background: var(--waiting); }
.col-dot.idle { background: var(--idle); }
.col-dot.error { background: var(--error); }
.col-dot.anomaly { background: var(--blocked); }
.col-label { flex: 1; }
.col-count { background: var(--bg); border: 1px solid var(--border); border-radius: 999px; padding: 0 8px; font-size: 11px; color: var(--muted); }
.board-col-body { padding: 8px; display: flex; flex-direction: column; gap: 6px; overflow-y: auto; }
.board-empty { color: var(--muted); text-align: center; padding: 12px 0; font-size: 12px; }
/* 看板卡片: 复用 .card, 但去掉左边距(非树形缩进) */
.board .card { margin-left: 0 !important; }

.groups { padding: 8px 24px 32px; display: flex; flex-direction: column; gap: 6px; }
.card { background: var(--panel); border: 1px solid var(--border); border-left: 3px solid transparent; border-radius: 8px; padding: 7px 12px; cursor: pointer; transition: border-color .2s, background .15s; }
.card:hover { background: var(--hover-bg); }
.card.running { border-left-color: var(--running); }
.card.thinking { border-left-color: var(--thinking); }
.card.editing { border-left-color: var(--editing); }
.card.waiting { border-left-color: var(--waiting); }
.card.idle { border-left-color: var(--idle); }
.card.error { border-left-color: var(--error); }
.card.flash { animation: flash 1.2s ease; }
@keyframes flash {
  0% { box-shadow: 0 0 0 0 rgba(88,166,255,.7); }
  100% { box-shadow: 0 0 0 8px rgba(88,166,255,0); }
}
.card.needinput { border-left-color: #d29922; border-color: #d29922; }
.card.anomaly { border-left-color: #da3633; border-color: #da3633; }

.row { display: flex; align-items: center; gap: 8px; min-width: 0; }
.chev { flex: 0 0 auto; width: 22px; height: 22px; display: inline-flex; align-items: center; justify-content: center; border-radius: 5px; color: var(--muted); font-size: 14px; font-weight: 700; cursor: pointer; user-select: none; transition: transform .15s, background .15s, color .15s; border: 1px solid var(--border); background: var(--chev-bg); }
.chev:hover { background: #1f6feb; color: #fff; border-color: #1f6feb; }
.chev.open { transform: rotate(90deg); color: var(--fg); }
.chev.leaf { visibility: hidden; }
.dot { flex: 0 0 auto; width: 9px; height: 9px; border-radius: 50%; background: var(--muted); }
.dot.running { background: var(--running); }
.dot.thinking { background: var(--thinking); }
.dot.editing { background: var(--editing); }
.dot.waiting { background: var(--waiting); }
.dot.idle { background: var(--idle); }
.dot.error { background: var(--error); }
.tool { font-weight: 600; font-size: 13.5px; flex: 0 0 auto; }
.sub-tag { font-size: 9px; background: var(--sub-tag-bg); color: var(--thinking); border: 1px solid var(--sub-tag-border); border-radius: 4px; padding: 0 5px; margin-left: 5px; vertical-align: middle; }
.pid { color: var(--muted2); font-size: 11px; flex: 0 0 auto; }
.warn-tag { flex: 0 0 auto; font-size: 11px; color: #f0c674; background: #3d3416; border: 1px solid #d29922; border-radius: 4px; padding: 1px 6px; }
.warn-tag.crit { color: #ff9a9a; background: #461c1c; border-color: #da3633; }
.state-text { color: var(--code); font-size: 12.5px; flex: 1 1 auto; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; margin-left: 4px; }

.kids { margin-top: 4px; display: flex; flex-direction: column; gap: 4px; border-left: 1px solid var(--border); padding-left: 6px; margin-left: 6px; }

.more { margin-top: 12px; border-top: 1px solid var(--border); padding-top: 10px; }
.more-tabs { display: flex; gap: 8px; margin-bottom: 10px; }
.more-tabs button { background: transparent; border: 1px solid var(--border); color: var(--muted); border-radius: 6px; padding: 3px 12px; font-size: 12px; cursor: pointer; }
.more-tabs button.active { color: var(--fg); border-color: var(--focus-blue); background: var(--active-blue); }
.info-grid { font-size: 12px; color: var(--info-fg); display: flex; flex-direction: column; gap: 4px; }
.info-grid b { color: var(--fg); font-weight: 600; margin-right: 6px; }
.info-grid .raw { font-family: ui-monospace, Menlo, monospace; color: var(--code); background: var(--raw-bg); border-radius: 6px; padding: 8px; margin-top: 4px; word-break: break-all; white-space: pre-wrap; }

.timeline { max-height: 240px; overflow-y: auto; font-size: 12px; }
.tl-loading, .tl-empty { color: var(--muted); padding: 8px 0; }
.tl-item { display: flex; align-items: center; gap: 8px; padding: 5px 0; border-bottom: 1px solid var(--tl-border); }
.tl-dot { width: 7px; height: 7px; border-radius: 50%; background: var(--muted2); flex: 0 0 auto; }
.tl-item.cmd .tl-dot { background: #58a6ff; }
.tl-item.edit .tl-dot { background: var(--editing); }
.tl-item.conn .tl-dot { background: var(--thinking); }
.tl-item.state .tl-dot { background: var(--running); }
.tl-time { color: var(--muted2); font-variant-numeric: tabular-nums; }
.tl-kind { color: var(--muted); background: var(--tl-kind-bg); border-radius: 4px; padding: 0 6px; flex: 0 0 auto; }
.tl-detail { color: var(--tl-detail); word-break: break-all; }
.tl-bar { display: flex; align-items: center; justify-content: space-between; margin-bottom: 4px; }
.tl-title { color: var(--muted); font-size: 11px; letter-spacing: .04em; }
.tl-toggle { font-size: 11px; padding: 2px 8px; border-radius: 5px; border: 1px solid var(--border); background: var(--tl-toggle-bg); color: var(--muted); cursor: pointer; }
.tl-toggle.active { color: #7ee787; border-color: #2ea043; background: var(--chip-green-bg); }
.tl-tag { font-size: 10px; padding: 0 5px; border-radius: 4px; background: var(--tl-tag-bg); color: #d29922; border: 1px solid var(--tl-tag-border); flex: 0 0 auto; }

.empty { padding: 40px 24px; color: var(--muted); text-align: center; }
.empty.error { color: #ff9a9a; }

.card-enter-active, .card-leave-active { transition: all .3s ease; }
.card-enter-from, .card-leave-to { opacity: 0; transform: translateY(8px); }
.expand-enter-active, .expand-leave-active { transition: all .2s ease; }
.expand-enter-from, .expand-leave-to { opacity: 0; max-height: 0; }

@media (max-width: 720px) {
  header { flex-wrap: wrap; gap: 8px; padding: 12px 16px; }
  .head-right { width: 100%; justify-content: flex-end; }
  .toolbar { padding: 10px 16px; flex-direction: column; align-items: stretch; gap: 8px; }
  .tb-row { display: flex; gap: 6px; flex-wrap: wrap; }
  .tb-row .search { width: 100%; }
  .filters { flex-wrap: wrap; }
  .groups { padding: 6px 12px 24px; }
  .card { padding: 6px 10px; }
  .tool { font-size: 12.5px; }
  .state-text { font-size: 11.5px; }
  .more-tabs { flex-wrap: wrap; }
}
</style>