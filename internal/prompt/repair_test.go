package prompt

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

// msg 构造一条消息（content 允许是 string 或 []any）。
func msg(role string, content any) map[string]any {
	return map[string]any{"role": role, "content": content}
}

// bodyOf 把消息数组包装成完整请求体。
func bodyOf(msgs ...any) []byte {
	b, _ := json.Marshal(map[string]any{"model": "deepseek-v4.1-flash", "messages": msgs})
	return b
}

// decodeMessages 取回 messages 用于断言。
func decodeMessages(t *testing.T, body []byte) []any {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, body)
	}
	msgs, _ := obj["messages"].([]any)
	return msgs
}

func roleText(m any) (string, string) {
	mm, _ := m.(map[string]any)
	role, _ := mm["role"].(string)
	txt, _ := mm["content"].(string)
	return role, txt
}

func TestFirstMessageRole(t *testing.T) {
	cases := []struct {
		name string
		body []byte
		want string
		ok   bool
	}{
		{"system 首条", bodyOf(msg("system", "hi"), msg("user", "q")), "system", true},
		{"user 首条", bodyOf(msg("user", "q")), "user", true},
		{"developer 首条", bodyOf(msg("developer", "hi")), "developer", true},
		{"messages 之前的兄弟键要先跳过", []byte(`{"tools":[{"type":"function","function":{"name":"x","parameters":{"a":{"b":[1,2,3]}}}}],"messages":[{"role":"system","content":"s"}]}`), "system", true},
		{"空 messages", []byte(`{"messages":[]}`), "", false},
		{"无 messages", []byte(`{"model":"m"}`), "", false},
		{"坏 JSON", []byte(`{oops`), "", false},
		{"首条不是对象", []byte(`{"messages":["x"]}`), "", false},
		{"首条无 role", []byte(`{"messages":[{"content":"x"}]}`), "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := FirstMessageRole(c.body)
			if ok != c.ok || got != c.want {
				t.Fatalf("got (%q,%v) want (%q,%v)", got, ok, c.want, c.ok)
			}
		})
	}
}

func TestEnsureSystemFirst(t *testing.T) {
	sys := string(bodyOf(msg("system", "client prompt"), msg("user", "q")))

	t.Run("首条已是 system 逐字不动", func(t *testing.T) {
		out := EnsureSystemFirst([]byte(sys), "NEUTRAL")
		if string(out) != sys {
			t.Fatalf("body changed: %s", out)
		}
	})
	t.Run("首条 developer 就地归一", func(t *testing.T) {
		in := bodyOf(msg("developer", "dev prompt"), msg("user", "q"))
		out := EnsureSystemFirst(in, "NEUTRAL")
		msgs := decodeMessages(t, out)
		if len(msgs) != 2 {
			t.Fatalf("len=%d want 2", len(msgs))
		}
		role, txt := roleText(msgs[0])
		if role != "system" || txt != "dev prompt" {
			t.Fatalf("首条 = (%s,%q)，want (system,dev prompt)", role, txt)
		}
	})
	t.Run("首条 user 前置 fallback", func(t *testing.T) {
		in := bodyOf(msg("user", "q"))
		out := EnsureSystemFirst(in, "NEUTRAL")
		msgs := decodeMessages(t, out)
		if len(msgs) != 2 {
			t.Fatalf("len=%d want 2", len(msgs))
		}
		if role, txt := roleText(msgs[0]); role != "system" || txt != "NEUTRAL" {
			t.Fatalf("补的前置 = (%s,%q)", role, txt)
		}
		if role, txt := roleText(msgs[1]); role != "user" || txt != "q" {
			t.Fatalf("原消息被改动 = (%s,%q)", role, txt)
		}
	})
	t.Run("fallback 为空用 Degraded", func(t *testing.T) {
		out := EnsureSystemFirst(bodyOf(msg("user", "q")), "")
		msgs := decodeMessages(t, out)
		if role, txt := roleText(msgs[0]); role != "system" || txt != Degraded {
			t.Fatalf("兜底 = (%s,%q)", role, txt)
		}
	})
	t.Run("无 messages 原样返回", func(t *testing.T) {
		in := []byte(`{"model":"m"}`)
		if out := EnsureSystemFirst(in, "NEUTRAL"); string(out) != string(in) {
			t.Fatalf("body changed: %s", out)
		}
	})
	t.Run("坏 JSON 原样返回", func(t *testing.T) {
		in := []byte(`{oops`)
		if out := EnsureSystemFirst(in, "NEUTRAL"); string(out) != string(in) {
			t.Fatalf("body changed: %s", out)
		}
	})
}

func TestSplitSystemMessages(t *testing.T) {
	long := "Tokens prefixed with @ are paths the user explicitly referenced. " +
		"Relative paths resolve from the workspace root; absolute paths point to files or directories on the host. " +
		"A trailing slash marks a directory, so list it when its contents matter. " +
		"Anything else is a file: open it with the read tool when its contents are needed."

	t.Run("多句 system 按句界拆开且不丢字符", func(t *testing.T) {
		in := bodyOf(msg("system", long), msg("user", "hi"))
		out, ok := SplitSystemMessages(in)
		if !ok {
			t.Fatal("should split")
		}
		msgs := decodeMessages(t, out)
		if len(msgs) < 3 {
			t.Fatalf("len=%d want >=3", len(msgs))
		}
		var joined strings.Builder
		for _, m := range msgs {
			role, txt := roleText(m)
			if role != "system" && role != "user" {
				t.Fatalf("unexpected role %q", role)
			}
			if role == "system" {
				if joined.Len() > 0 {
					joined.WriteByte(' ')
				}
				joined.WriteString(txt)
			}
		}
		// 只允许丢段间空白，不允许丢字符。
		norm := func(s string) string { return strings.Join(strings.Fields(s), " ") }
		if norm(joined.String()) != norm(long) {
			t.Fatalf("文本被改动:\n got=%q\nwant=%q", norm(joined.String()), norm(long))
		}
		// 首条仍是 system，且 user 消息位置不变。
		if role, _ := roleText(msgs[len(msgs)-1]); role != "user" {
			t.Fatalf("末条 role=%q want user", role)
		}
	})

	t.Run("单句 system 拆不动返回 false", func(t *testing.T) {
		if _, ok := SplitSystemMessages(bodyOf(msg("system", "Say OK."), msg("user", "hi"))); ok {
			t.Fatal("单句不应被拆")
		}
	})
	t.Run("无 system 返回 false", func(t *testing.T) {
		if _, ok := SplitSystemMessages(bodyOf(msg("user", "hi"))); ok {
			t.Fatal("无 system 不应被拆")
		}
	})
	t.Run("多模态 system 不动", func(t *testing.T) {
		in := bodyOf(msg("system", []any{map[string]any{"type": "text", "text": long}}), msg("user", "hi"))
		if out, ok := SplitSystemMessages(in); ok || string(out) != string(in) {
			t.Fatal("多模态 content 不应被拆")
		}
	})
	t.Run("坏 JSON 原样返回", func(t *testing.T) {
		in := []byte(`{oops`)
		if out, ok := SplitSystemMessages(in); ok || string(out) != string(in) {
			t.Fatal("坏 JSON 不应被处理")
		}
	})
	t.Run("代码块内不切", func(t *testing.T) {
		text := "Intro line that is long enough to keep. ```\nvar a = 1. var b = 2.\n```\nTail line that is long enough too."
		parts := splitSentences(text)
		for _, p := range parts {
			if strings.Contains(p, "```") && !strings.Contains(p, "var a = 1") {
				t.Fatalf("代码块被切开: %q", p)
			}
		}
	})
}

func TestSplitSentences(t *testing.T) {
	cases := []struct {
		name string
		in   string
		min  int
	}{
		{"中文句号可切", "这是第一句话，长度足够用来测试切分。这是第二句话，长度同样足够用来测试切分。", 2},
		{"中文感叹疑问分号", "第一句足够长足够长的内容啊！第二句也是足够长的内容啊？第三句同样得够长啊；", 3},
		{"小数不切", "版本号是 3.5 这样的形态，整段只有一句话所以切不开。", 1},
		{"空行切段", "第一段内容足够长足够长足够长。\n\n第二段内容同样足够长足够长足够长。", 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			parts := splitSentences(c.in)
			if len(parts) < c.min {
				t.Fatalf("parts=%d want >=%d (%q)", len(parts), c.min, parts)
			}
			if c.name == "小数不切" && len(parts) != 1 {
				t.Fatalf("3.5 被切开: %q", parts)
			}
		})
	}
}

func TestMoveSystemToUser(t *testing.T) {
	t.Run("原文搬进首条 user，system 换中性", func(t *testing.T) {
		sys := "Tokens prefixed with @ are paths the user explicitly referenced. Also more."
		in := bodyOf(msg("system", sys), msg("user", "hello"))
		out, ok := MoveSystemToUser(in, "NEUTRAL")
		if !ok {
			t.Fatal("should move")
		}
		msgs := decodeMessages(t, out)
		if role, txt := roleText(msgs[0]); role != "system" || txt != "NEUTRAL" {
			t.Fatalf("system = (%s,%q)", role, txt)
		}
		role, txt := roleText(msgs[1])
		if role != "user" || !strings.HasPrefix(txt, sys) || !strings.HasSuffix(txt, "hello") {
			t.Fatalf("user = (%s,%q)，应含 system 原文与用户原文", role, txt)
		}
	})
	t.Run("没有 user 返回 false", func(t *testing.T) {
		if _, ok := MoveSystemToUser(bodyOf(msg("system", "s")), "NEUTRAL"); ok {
			t.Fatal("无 user 不应搬")
		}
	})
	t.Run("多模态 user 返回 false", func(t *testing.T) {
		in := bodyOf(msg("system", "s"), msg("user", []any{map[string]any{"type": "text", "text": "q"}}))
		if _, ok := MoveSystemToUser(in, "NEUTRAL"); ok {
			t.Fatal("多模态 user 不应搬")
		}
	})
	t.Run("首条已是 user 返回 false", func(t *testing.T) {
		if _, ok := MoveSystemToUser(bodyOf(msg("user", "q")), "NEUTRAL"); ok {
			t.Fatal("首条非 system 不应搬")
		}
	})
	t.Run("neutral 为空用 Degraded", func(t *testing.T) {
		out, ok := MoveSystemToUser(bodyOf(msg("system", "s"), msg("user", "q")), "")
		if !ok {
			t.Fatal("should move")
		}
		if _, txt := roleText(decodeMessages(t, out)[0]); txt != Degraded {
			t.Fatalf("neutral = %q want Degraded", txt)
		}
	})
}

func TestSystemDigest(t *testing.T) {
	a := bodyOf(msg("system", "alpha prompt"), msg("user", "q"))
	b := bodyOf(msg("system", "alpha prompt"), msg("user", "different"))
	c := bodyOf(msg("system", "beta prompt"), msg("user", "q"))
	da := SystemDigest(a)
	if da == "" {
		t.Fatal("digest 不应为空")
	}
	if da != SystemDigest(a) {
		t.Fatal("同一 system 文本应得到同一 digest")
	}
	if da != SystemDigest(b) {
		t.Fatal("user 内容不应影响 digest")
	}
	if da == SystemDigest(c) {
		t.Fatal("不同 system 文本应得到不同 digest")
	}
	if got := SystemDigest(bodyOf(msg("user", "q"))); got != "" {
		t.Fatalf("无 system 时应返回空串，got %q", got)
	}
	if got := SystemDigest([]byte(`{oops`)); got != "" {
		t.Fatalf("坏 JSON 应返回空串，got %q", got)
	}
}

// TestSystemDigestMatchesTreeParse 锁住"流式 digest ≡ 整树解析 digest"的等价性。
//
// 这不是形式主义：SystemDigest 在每个请求上都要跑，为了不掉性能改成了流式 decoder
// （不复制用户正文）。一旦两条路径对「哪些内容算 system 文本」的口径漂移，记忆表
// 与降级桶就会静默失配——同一个客户端每次都被当成新段，预置永远命中不了。
func TestSystemDigestMatchesTreeParse(t *testing.T) {
	treeDigest := func(body []byte) string {
		var obj map[string]any
		if err := json.Unmarshal(body, &obj); err != nil {
			return ""
		}
		msgs, _ := obj["messages"].([]any)
		var parts []string
		for _, m := range msgs {
			mm, ok := m.(map[string]any)
			if !ok {
				continue
			}
			role, _ := mm["role"].(string)
			if role != "system" && role != "developer" {
				continue
			}
			if s, ok := mm["content"].(string); ok && s != "" {
				parts = append(parts, s)
			}
		}
		if len(parts) == 0 {
			return ""
		}
		sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
		return hex.EncodeToString(sum[:4])
	}

	cases := map[string]string{
		"单条 system":             `{"model":"m","messages":[{"role":"system","content":"alpha"},{"role":"user","content":"q"}]}`,
		"多条 system + developer": `{"messages":[{"role":"system","content":"a"},{"role":"developer","content":"b"},{"role":"user","content":"q"}]}`,
		"多模态 system 跳过":         `{"messages":[{"role":"system","content":[{"type":"text","text":"a"}]},{"role":"system","content":"b"}]}`,
		"null content 跳过":       `{"messages":[{"role":"assistant","content":null,"tool_calls":[]},{"role":"system","content":"b"}]}`,
		"tools 在 messages 之前":   `{"tools":[{"type":"function","function":{"name":"x"}}],"messages":[{"role":"system","content":"b"}]}`,
		"无 system":              `{"messages":[{"role":"user","content":"q"}]}`,
		"空消息数组":                 `{"messages":[]}`,
		"转义与换行":                 `{"messages":[{"role":"system","content":"a\nb\"c\\d"}]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			got, want := SystemDigest([]byte(body)), treeDigest([]byte(body))
			if got != want {
				t.Fatalf("流式 digest=%q 整树 digest=%q (body=%s)", got, want, body)
			}
		})
	}
}
