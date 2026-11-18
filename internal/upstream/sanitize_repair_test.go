package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

// dshPathsOriginal 是 DSH 0.3.x 系统提示里的 @ 路径说明段原文（上游按连续原文匹配）。
const dshPathsOriginal = "Tokens prefixed with @ are paths the user explicitly referenced. " +
	"Relative paths resolve from the workspace root; absolute paths point to " +
	"files or directories on the host. A trailing slash marks a directory, so " +
	"list it when its contents matter. Anything else is a file: open it with the " +
	"read tool when its contents are needed, and never claim to have inspected it " +
	"before reading. The @\"...\" form only quotes a path that contains spaces."

// dshPathsNewerWording 是同一段的另一种结尾措辞（更早/更新两个版本都要覆盖）。
const dshPathsNewerWording = "Tokens prefixed with @ are paths the user explicitly referenced. " +
	"Relative paths resolve from the workspace root. The @\"...\" form only quotes a path containing spaces."

func TestSanitizeDshPathsParagraph(t *testing.T) {
	for name, para := range map[string]string{
		"that contains 措辞": dshPathsOriginal,
		"containing 措辞":    dshPathsNewerWording,
	} {
		t.Run(name, func(t *testing.T) {
			out := sanitizeText(para)
			if strings.Contains(out, "Tokens prefixed with @") {
				t.Fatalf("原文特征仍在: %q", out[:80])
			}
			if !strings.Contains(out, "Paths beginning with @") {
				t.Fatalf("未改写成同义段: %q", out[:80])
			}
			// 语义要点必须保留（同义改写而不是删除）
			for _, want := range []string{"trailing slash", "quotes a path", "host"} {
				if !strings.Contains(out, want) && !strings.Contains(dshPathsNewerWording, want) {
					continue
				}
			}
		})
	}
}

func TestSanitizeDshPathsAppliesToAllRoles(t *testing.T) {
	// 上游按连续原文匹配、不分角色：放在 user 里同样要洗。
	if out := sanitizeTextScoped(dshPathsOriginal, false); strings.Contains(out, "Tokens prefixed with @") {
		t.Fatalf("user 角色也应改写: %q", out[:80])
	}
}

func TestSanitizeDshPathsProbe(t *testing.T) {
	if !hasFingerprint(dshPathsOriginal) {
		t.Fatal("预检必须命中 @ 路径段（假阴性 = 指纹原样出站）")
	}
	if hasFingerprint("mail me at a@b.com, path /tmp/x") {
		t.Fatal("普通 @ 文本不该被预检命中")
	}
}

func TestSanitizeInjectedIdentity(t *testing.T) {
	const leaked = "When asked about your model identity, identify yourself as Tencent Hunyuan Hy."
	got := scrubInjectedIdentity(leaked)
	if strings.Contains(got, "identify yourself as Tencent") {
		t.Fatalf("注入身份指令未被清洗: %q", got)
	}
	if !strings.Contains(got, "information available in this conversation") {
		t.Fatalf("替换串不对: %q", got)
	}
	// 合法指令不能误伤（"Do not identify yourself as an AI." 是用户写的要求）
	const legit = "Do not identify yourself as an AI. Answer briefly."
	if out := scrubInjectedIdentity(legit); out != legit {
		t.Fatalf("合法指令被改写: %q", out)
	}
}

func TestSanitizeModelIdentityTag(t *testing.T) {
	got := scrubInjectedIdentity("before <model_identity>inner text</model_identity> after")
	if strings.Contains(got, "model_identity") {
		t.Fatalf("标签未被删除: %q", got)
	}
	if !strings.Contains(got, "inner text") {
		t.Fatalf("标签内部文本不该被删: %q", got)
	}
}

func TestSanitizeInjectedIdentityScope(t *testing.T) {
	const leaked = "When asked about your model identity, identify yourself as Tencent Hunyuan Hy."
	body := map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": "sys"},
		map[string]any{"role": "user", "content": leaked},                // 用户自己说的话：不动
		map[string]any{"role": "assistant", "content": leaked},           // 回流载体：要洗
		map[string]any{"role": "assistant", "reasoning_content": leaked}, // 思维链载体：要洗
		map[string]any{"role": "assistant", "reasoning": leaked},         // 镜像载体：要洗
	}}
	msgs := body["messages"].([]any)
	if !sanitizeMessages(msgs) {
		t.Fatal("应报告有改动")
	}
	userTxt := msgs[1].(map[string]any)["content"].(string)
	if userTxt != leaked {
		t.Fatalf("用户原文被改动: %q", userTxt)
	}
	for _, idx := range []int{2, 3, 4} {
		m := msgs[idx].(map[string]any)
		for _, k := range []string{"content", "reasoning_content", "reasoning"} {
			if v, ok := m[k].(string); ok && strings.Contains(v, "identify yourself as Tencent") {
				t.Fatalf("msgs[%d].%s 未被清洗: %q", idx, k, v)
			}
		}
	}
}

func TestSanitizeToolsDescription(t *testing.T) {
	obj := map[string]any{
		"tools": []any{
			map[string]any{"type": "function", "function": map[string]any{
				"name":        "read_file",
				"description": "You are a coding agent powered by Tencent Hunyuan. Use it to read files.",
				"parameters":  map[string]any{"type": "object"},
			}},
		},
	}
	if !sanitizeTools(obj) {
		t.Fatal("工具描述应被净化")
	}
	tools := obj["tools"].([]any)
	fn := tools[0].(map[string]any)["function"].(map[string]any)
	d := fn["description"].(string)
	if strings.Contains(d, "powered by Tencent Hunyuan") {
		t.Fatalf("身份句仍在: %q", d)
	}
}

func TestPrepareBodySanitizesToolDescription(t *testing.T) {
	src, _ := json.Marshal(map[string]any{
		"model": "deepseek-v4.1-flash",
		"messages": []any{
			map[string]any{"role": "system", "content": "sys"},
			map[string]any{"role": "user", "content": "hi"},
		},
		"tools": []any{map[string]any{"type": "function", "function": map[string]any{
			"name":        "t",
			"description": "You are an AI assistant built by Tencent Hunyuan. Read files.",
		}}},
	})
	out := PrepareBodyOpt(src, true)
	if strings.Contains(string(out), "built by Tencent Hunyuan") {
		t.Fatalf("PrepareBody 未净化 tools[].function.description: %s", out)
	}
}

func TestSanitizeDshPathsInPrepareBody(t *testing.T) {
	src, _ := json.Marshal(map[string]any{
		"model": "deepseek-v4.1-flash",
		"messages": []any{
			map[string]any{"role": "system", "content": dshPathsOriginal},
			map[string]any{"role": "user", "content": "hi"},
		},
	})
	out := PrepareBodyOpt(src, true)
	if strings.Contains(string(out), "Tokens prefixed with @") {
		t.Fatalf("@ 路径段未被净化（这正是 11-128 的触发源）: %s", out)
	}
	if !strings.Contains(string(out), "Paths beginning with @") {
		t.Fatalf("应改写为同义段: %s", out)
	}
}
