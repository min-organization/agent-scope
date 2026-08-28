package collector

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"agentmon/internal/store"
)

// TestTranscriptStateAPIError 验证 claude transcript 的 apiErrorStatus(如 429) 被推导为 error 状态,
// 且返回结构化的 reason 枚举 + errorCode(供前端 i18n 渲染, 不再硬编码中文 detail)。
func TestTranscriptStateAPIError(t *testing.T) {
	idleNs := int64(60 * 1e9)
	now := time.Now().UnixNano()
	recent := func() int64 { return now - 5*1e9 } // 5s 前(近期活动)

	// 429 rate_limit: assistant 行含 apiErrorStatus=429 + error=rate_limit(近期发生)
	t429 := transcriptInfo{
		lastType:      "assistant",
		lastApiStatus: "429",
		lastApiErr:    "rate_limit",
		lastApiErrMsg: true,
		lastTs:        recent(),
		lastApiTs:     recent(),
	}
	st, reason, code := transcriptState(t429, idleNs)
	if st != "error" {
		t.Fatalf("期望 429 -> error, 实际 %q", st)
	}
	if reason != store.ReasonLLMError {
		t.Fatalf("期望 429 -> reason=llm_error, 实际 %q", reason)
	}
	if code != "429" {
		t.Fatalf("期望 429 -> errorCode=429, 实际 %q", code)
	}

	// 普通 assistant(无错误) -> thinking
	tThink := transcriptInfo{lastType: "assistant", lastTs: recent()}
	if st, _, _ := transcriptState(tThink, idleNs); st != "thinking" {
		t.Fatalf("期望 assistant(无错) -> thinking, 实际 %q", st)
	}

	// user -> 处理中(thinking, 统一语义: 不再用"等待 agent 响应"歧义)
	tUser := transcriptInfo{lastType: "user", lastTs: recent()}
	if st, _, _ := transcriptState(tUser, idleNs); st != "thinking" {
		t.Fatalf("期望 user -> thinking(处理中), 实际 %q", st)
	}

	// 未知内部类型 -> idle(绝不回落 running)
	tUnknown := transcriptInfo{lastType: "file-history-snapshot", lastTs: recent()}
	if st, _, _ := transcriptState(tUnknown, idleNs); st != "idle" {
		t.Fatalf("期望 未知类型 -> idle, 实际 %q", st)
	}

	// 陈旧 429(error 行发生在 600s 前, 无近期活动): 不应再显示 error —— error 新鲜度基准是
	// error 行自身时间(lastApiTs), 陈旧 error 已被后续活动超越, 退化到 system -> idle。
	// (真正\"已恢复\"由 readTranscript 遇正常 assistant 行清除 errStatus 覆盖; 真正\"刚发生\"的
	// 新鲜 error 由 TestTranscriptStateErrorFreshAfterUserResend 覆盖。)
	tStale := transcriptInfo{lastType: "system", lastApiStatus: "429", lastTs: now - 600*1e9, lastApiTs: now - 600*1e9}
	if st, _, _ := transcriptState(tStale, idleNs); st != "idle" {
		t.Fatalf("期望 陈旧错误(600s 前)被隐藏 -> idle, 实际 %q", st)
	}

	// 普通 tool_use 表示工具正在调度/执行，不能仅凭 stop_reason 猜测等待批准。
	tFreshTool := transcriptInfo{lastType: "assistant", lastStopReason: "tool_use", lastToolName: "Read", lastTs: recent()}
	if st, _, _ := transcriptState(tFreshTool, idleNs); st != "thinking" {
		t.Fatalf("期望 新鲜普通 tool_use -> thinking, 实际 %q", st)
	}

	// 明确要求用户回答的交互工具才是确定性等待。
	tAskUser := transcriptInfo{lastType: "assistant", lastStopReason: "tool_use", lastToolName: "AskUserQuestion", lastTs: recent()}
	if st, reason, _ := transcriptState(tAskUser, idleNs); st != "waiting" || reason != store.ReasonAwaitingInput {
		t.Fatalf("期望 AskUserQuestion -> waiting/awaiting_input, 实际 %q/%q", st, reason)
	}

	// 超时 + 末行 assistant 但非 tool_use(对话空闲/生成完等下一句) -> idle(不误判 waiting)。
	tAssistIdle := transcriptInfo{lastType: "assistant", lastStopReason: "end_turn", lastTs: now - 600*1e9}
	if st, _, _ := transcriptState(tAssistIdle, idleNs); st != "idle" {
		t.Fatalf("期望 超时 assistant(非tool_use) -> idle, 实际 %q", st)
	}
}

// TestReadTranscriptParsesAPIError 验证 readTranscript 解析真实 claude 格式的 apiErrorStatus/error 字段。
func TestReadTranscriptParsesAPIError(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "session.jsonl")
	now := time.Now().UTC()
	// 真实 claude 格式: 末行 assistant 含 apiErrorStatus=429(用当前时间, 避免 age 守卫误判 idle)
	content := `{"type":"user","timestamp":"` + now.Add(-5*time.Second).Format(time.RFC3339Nano) + `"}
{"type":"assistant","timestamp":"` + now.Format(time.RFC3339Nano) + `","apiErrorStatus":429,"error":"rate_limit","isApiErrorMessage":true}
`
	if err := os.WriteFile(f, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	info := readTranscript(f, "claude", map[string]*agentMonitor{})
	if info == nil {
		t.Fatal("readTranscript 返回 nil")
	}
	if info.lastType != "assistant" {
		t.Fatalf("lastType 期望 assistant, 实际 %q", info.lastType)
	}
	if info.lastApiStatus != "429" {
		t.Fatalf("lastApiStatus 期望 429, 实际 %q", info.lastApiStatus)
	}
	if info.lastApiErr != "rate_limit" {
		t.Fatalf("lastApiErr 期望 rate_limit, 实际 %q", info.lastApiErr)
	}
	st, _, _ := transcriptState(*info, 60*1e9)
	if st != "error" {
		t.Fatalf("期望 error, 实际 %q", st)
	}
}

func TestReadTranscriptDistinguishesInteractiveTool(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "session.jsonl")
	now := time.Now().UTC().Format(time.RFC3339Nano)
	content := `{"type":"assistant","timestamp":"` + now + `","message":{"stop_reason":"tool_use","content":[{"type":"tool_use","name":"AskUserQuestion"}]}}` + "\n"
	if err := os.WriteFile(f, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	info := readTranscript(f, "claude", map[string]*agentMonitor{})
	if info == nil {
		t.Fatal("readTranscript 返回 nil")
	}
	if info.lastToolName != "AskUserQuestion" {
		t.Fatalf("lastToolName 期望 AskUserQuestion, 实际 %q", info.lastToolName)
	}
	if st, reason, _ := transcriptState(*info, 60*1e9); st != "waiting" || reason != store.ReasonAwaitingInput {
		t.Fatalf("显式交互工具期望 waiting/awaiting_input, 实际 %q/%q", st, reason)
	}
}

func TestReadTranscriptTracksUnresolvedParallelTools(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "session.jsonl")
	now := time.Now().UTC()
	content := `{"type":"assistant","timestamp":"` + now.Add(-time.Second).Format(time.RFC3339Nano) + `","message":{"stop_reason":"tool_use","content":[{"type":"tool_use","id":"read-1","name":"Read"},{"type":"tool_use","id":"bash-1","name":"Bash"}]}}
{"type":"user","timestamp":"` + now.Format(time.RFC3339Nano) + `","message":{"content":[{"type":"tool_result","tool_use_id":"read-1","content":"done"}]}}
`
	if err := os.WriteFile(f, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	info := readTranscript(f, "claude", map[string]*agentMonitor{})
	if info == nil {
		t.Fatal("readTranscript 返回 nil")
	}
	if info.pendingToolCount != 1 || info.pendingToolName != "Bash" {
		t.Fatalf("期望仅 Bash 未完成, 实际 count=%d name=%q", info.pendingToolCount, info.pendingToolName)
	}

	running := claudeSource{}.DeriveTranscriptState(info, 60*1e9, false)
	if running.State != store.StateThinking || running.NeedsInput {
		t.Fatalf("工具未完成但进程活跃应为 thinking, 实际 state=%q needs=%v", running.State, running.NeedsInput)
	}
	waiting := claudeSource{}.DeriveTranscriptState(info, 60*1e9, true)
	if waiting.State != store.StateWaiting || waiting.Reason != store.ReasonAwaitingApproval || !waiting.NeedsInput {
		t.Fatalf("工具未完成且终端等待应为 waiting/awaiting_approval, 实际 state=%q reason=%q needs=%v",
			waiting.State, waiting.Reason, waiting.NeedsInput)
	}
}

// TestReadTranscriptCapturesNonLastErrorLine 验证: 429 错误行不在文件末尾(其后还有正常行)时,
// 仍能被捕获。这是 v1.8.34 修复的核心: 之前只解析最后一行, 会漏掉非末尾的 429 信号 -> 页面始终空闲。
func TestReadTranscriptCapturesNonLastErrorLine(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "session.jsonl")
	now := time.Now().UTC()
	// 429 行后跟 system 行(而非 assistant 完成行) -> 错误信号应保留(时间戳用当前, 避免 age 守卫)
	content := `{"type":"user","timestamp":"` + now.Add(-6*time.Second).Format(time.RFC3339Nano) + `"}
{"type":"assistant","timestamp":"` + now.Add(-5*time.Second).Format(time.RFC3339Nano) + `","apiErrorStatus":429,"error":"rate_limit","isApiErrorMessage":true}
{"type":"system","timestamp":"` + now.Add(-4*time.Second).Format(time.RFC3339Nano) + `","subtype":"turn_duration"}
`
	if err := os.WriteFile(f, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	info := readTranscript(f, "claude", map[string]*agentMonitor{})
	if info == nil {
		t.Fatal("readTranscript 返回 nil")
	}
	if info.lastApiStatus != "429" {
		t.Fatalf("期望捕获 429(非末尾行), 实际 lastApiStatus=%q", info.lastApiStatus)
	}
	// 末行是 system, 但错误优先 -> error
	st, reason, code := transcriptState(*info, 60*1e9)
	if st != "error" {
		t.Fatalf("期望 error(错误优先于末行 type), 实际 %q", st)
	}
	if reason != store.ReasonLLMError {
		t.Fatalf("期望 reason=llm_error, 实际 %q", reason)
	}
	if code != "429" {
		t.Fatalf("期望 errorCode=429, 实际 %q", code)
	}
}

// TestReadTranscriptErrorClearedOnRecovery 验证: 429 后若出现正常 assistant 完成行(无错误信号),
// 错误标记被清除(表示已恢复)。避免 429 后一直卡在 error。
func TestReadTranscriptErrorClearedOnRecovery(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "session.jsonl")
	now := time.Now().UTC()
	content := `{"type":"assistant","timestamp":"` + now.Add(-5*time.Second).Format(time.RFC3339Nano) + `","apiErrorStatus":429,"error":"rate_limit","isApiErrorMessage":true}
{"type":"assistant","timestamp":"` + now.Format(time.RFC3339Nano) + `","stop_reason":"end_turn"}
`
	if err := os.WriteFile(f, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	info := readTranscript(f, "claude", map[string]*agentMonitor{})
	if info == nil {
		t.Fatal("readTranscript 返回 nil")
	}
	if info.lastApiStatus != "" {
		t.Fatalf("期望恢复后 lastApiStatus 清空, 实际 %q", info.lastApiStatus)
	}
	st, _, _ := transcriptState(*info, 60*1e9)
	if st != "thinking" {
		t.Fatalf("期望恢复后 thinking, 实际 %q", st)
	}
}

// TestTranscriptStateErrorFreshAfterUserResend 回归(2026-08-26): claude 刚报 520 error(<idleNs),
// 用户随后重发消息(user 行)。520 是近期发生的(error 行自身 age 小) -> 仍显示 error。
// 这是\"控制台刚报 520/在等授权, 页面不该显示空闲\"的正确表现。
func TestTranscriptStateErrorFreshAfterUserResend(t *testing.T) {
	idleNs := int64(60 * 1e9)
	now := time.Now().UnixNano()
	fresh := func() int64 { return now - 5*1e9 } // 5s 前(近期 error)
	tInfo := transcriptInfo{
		lastType:      "user",
		lastApiStatus: "520",
		lastApiErr:    "server_error",
		lastApiErrMsg: true,
		lastTs:        fresh(),
		lastApiTs:     fresh(),
	}
	st, reason, code := transcriptState(tInfo, idleNs)
	if st != "error" {
		t.Fatalf("期望 近期 520 error 在 user 重发后仍显示 error, 实际 %q", st)
	}
	if reason != store.ReasonLLMError || code != "520" {
		t.Fatalf("期望 reason=llm_error code=520, 实际 %q %q", reason, code)
	}
}

// TestTranscriptStateErrorStaleHiddenAfterUserResend 回归(2026-08-26, 用户实际反馈):
// claude 在用户重发消息之前就报了 520(陈旧, error 行 age 远超 idleNs), 之后用户重发、claude 卡在
// \"等待用户确认输入\"(但 transcript 无 tool_use/permission 结构化信号)。此时页面不该显示陈旧 520,
// 而应退化到 user -> thinking_user(处理中/等待输入), 否则会误显示\"错误\"。
func TestTranscriptStateErrorStaleHiddenAfterUserResend(t *testing.T) {
	idleNs := int64(60 * 1e9)
	now := time.Now().UnixNano()
	old := func() int64 { return now - 600*1e9 } // 600s 前(陈旧 error)
	fresh := func() int64 { return now - 5*1e9 } // 近期 user 活动
	tInfo := transcriptInfo{
		lastType:      "user",
		lastApiStatus: "520", // 陈旧 error, 发生在 user 重发之前
		lastApiErr:    "server_error",
		lastApiErrMsg: true,
		lastTs:        fresh(), // 末内容行(user)近期
		lastApiTs:     old(),   // 但 error 行本身陈旧
	}
	st, _, _ := transcriptState(tInfo, idleNs)
	if st != "thinking" {
		t.Fatalf("期望 陈旧 520 被隐藏, 退化到 user->thinking(等待输入), 实际 %q", st)
	}
}

// TestClaudeDeriveTranscriptStateDoesNotGuessFromSleep 验证末行 user 时不根据模糊的进程睡眠
// 信号猜测等待输入。Claude 在 LLM 推理期间同样可能阻塞于 epoll。
func TestClaudeDeriveTranscriptStateDoesNotGuessFromSleep(t *testing.T) {
	idleNs := int64(60 * 1e9)
	now := time.Now().UnixNano()
	fresh := func() int64 { return now - 5*1e9 }

	// 末行 user + ptsBlocked 仍应是 thinking。
	txUser := &transcriptInfo{lastType: "user", lastTs: fresh()}
	r := claudeSource{}.DeriveTranscriptState(txUser, idleNs, true)
	if r.State != store.StateThinking || r.NeedsInput {
		t.Fatalf("期望 thinking/非 needs_input, 实际 state=%q reason=%q needs=%v", r.State, r.Reason, r.NeedsInput)
	}

	// 场景2: 末行 user 但无 ptsBlocked -> 不提升(退化为 thinking)
	txUserNoBlock := &transcriptInfo{lastType: "user", lastTs: fresh()}
	r2 := claudeSource{}.DeriveTranscriptState(txUserNoBlock, idleNs, false)
	if r2.State == store.StateWaiting {
		t.Fatalf("场景2 末行 user 但无 ptsBlocked 不应提升为 waiting, 实际 %q", r2.State)
	}

	// 场景3: idle(末行 system) + ptsBlocked -> 不提升(真正空闲的 claude 不因阻塞误判等待)
	txIdle := &transcriptInfo{lastType: "system", lastTs: fresh()}
	r3 := claudeSource{}.DeriveTranscriptState(txIdle, idleNs, true)
	if r3.State == store.StateWaiting {
		t.Fatalf("场景3 idle+ptsBlocked 不应提升为 waiting, 实际 %q", r3.State)
	}

	// error 态仍保持 error。
	txErr := &transcriptInfo{lastType: "user", lastApiStatus: "520", lastApiErr: "server_error",
		lastApiErrMsg: true, lastTs: fresh(), lastApiTs: fresh()}
	r4 := claudeSource{}.DeriveTranscriptState(txErr, idleNs, true)
	if r4.State != store.StateError {
		t.Fatalf("场景4 error 应优先于 waiting, 实际 %q", r4.State)
	}
}

// TestParseClaudeLiveState 校验 claude 实时会话状态的映射。
// 用例中的 status 取值必须来自 claude 的真实联合类型 ["busy","shell","idle","waiting"] ——
// 历史 bug: 曾用臆造的 "working"/"running" 写用例, 测试全绿但线上最常见的 "busy" 走到
// default 分支被静默丢弃, 推理中的 claude 拿不到权威状态。
func TestParseClaudeLiveState(t *testing.T) {
	permission := parseClaudeLiveState(
		[]byte(`{"pid":123,"status":"waiting","waitingFor":"permission prompt"}`), 123)
	if permission.State != store.StateWaiting || permission.Reason != store.ReasonAwaitingApproval || !permission.NeedsInput {
		t.Fatalf("permission prompt 期望 waiting/awaiting_approval, 实际 %+v", permission)
	}

	// claude 实测写入的 waitingFor 值
	sandbox := parseClaudeLiveState(
		[]byte(`{"pid":123,"status":"waiting","waitingFor":"sandbox request"}`), 123)
	if sandbox.State != store.StateWaiting || sandbox.Reason != store.ReasonAwaitingApproval || !sandbox.NeedsInput {
		t.Fatalf("sandbox request 期望 waiting/awaiting_approval, 实际 %+v", sandbox)
	}
	input := parseClaudeLiveState(
		[]byte(`{"pid":123,"status":"waiting","waitingFor":"input needed"}`), 123)
	if input.State != store.StateWaiting || input.Reason != store.ReasonAwaitingInput || !input.NeedsInput {
		t.Fatalf("input needed 期望 waiting/awaiting_input, 实际 %+v", input)
	}

	// busy 是推理/子代理运行中的最常见活跃态, 必须映射为 thinking 且不产生 needs_input。
	busy := parseClaudeLiveState([]byte(`{"pid":123,"status":"busy"}`), 123)
	if busy.State != store.StateThinking || busy.Reason != store.ReasonThinkingLLM || busy.NeedsInput {
		t.Fatalf("busy 期望 thinking/thinking_llm 且非 needs_input, 实际 %+v", busy)
	}
	idle := parseClaudeLiveState([]byte(`{"pid":123,"status":"idle"}`), 123)
	if idle.State != store.StateIdle || idle.Reason != store.ReasonIdle || idle.NeedsInput || idle.Confidence != "high" {
		t.Fatalf("idle 期望 idle, 实际 %+v", idle)
	}
	// shell: 用户切入 shell 交互, 活跃但非推理; 不得产生 needs_input(否则误发等待告警)。
	shell := parseClaudeLiveState([]byte(`{"pid":123,"status":"shell"}`), 123)
	if shell.State != store.StateRunning || shell.NeedsInput {
		t.Fatalf("shell 期望 running 且非 needs_input, 实际 %+v", shell)
	}

	for name, data := range map[string]string{
		"pid mismatch": `{"pid":456,"status":"waiting","waitingFor":"permission prompt"}`,
		"invalid":      `{`,
		// claude 未来新增的未知状态: 退回 transcript 推导, 不臆造映射
		"unknown status": `{"pid":123,"status":"hibernating"}`,
	} {
		if got := parseClaudeLiveState([]byte(data), 123); got.State != "" {
			t.Fatalf("%s 不应覆盖 transcript 状态, 实际 %+v", name, got)
		}
	}
}
