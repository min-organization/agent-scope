<template>
  <article
    v-if="node"
    class="card"
    :class="[node.state, { flash: flashSet.has(node.pid), needinput: node.needs_input, anomaly: alertsByPid[node.pid] }]"
    :style="{ marginLeft: depth > 0 ? (depth * 14) + 'px' : '0' }"
  >
    <div class="row" @click.stop="toggle(node)">
      <span
        v-if="!board"
        class="chev"
        :class="{ open: hasKids && isExpanded(node), leaf: !hasKids }"
        :title="hasKids ? (isExpanded(node) ? t('chev.collapse') : t('chev.expand')) : ''"
        :aria-label="hasKids ? (isExpanded(node) ? t('chev.collapse') : t('chev.expand')) : t('chev.leaf')"
        role="button"
        tabindex="0"
        @click.stop="hasKids && toggleExpand(node)"
        @keydown.enter.stop="hasKids && toggleExpand(node)"
      >{{ hasKids ? '▸' : '' }}</span>
      <span class="dot" :class="node.state" :aria-label="'状态: ' + labelOf(node.state)"></span>
      <span class="tool">
        {{ node.tool }}
        <span v-if="node.is_subagent" class="sub-tag">{{ node.src === 'transcript' ? t('sub.transcript') : t('sub.proc') }}</span>
      </span>
      <span class="pid">pid {{ node.pid }}</span>
      <span v-if="node.needs_input" class="warn-tag">{{ t('warn.waitInput') }}</span>
      <span v-else-if="alertsByPid[node.pid]" class="warn-tag crit">⚠ {{ alertLabel(alertsByPid[node.pid].kind) }}</span>
      <span class="state-text">
        <template v-if="node.is_subagent && node.task">📋 {{ node.task }}</template>
        <template v-else>{{ labelOf(node.state) }}</template>
        <template v-if="node.last_edit_file && node.last_edit_file !== 'null'"> · 📝 {{ node.last_edit_file }}</template>
      </span>
    </div>

    <!-- 子树: 折叠时隐藏(默认: 有异常/等待的 root 自动展开, 其余折叠); 看板模式不渲染子树 -->
    <div v-if="!board && isExpanded(node) && hasKids" class="kids">
      <AgentNode
        v-for="cid in node.children"
        :key="cid"
        :node="byPid[cid]"
        :depth="depth + 1"
        :by-pid="byPid"
      />
    </div>

    <!-- 详情下钻(点击卡片展开, 不随树折叠) -->
    <transition name="expand">
      <div v-if="open === node.pid" class="more">
        <div class="more-tabs">
          <button :class="{ active: tab !== 'timeline' }" @click.stop="tab = 'info'">{{ t('detail.info') }}</button>
          <button :class="{ active: tab === 'timeline' }" @click.stop="loadEvents(node)">{{ t('detail.timeline') }}</button>
        </div>
        <div v-if="tab !== 'timeline'" class="info-grid">
          <div><b>{{ t('detail.status') }}</b> {{ labelOf(node.state) }} <small>({{ node.state }})</small></div>
          <div><b>{{ t('detail.cmd') }}</b> {{ dv(node.last_cmd) || '—' }}</div>
          <div><b>{{ t('detail.file') }}</b> {{ dv(node.last_edit_file) || dv(node.last_file) || '—' }}</div>
          <div><b>{{ t('detail.conn') }}</b> {{ dv(node.last_conn) || '—' }}</div>
          <div><b>{{ t('detail.task') }}</b> {{ dv(node.task) || '—' }}</div>
          <div><b>{{ t('detail.detail') }}</b> {{ reasonLabel(node.state_reason, node.state_error_code) }}</div>
          <div class="raw">{{ dv(node.last_text) || '—' }}</div>
        </div>
        <div v-else class="timeline">
          <div class="tl-bar">
            <span class="tl-title">{{ t('detail.timelineTitle') }}</span>
            <button class="tl-toggle" :class="{ active: onlyUser }" @click="toggleOnlyUser">{{ t('detail.onlyUser') }}</button>
          </div>
          <div v-if="eventsLoading" class="tl-loading">{{ t('empty.timelineLoading') }}</div>
          <div v-else-if="!events.length" class="tl-empty">{{ t('empty.noEvent') }}</div>
          <div v-for="(e, i) in events" :key="i" class="tl-item" :class="e.kind">
            <span class="tl-dot"></span>
            <span class="tl-time">{{ fmtTime(e.ts) }}</span>
            <span class="tl-kind">{{ kindLabel(e.kind) }}</span>
            <span class="tl-detail">{{ e.detail }}</span>
            <span v-if="e.kind === 'edit' && e.file_kind === 'agent_temp'" class="tl-tag">{{ t('detail.agentInternal') }}</span>
          </div>
        </div>
      </div>
    </transition>
  </article>
</template>

<script setup>
import { computed } from 'vue'
import {
  open, tab, events, eventsLoading, flashSet, alertsByPid, onlyUser,
  labelOf, alertLabel, kindLabel, dv, fmtTime,
  loadEvents, toggle, isExpanded, toggleExpand, toggleOnlyUser,
} from '../composables/useAgentMon.js'
import { t } from '../i18n.js'

const props = defineProps({ node: Object, depth: Number, byPid: Object, board: Boolean })

const hasKids = computed(() => !!(props.node.children && props.node.children.length))

// reasonLabel 把后端返回的 state_reason 枚举(可本地化)渲染为文案。
// llm_error 时附上错误码(如 429); 其他原因直接走 i18n; 空原因返回 '—'。
function reasonLabel(reason, errorCode) {
  if (!reason) return '—'
  let s = t('detail.' + reason)
  if (reason === 'llm_error' && errorCode) s += ' (' + errorCode + ')'
  return s
}
</script>
