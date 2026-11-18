package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/prompt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// 本文件钉住 11-128 修复阶梯的行为契约：
//
//	L2 拆句   —— 上游按「连续原文」匹配时把长段切成多条 system
//	L3 搬家   —— 连单句都拦时把 system 原文搬进首条 user
//	L4 降级   —— 都不行才换中性提示词，且**只对该段生效**
//
// 假上游刻意模拟「按连续原文匹配」：它解析出站 body 的 messages，只有当**同一条
// system 消息里**同时出现整段原文时才回 400。拆句后跨了消息边界 → 放行；搬家后
// 原文落在 user → 放行。这样断言的是修复机制本身，而不是某段具体文案。

// fingerprintSentences 模拟被拦的「长段指纹」：四句连续原文，跨句边界后即失配。
const fingerprintSentences = "Tokens prefixed with @ are paths the user explicitly referenced. " +
	"Relative paths resolve from the workspace root; absolute paths point to files or directories on the host. " +
	"A trailing slash marks a directory, so list it when its contents matter. " +
	"Anything else is a file: open it with the read tool when its contents are needed."

// sentinelSentence 单句级指纹：用于逼出 L3（拆句也救不回来的场景）。
const sentinelSentence = "The @\"...\" form only quotes a path that contains spaces."

type capturedCall struct {
	systemMsgs []string
	userMsgs   []string
}

// ladderUpstream 构造一个「按连续原文匹配」的假上游，并记录每次出站体的消息分布。
func ladderUpstream(t *testing.T, blocked func(systemMsgs []string, joined string) bool) (*upstream.Client, *[][]byte, *[]capturedCall) {
	t.Helper()
	var mu sync.Mutex
	bodies := [][]byte{}
	calls := []capturedCall{}
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			systems, users, joined := messageRoles(raw)
			mu.Lock()
			bodies = append(bodies, raw)
			calls = append(calls, capturedCall{systemMsgs: systems, userMsgs: users})
			mu.Unlock()
			if blocked(systems, joined) {
				return &http.Response{
					StatusCode: 400,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"code":11-128,"msg":"Illegal API invocation from an unapproved channel"}`)),
				}, nil
			}
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN: "https://fake.example",
	}
	return up, &bodies, &calls
}

// messageRoles 从出站请求体里取出 system 消息列表与 user 消息列表（都取纯文本）。
func messageRoles(body []byte) (systems, users []string, joined string) {
	var obj struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, nil, ""
	}
	var all []string
	for _, m := range obj.Messages {
		var s string
		if err := json.Unmarshal(m.Content, &s); err != nil {
			continue
		}
		switch m.Role {
		case "system":
			systems = append(systems, s)
			all = append(all, s)
		case "user":
			users = append(users, s)
		}
	}
	return systems, users, strings.Join(all, "\n")
}

// systemContainsWholeRun 某一条 system 里出现整段连续原文（模拟上游的连续匹配）。
func systemContainsWholeRun(systems []string) bool {
	for _, s := range systems {
		if strings.Contains(s, fingerprintSentences) {
			return true
		}
	}
	return false
}

// systemContainsSentinel 某一条 system 里出现单句指纹（拆句也救不回来）。
func systemContainsSentinel(systems []string) bool {
	for _, s := range systems {
		if strings.Contains(s, sentinelSentence) {
			return true
		}
	}
	return false
}

// repairEnabledConfig 阶梯全开 + 记忆落临时目录（每个用例一个独立文件）。
func repairEnabledConfig(t *testing.T) repairConfig {
	t.Helper()
	return repairConfig{
		Enabled:           true,
		EnsureSystemFirst: true,
		SplitSystem:       true,
		MoveSystemToUser:  true,
		Budget:            3,
		DegradeTTL:        30 * time.Minute,
		FallbackTTL:       30 * time.Minute,
		FallbackThreshold: 3,
		MemoryEnabled:     true,
		MemoryPath:        filepath.Join(t.TempDir(), "repair-memory.json"),
		PreapplyAfter:     2,
	}
}

func ladderRequest(t *testing.T, h *Handler, sys string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"model":  "glm-5.2",
		"stream": true,
		"messages": []any{
			map[string]any{"role": "system", "content": sys},
			map[string]any{"role": "user", "content": "hi"},
		},
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(string(body))))
	return rec
}

// systemDigestOf 复算 handler 侧用的段指纹（system 文本 hash 前 8 位）。
func systemDigestOf(sys string) string {
	body, _ := json.Marshal(map[string]any{
		"messages": []any{map[string]any{"role": "system", "content": sys}},
	})
	return prompt.SystemDigest(body)
}

// TestRepairLadderSplitRescues L2：整段连续原文被拦 → 拆句后放行，且不进入降级。
func TestRepairLadderSplitRescues(t *testing.T) {
	up, bodies, calls := ladderUpstream(t, func(systems []string, _ string) bool {
		return systemContainsWholeRun(systems)
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "passthrough", Repair: repairEnabledConfig(t)})

	rec := ladderRequest(t, h, fingerprintSentences)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(*bodies) != 2 {
		t.Fatalf("want 2 upstream calls (400 + split retry), got %d", len(*bodies))
	}
	// 第二次出站：多条 system、且没有任何一条包含整段原文。
	second := (*calls)[1]
	if len(second.systemMsgs) < 2 {
		t.Fatalf("拆句后应有 >=2 条 system，got %d: %v", len(second.systemMsgs), second.systemMsgs)
	}
	if systemContainsWholeRun(second.systemMsgs) {
		t.Fatal("拆句后仍有一条 system 含整段原文")
	}
	// 拆句不丢字符：拼起来（忽略段间空白）应与原文一致。
	norm := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	if got := norm(strings.Join(second.systemMsgs, " ")); got != norm(fingerprintSentences) {
		t.Fatalf("拆句改动了文本:\n got=%q\nwant=%q", got, norm(fingerprintSentences))
	}
	if active, _ := h.DegradeSnapshot(); active {
		t.Fatal("L2 救回后不该进入降级")
	}
}

// TestRepairLadderMoveRescues L3：连单句都拦（拆句无效）→ 搬家到 user 后放行。
func TestRepairLadderMoveRescues(t *testing.T) {
	sys := fingerprintSentences + " " + sentinelSentence
	up, bodies, calls := ladderUpstream(t, func(systems []string, _ string) bool {
		// 只要 system 里出现单句指纹就拦：L2 拆句后仍在某条 system 里 → 继续 400。
		return systemContainsSentinel(systems)
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "passthrough", Repair: repairEnabledConfig(t)})

	rec := ladderRequest(t, h, sys)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(*bodies) != 3 {
		t.Fatalf("want 3 upstream calls (400 + split + move), got %d", len(*bodies))
	}
	third := (*calls)[2]
	if systemContainsSentinel(third.systemMsgs) {
		t.Fatal("搬家后 system 里不该再有指纹原文")
	}
	// 原文搬进首条 user，一字不少。
	joinedUser := strings.Join(third.userMsgs, "\n")
	if !strings.Contains(joinedUser, sentinelSentence) {
		t.Fatalf("首条 user 应包含 system 原文: %v", third.userMsgs)
	}
	if len(third.systemMsgs) != 1 || third.systemMsgs[0] != prompt.Degraded {
		t.Fatalf("搬家后 system 应为中性提示词，got %v", third.systemMsgs)
	}
}

// TestRepairLadderExhaustedFallsBackToFirewall 三级全失败 → 回内容防火墙错误，不罚号。
func TestRepairLadderExhaustedFallsBackToFirewall(t *testing.T) {
	up, bodies, _ := ladderUpstream(t, func([]string, string) bool { return true })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "passthrough", Repair: repairEnabledConfig(t)})

	rec := ladderRequest(t, h, fingerprintSentences)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "content_blocked") {
		t.Fatalf("应回 content_blocked 错误: %s", rec.Body.String())
	}
	// 1 次原始 + L2 + L3 + L4 = 4 次上游调用（budget 正好用满）。
	if len(*bodies) != 4 {
		t.Fatalf("want 4 upstream calls, got %d", len(*bodies))
	}
	// 最后一级触发的降级必须落在 (realm, seg) 桶上，而不是全局。
	if !h.degrade.Active("cn", systemDigestOf(fingerprintSentences)) && !h.degrade.ActiveAny("cn") {
		t.Fatal("L4 失败后应有作用域降级记录")
	}
}

// TestRepairMemoryPreapply 记忆预置：同一段连续命中后，后续请求直接从该级起步。
func TestRepairMemoryPreapply(t *testing.T) {
	up, bodies, _ := ladderUpstream(t, func(systems []string, _ string) bool {
		return systemContainsWholeRun(systems)
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	cfg := repairEnabledConfig(t)
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "passthrough", Repair: cfg})

	// 第 1、2 次：未达预置阈值（PreapplyAfter=2）→ 各 2 次上游调用。
	if rec := ladderRequest(t, h, fingerprintSentences); rec.Code != 200 {
		t.Fatalf("req1 code=%d", rec.Code)
	}
	if len(*bodies) != 2 {
		t.Fatalf("req1 calls=%d want 2", len(*bodies))
	}
	if rec := ladderRequest(t, h, fingerprintSentences); rec.Code != 200 {
		t.Fatalf("req2 code=%d", rec.Code)
	}
	if len(*bodies) != 4 {
		t.Fatalf("req2 累计 calls=%d want 4", len(*bodies))
	}
	// 第 3 次：记忆 Hits=2 → 直接发 L2 形态 → 只需 1 次上游调用。
	if rec := ladderRequest(t, h, fingerprintSentences); rec.Code != 200 {
		t.Fatalf("req3 code=%d", rec.Code)
	}
	if len(*bodies) != 5 {
		t.Fatalf("req3 累计 calls=%d want 5（记忆预置应省掉一次 400 往返）", len(*bodies))
	}
	// 记忆必须落盘（重启后仍有效）。
	mem := h.RepairMemory()
	if mem == nil {
		t.Fatal("记忆表应启用")
	}
	if got := mem.Snapshot(); len(got) != 1 || got[0].Level != int(repairSplit) || got[0].Hits != 3 {
		t.Fatalf("记忆内容不符: %+v", got)
	}
}

// TestRepairDegradeScopeIsolation 一段降级不影响别的段（旧实现是进程级单例，会污染全站）。
func TestRepairDegradeScopeIsolation(t *testing.T) {
	// 假上游一律拦截：阶梯三级全失败 → 该段进入作用域降级。
	up, bodies, calls := ladderUpstream(t, func([]string, string) bool { return true })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "passthrough", Repair: repairEnabledConfig(t)})

	bad := fingerprintSentences + " " + sentinelSentence
	if rec := ladderRequest(t, h, bad); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad 段应失败 code=%d", rec.Code)
	}
	if !h.degrade.Active("cn", systemDigestOf(bad)) {
		t.Fatal("bad 段应产生作用域降级（key = realm + 段指纹）")
	}
	other := "another client system prompt that is long enough."
	if h.degrade.Active("cn", systemDigestOf(other)) {
		t.Fatal("别的段不该被降级（这正是要修掉的污染）")
	}

	// 别的段的请求首次出站体必须原样：不能被换成中性提示词（无跨段污染）。
	before := len(*bodies)
	_ = ladderRequest(t, h, other)
	if len(*bodies) == before {
		t.Fatal("无关段请求应至少发一次上游调用")
	}
	firstOfOther := (*calls)[before]
	if len(firstOfOther.systemMsgs) != 1 || firstOfOther.systemMsgs[0] != other {
		t.Fatalf("无关段的首次出站 system 被污染: %v", firstOfOther.systemMsgs)
	}
}

// TestRepairDisabledKeepsLegacyBehavior repair.enabled=false 时必须逐字保留旧行为：
// 一次 400 → 全局降级 → 中性提示词重试一次（2 次上游调用）。
func TestRepairDisabledKeepsLegacyBehavior(t *testing.T) {
	up, bodies, calls := ladderUpstream(t, func(systems []string, _ string) bool {
		return systemContainsSentinel(systems)
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "passthrough"}) // Repair 零值 = 禁用

	rec := ladderRequest(t, h, "x "+sentinelSentence)
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	if len(*bodies) != 2 {
		t.Fatalf("calls=%d want 2（一次降级重试）", len(*bodies))
	}
	if last := (*calls)[1].systemMsgs; len(last) != 1 || last[0] != prompt.Degraded {
		t.Fatalf("降级重试的 system 应为 Degraded，got %v", last)
	}
	if !h.degrade.ActiveAny("") {
		t.Fatal("旧行为触发的是全局降级")
	}
}

// TestEnsureSystemFirstPreventsChannelReject 首条不是 system 时补一条，
// 否则每次请求都必然 11-128（渠道校验文案与内容拦截同一句）。
func TestEnsureSystemFirstPreventsChannelReject(t *testing.T) {
	up, _, calls := ladderUpstream(t, func(systems []string, _ string) bool {
		// 模拟上游渠道校验：首条不是 system 就拒。
		return len(systems) == 0
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	cfg := repairEnabledConfig(t)
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "passthrough", Repair: cfg})

	body, _ := json.Marshal(map[string]any{
		"model":  "glm-5.2",
		"stream": true,
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
		},
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(string(body))))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if len((*calls)[0].systemMsgs) != 1 {
		t.Fatalf("应在最前补一条 system，got %v", (*calls)[0].systemMsgs)
	}
}
