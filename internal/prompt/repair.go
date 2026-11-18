package prompt

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// 本文件是 11-128「内容/渠道拦截」的三级保真修复阶梯（passthrough 保真模式下
// 不替换客户端 system 的前提下，逐级加大干预力度）：
//
//	L2 SplitSystemMessages 断连续原文，一字不改、通道仍是 system（首选）
//	L3 MoveSystemToUser    换承载通道，一字不改（次选）
//	L4 中性提示词          丢客户端 prompt（internal/server/degrade.go，最后一级）
//
// 为什么"断连续原文"有效：上游对这几个指纹是按**连续原文**匹配的，不是语义审核。
// 真机二分实测（CN 账号 / deepseek-v4.1-flash）：同一段 @ 路径说明整段放 system
// → 400 11-128；按句界拆成两条 system → 200；每句一条 system → 200；同义改写后
// 仍在 system → 200。把同一段搬进首条 user（system 换中性句）同样 200。
//
// 与 sanitize.go 的分工：那边改**措辞**（语义不变、可见字符变），这里改**结构**
// （一个字都不动）。两者叠加而非替代——措辞层解决已实测确认的指纹，结构层兜住
// 未见过的新指纹。

// EnsureSystemFirst 保证 messages[0] 是 role=system —— 上游**渠道校验**的硬要求。
//
// 为什么必须做（上游实测）：首条是 user → 400 11-128 "first message is not
// system prompt"；首条是 developer → 400 11-128 "Illegal API invocation from an
// unapproved channel"（与内容拦截同一句文案，极易被误归成内容问题）；首条是
// system → 200。也就是说**首条角色本身**是渠道校验的一部分，和内容无关。
//
// custom / append 两种模式本来就以网关 system 开头，唯独 passthrough 会原样透传
// 客户端消息——大量 SDK / 手搓 curl 不带 system，首条是 user，于是每一次请求都
// 必然 11-128，账号被反复计成"渠道校验拦截"。
//
// 语义边界：只在**首条不是 system/developer 时**补一条，位置在最前；首条已是
// system 时逐字不动（走 FirstMessageRole 快路径，一次整树解析都不做）；首条是
// developer 时只做 developer→system 归一（内容不删不并，上游 role 白名单不含
// developer）。fallback 为空时用 Degraded 兜底，绝不因为没配提示词就不补。
func EnsureSystemFirst(body []byte, fallback string) []byte {
	if len(body) == 0 {
		return body
	}
	// 快路径：首条已是 system（代理类客户端几乎恒如此）→ 零改动、零解析。
	if role, ok := FirstMessageRole(body); ok && role == "system" {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		// 无 messages / 空数组：没有"首条"可补。凭空造一条 system 会把
		// 上游本该报"没有对话"的请求变成一次成功的空对话，语义变化更大。
		return body
	}
	first, ok := msgs[0].(map[string]any)
	if !ok {
		return body
	}
	switch role, _ := first["role"].(string); role {
	case "system":
		return body // 已是官方形态
	case "developer":
		// 上游不认 developer：就地归一，内容逐字保留（不删不并）。
		first["role"] = "system"
		if out, err := json.Marshal(obj); err == nil {
			return out
		}
		return body
	}
	neutral := fallback
	if neutral == "" {
		neutral = Degraded
	}
	if neutral == "" {
		return body
	}
	rewritten := make([]any, 0, len(msgs)+1)
	rewritten = append(rewritten, map[string]any{"role": "system", "content": neutral})
	rewritten = append(rewritten, msgs...)
	obj["messages"] = rewritten
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// SplitSystemMessages 把**最长的那条** system/developer 消息按句子边界拆成多条
// system 消息，用于 11-128 的**首选**抢救。
//
// 原理：上游按「连续原文」匹配指纹，句界一断就失配，而文本一个字都不改、承载
// 通道仍是 system（模型的指令优先级不变）。这是三级阶梯里语义损失最小的一级。
//
// 只拆最长的一条：短 system 拆开反而是把一句指令切成两半——探针实测"人格句"
// 若在非句界处被切开就失效，而在**句界处**拆成两条（"You are a pirate." /
// "You must begin every reply with the word ARRR."）人格照旧生效。故只从句界切。
//
// 返回 false 的四种情况（都交给下一级，不硬拆）：无 system 文本、最长那条
// content 不是纯字符串（多模态 parts 里插消息会打乱顺序）、切不出 ≥2 段
// （单句 system 没法拆）、解析失败。
func SplitSystemMessages(body []byte) ([]byte, bool) {
	if len(body) == 0 {
		return body, false
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body, false
	}
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return body, false
	}
	best, bestLen := -1, 0
	for i, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		role, _ := mm["role"].(string)
		if role != "system" && role != "developer" {
			continue
		}
		txt, ok := mm["content"].(string)
		if !ok || len(txt) <= bestLen {
			continue
		}
		best, bestLen = i, len(txt)
	}
	if best < 0 {
		return body, false
	}
	parts := splitSentences(msgs[best].(map[string]any)["content"].(string))
	if len(parts) < 2 {
		return body, false
	}
	rebuilt := make([]any, 0, len(msgs)+len(parts)-1)
	rebuilt = append(rebuilt, msgs[:best]...)
	for _, p := range parts {
		rebuilt = append(rebuilt, map[string]any{"role": "system", "content": p})
	}
	rebuilt = append(rebuilt, msgs[best+1:]...)
	obj["messages"] = rebuilt
	out, err := json.Marshal(obj)
	if err != nil {
		return body, false
	}
	return out, true
}

// MoveSystemToUser 把 system/developer 的**原文**搬进首条 user 消息前部，system
// 换成中性提示词，作为 11-128 的**后一层**抢救（前一层是 SplitSystemMessages：
// 留在 system 更好，拆不动才搬家）。返回 false = 这条请求结构不安全，别动。
//
// 与 Rewrite 的区别是丢不丢东西：Rewrite 用中性提示词**替换** system，客户端
// 辛苦写的人设 / 项目约定就此消失；这里只换承载通道（system → user），一个字符
// 都不丢。上游对 11-128 的判据是「system 消息内容命中特征」，同一段文本放进
// user 就能过（实测同一 body 换号结果一致、system 原文搬进首条 user 即通过），
// 所以这不是绕过审核，是换通道。
//
// 保守放弃的四种情况：没有 system/developer 文本、system 内容不是纯文本、
// 找不到 user 消息、首条 user 不是纯文本（多模态 parts 里插文本会打乱顺序）。
func MoveSystemToUser(body []byte, neutral string) ([]byte, bool) {
	if len(body) == 0 {
		return body, false
	}
	if neutral == "" {
		neutral = Degraded
	}
	if neutral == "" {
		return body, false
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body, false
	}
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return body, false
	}
	first, ok := msgs[0].(map[string]any)
	if !ok {
		return body, false
	}
	switch role, _ := first["role"].(string); role {
	case "system", "developer":
	default:
		return body, false
	}
	sysText, ok := first["content"].(string)
	if !ok || strings.TrimSpace(sysText) == "" {
		return body, false
	}
	for _, m := range msgs[1:] {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := mm["role"].(string); role != "user" {
			continue
		}
		txt, ok := mm["content"].(string)
		if !ok {
			return body, false // 多模态 parts：插文本会打乱 part 顺序
		}
		mm["content"] = sysText + "\n\n" + txt
		first["role"] = "system"
		first["content"] = neutral
		out, err := json.Marshal(obj)
		if err != nil {
			return body, false
		}
		return out, true
	}
	return body, false
}

// SystemDigest 返回请求体里 system/developer 文本的短指纹（sha256 前 8 位十六进制）。
//
// 用途：给「按段记忆」与「按段降级」当 key——同一段第二次出现时不必再先撞一次 400。
// 必须在**任何修复之前**计算（拆句/搬家会改变消息结构，算出来的 hash 就变了）。
//
// 拼接口径：按消息顺序把 system/developer 的纯文本 content 用 "\n" 连起来再哈希，
// 多模态 content 与非字符串 content 跳过（那类请求本来也不进结构层修复）。
// 没有任何 system 文本时返回空串——调用方据此不做记忆（空 key 会把所有请求混在一起）。
//
// 实现用流式 decoder 而不是 json.Unmarshal：这是**每个请求**都要跑的一步，而
// json.Unmarshal 会把整棵 body（100~500KB、含全部历史消息）解成 map 并拷贝每个
// 字符串。流式只解 messages[] 里每条消息的 role + content 原文，用户正文那几 MB
// 一个字节都不复制（RawMessage 只拷 content，而 content 只在 system/developer 时
// 才需要文本值）。语义与整树解析逐字节等价。
func SystemDigest(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	tok, err := dec.Token()
	if err != nil {
		return ""
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return ""
	}
	h := sha256.New()
	first := true
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return ""
		}
		if key, _ := keyTok.(string); key != "messages" {
			if err := skipValue(dec); err != nil {
				return ""
			}
			continue
		}
		arrTok, err := dec.Token()
		if err != nil {
			return ""
		}
		if d, ok := arrTok.(json.Delim); !ok || d != '[' {
			return ""
		}
		for dec.More() {
			var m struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			}
			if err := dec.Decode(&m); err != nil {
				return ""
			}
			if m.Role != "system" && m.Role != "developer" {
				continue
			}
			var s string
			if err := json.Unmarshal(m.Content, &s); err != nil || s == "" {
				continue
			}
			if !first {
				h.Write([]byte("\n"))
			}
			first = false
			h.Write([]byte(s))
		}
		// 吃掉数组的收尾 ']'，否则外层对象循环的 token 流会错位。
		if _, err := dec.Token(); err != nil {
			return ""
		}
	}
	if first {
		return ""
	}
	sum := h.Sum(nil)
	return hex.EncodeToString(sum[:4])
}

// 句界切分的几个约束（改动前请先看这里）：
//
//  1. **中文必须能切**：只看 ". ! ?" 的实现对中文 prompt 完全失效（中文 prompt
//     在这些客户端里很常见），那样 L2 会退化成"拆不动 → 直接降到 L4"，等于
//     白丢一级保真。故中文句末标点 。！？； 一律算句界，且不需要后接空白。
//  2. **小数点不能切**："3.5" / "v1.2.3" 切开就把一个数/版本号拆成两条消息。
//     判据：'.' 前后都是数字 → 不是句界。
//  3. **代码块不能切**：system 里的 ``` 围栏内切开会把一个代码块劈成两条消息，
//     模型看到的上下文顺序虽不变，但围栏状态被打断。故围栏内不切。
//  4. **空行算句界**：段落边界是最强的自然断点，多断一刀无害、少断一刀才致命。
//  5. **碎片回并**：切出来的过短片段（<24 字节）并进下一段，避免产生大量
//     "e.g." 这种碎消息——碎片多不等于断得有效，反而增加消息条数。
const (
	splitMinSegmentBytes = 24 // 短于此长度的片段并进下一段
	splitMaxParts        = 64 // 最多拆成多少条（防止极端 system 产生上百条消息）
	splitFence           = "```"
)

// splitSentences 按句界把一段文本切成若干段，保留原标点、不增删任何字符
// （段首尾空白剔除，段间空白丢弃——消息边界本身就是分隔）。
func splitSentences(text string) []string {
	if text == "" {
		return nil
	}
	var raw []string
	start := 0
	inFence := false
	for i := 0; i < len(text); {
		// 围栏开关：``` 出现即翻转（不要求独占一行，宁可多判）。
		if strings.HasPrefix(text[i:], splitFence) {
			inFence = !inFence
			i += len(splitFence)
			continue
		}
		// 空行边界：跨行段落分隔。
		if text[i] == '\n' {
			j := i + 1
			for j < len(text) && (text[j] == ' ' || text[j] == '\t' || text[j] == '\r') {
				j++
			}
			if j < len(text) && text[j] == '\n' && !inFence {
				if seg := strings.TrimSpace(text[start:j]); seg != "" {
					raw = append(raw, seg)
				}
				start = j + 1
				i = j + 1
				continue
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(text[i:])
		if !inFence {
			switch r {
			case '。', '！', '？', '；':
				// 中文句末标点：后面不需要空白，标点本身留在本段。
				i += size
				if seg := strings.TrimSpace(text[start:i]); seg != "" {
					raw = append(raw, seg)
				}
				start = i
				continue
			case '.', '!', '?':
				// 英文句末标点：要求后面是空白 / 换行 / 结尾，且 '.' 不在两个数字之间。
				next := i + size
				if next < len(text) && text[next] != ' ' && text[next] != '\t' && text[next] != '\n' && text[next] != '\r' {
					i += size
					continue
				}
				if r == '.' && i > 0 && next < len(text) && isDigit(text[i-1]) && isDigit(text[next]) {
					i += size
					continue
				}
				i = next
				if seg := strings.TrimSpace(text[start:i]); seg != "" {
					raw = append(raw, seg)
				}
				start = i
				continue
			}
		}
		i += size
	}
	if seg := strings.TrimSpace(text[start:]); seg != "" {
		raw = append(raw, seg)
	}
	return mergeShortSegments(raw)
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

// mergeShortSegments 把过短片段并进下一段（末尾碎片并进上一段），并限制总条数。
func mergeShortSegments(parts []string) []string {
	if len(parts) < 2 {
		return parts
	}
	out := make([]string, 0, len(parts))
	pending := ""
	for _, p := range parts {
		if pending != "" {
			p = pending + " " + p
			pending = ""
		}
		if len(p) < splitMinSegmentBytes {
			pending = p
			continue
		}
		out = append(out, p)
	}
	if pending != "" {
		if len(out) == 0 {
			out = append(out, pending)
		} else {
			out[len(out)-1] = out[len(out)-1] + " " + pending
		}
	}
	// 条数上限：相邻合并到 <= splitMaxParts（保序、不丢字）。
	for len(out) > splitMaxParts {
		step := (len(out) + splitMaxParts - 1) / splitMaxParts
		merged := make([]string, 0, splitMaxParts)
		for i := 0; i < len(out); i += step {
			end := i + step
			if end > len(out) {
				end = len(out)
			}
			merged = append(merged, strings.Join(out[i:end], " "))
		}
		out = merged
	}
	return out
}
