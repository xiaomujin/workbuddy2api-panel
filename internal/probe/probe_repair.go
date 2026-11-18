//go:build ignore

// probe_repair 离线二分定位「11-128 拦截到底拦的是哪一段」。
//
// 输入：网关落盘的修复现场（server 的 repair.dump_dir，只含 system 文本与角色序列，
// 不含用户内容）。输出：最小触发前缀 + 建议的修复级别，供人工写回段指纹记忆
// （面板「修复记忆」卡片或 data/repair-memory.json）。
//
// 为什么不做自动灌库：每次探测都要真的打上游（多一次 400 = 多一次风控暴露），
// 而"到底该用哪一级"是人的判断（拆句够了 vs 只能搬家）。工具把**事实**测出来，
// 决策留在人手里。
//
// 用法（仓库根目录）：
//
//	go run internal/probe/probe_repair.go \
//	    -dump data/dump/20261118T035841Z-cn-a1b2c3d4.json \
//	    -auths ./auths -realm cn
//
// 只打印状态码与上游错误原文（截断），不打印任何 token。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/prompt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// dumpFile 与 server/repair_dump.go 的落盘格式保持一致。
type dumpFile struct {
	Version     int       `json:"version"`
	Time        time.Time `json:"time"`
	Realm       string    `json:"realm"`
	Seg         string    `json:"seg"`
	Model       string    `json:"model"`
	Reason      string    `json:"reason"`
	SystemTexts []string  `json:"system_texts"`
	Roles       []string  `json:"roles"`
}

func main() {
	dumpPath := flag.String("dump", "", "修复现场 json（server repair.dump_dir 的产物）")
	authDir := flag.String("auths", "./auths", "auth 文件目录")
	realm := flag.String("realm", "", "cn / global（空 = 用 dump 里记录的域）")
	model := flag.String("model", "", "模型 id（空 = 用 dump 里记录的模型）")
	raw := flag.Bool("raw", false, "探测时关闭指纹脱敏（默认关闭脱敏：我们要看上游对原文的反应）")
	flag.Parse()

	if *dumpPath == "" {
		fmt.Println("缺少 -dump")
		os.Exit(2)
	}
	rawDump, err := os.ReadFile(*dumpPath)
	if err != nil {
		fmt.Println("read dump:", err)
		os.Exit(2)
	}
	var d dumpFile
	if err := json.Unmarshal(rawDump, &d); err != nil {
		fmt.Println("parse dump:", err)
		os.Exit(2)
	}
	if len(d.SystemTexts) == 0 {
		fmt.Println("dump 里没有 system 文本，无法定位（该请求的拦截不在 system 来源）")
		os.Exit(2)
	}
	r := *realm
	if r == "" {
		r = d.Realm
	}
	if r == "" {
		r = "cn"
	}
	m := *model
	if m == "" {
		m = d.Model
	}
	if m == "" {
		m = "deepseek-v4.1-flash"
	}

	// 只探测**最长的那条** system：那才是"连续原文"最可能命中的载体。
	longest := ""
	for _, s := range d.SystemTexts {
		if len(s) > len(longest) {
			longest = s
		}
	}
	parts := splitBySentence(longest)
	fmt.Printf("dump: %s\nrealm=%s model=%s seg=%s reason=%s\nsystem=%d 段, 最长 %d 字节 -> %d 句\n\n",
		filepath.Base(*dumpPath), r, m, d.Seg, d.Reason, len(d.SystemTexts), len(longest), len(parts))

	a := pickAccount(*authDir, r)
	if a == nil {
		fmt.Printf("FAIL: %s 目录里没有可用的 realm=%s 账号\n", *authDir, r)
		os.Exit(1)
	}
	c := upstream.New()
	c.SanitizeFingerprints.Store(!*raw)

	// 探针 1：整段原文（期望 400 —— 证明段定位正确、账号/模型与现场一致）。
	st, body := send(c, a, m, longest)
	fmt.Printf("%-6s 探针1 整段原文              status=%d %s\n", verdict(st == 400, st), firstLine(body))
	if st != 400 {
		fmt.Println("\n整段没被拦：现场已失效（上游规则变了 / 账号或模型不同 / dump 过期），停止。")
		return
	}

	// 探针 2：单句。单句就被拦 → 拆句（L2）救不了，建议搬家（L3）。
	st1, body1 := send(c, a, m, parts[0])
	fmt.Printf("%-6s 探针2 只发第 1 句           status=%d %s\n", verdict(st1 == 400, st1), firstLine(body1))

	// 探针 3：最少几句连排才被拦（前缀二分）。
	minK := 0
	lo, hi := 2, len(parts)
	for lo <= hi {
		mid := (lo + hi) / 2
		stm, _ := send(c, a, m, strings.Join(parts[:mid], " "))
		if stm == 400 {
			minK = mid
			hi = mid - 1
		} else {
			lo = mid + 1
		}
	}
	if minK == 0 {
		fmt.Println("探针3 前缀二分：没有前缀被拦（可能整段才拦，或拦截在 user 来源）")
	} else {
		fmt.Printf("探针3 最少 %d 句连排即被拦：\n      第 %d~%d 句（%d 字节）\n", minK, 1, minK, len(strings.Join(parts[:minK], " ")))
	}

	// 建议级别：单句就拦 → L2 拆句无效（拆完仍在某条 system 里），必须 L3 搬家；
	// 只有连排才拦（≥2 句）→ L2 拆句即可（句界一断就失配，且一字不改）。
	fmt.Println()
	switch {
	case st1 == 400:
		fmt.Println("建议：level=3（搬家）。单句即被拦，拆句改不动它；把 system 原文搬进首条 user 可过。")
	case minK >= 2:
		fmt.Println("建议：level=2（拆句）。只有多句连排才拦，按句界拆成多条 system 即失配，且文本一字不改。")
	default:
		fmt.Println("建议：先人工复核。探针结果不一致（整段拦、单句不拦、二分无结论）。")
	}
	fmt.Println("写回：面板「修复记忆」→ realm + seg 填上面的值，level 填建议级别。")
}

func verdict(ok bool, status int) string {
	if ok {
		return "PASS"
	}
	return "DIFF"
}

// splitBySentence 复用生产侧的拆句实现（SplitSystemMessages 内部同一套规则），
// 保证"离线测出来的边界"与"线上拆出来的边界"完全一致。
func splitBySentence(text string) []string {
	body, _ := json.Marshal(map[string]any{
		"messages": []any{map[string]any{"role": "system", "content": text}},
	})
	if out, ok := prompt.SplitSystemMessages(body); ok {
		var obj struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(out, &obj); err == nil && len(obj.Messages) > 1 {
			parts := make([]string, 0, len(obj.Messages))
			for _, mm := range obj.Messages {
				parts = append(parts, mm.Content)
			}
			return parts
		}
	}
	return []string{text}
}

// send 走网关真实的出站管线（ChatStream），返回状态码与响应原文。
func send(c *upstream.Client, a *auth.Auth, model, system string) (int, string) {
	body, _ := json.Marshal(map[string]any{
		"model":      model,
		"stream":     true,
		"max_tokens": 32,
		"messages": []any{
			map[string]any{"role": "system", "content": system},
			map[string]any{"role": "user", "content": "Say OK."},
		},
	})
	rc, status, resp, err := c.ChatStream(a, body, "127.0.0.1", upstream.ChatMeta{})
	if err != nil && status == 0 {
		return -1, "ERR: " + err.Error()
	}
	if rc != nil {
		defer rc.Close()
	}
	if status != 200 {
		return status, string(resp)
	}
	// 200：读一点正文就够（这里只关心状态码）。
	buf, _ := io.ReadAll(io.LimitReader(rc, 4<<10))
	return status, string(buf)
}

func firstLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 140 {
		return s[:140] + "…"
	}
	return s
}

// pickAccount 取一个该 realm 的可用账号（不回显凭证）。
func pickAccount(authDir, realm string) *auth.Auth {
	entries, err := os.ReadDir(authDir)
	if err != nil {
		return nil
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(authDir, e.Name()))
		if err != nil {
			continue
		}
		a, err := auth.Parse(raw)
		if err != nil || a == nil || a.AccessToken == "" {
			continue
		}
		if a.IsGlobal() != (realm == "global") {
			continue
		}
		if a.ExpiresAt > 0 && time.Now().Unix() > a.ExpiresAt {
			continue
		}
		return a
	}
	return nil
}
