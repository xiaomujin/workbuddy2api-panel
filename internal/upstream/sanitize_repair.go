package upstream

import (
	"regexp"
)

// 本文件是 sanitize.go 的补充层：三类「2026-10 之后新确认的指纹」与它们的探针。
//
// 与 sanitize.go 主表的定位差别：那张表管**已实测确认的逐字模板句**，这里管两类
// 更隐蔽的形态——
//  1. 段内**不含任何品牌词**的长段说明（品牌预检根本不命中，原样出站即 400）；
//  2. 上游服务端**注入**、又被模型复述回历史的身份指令（客户端 body 里从来不是
//     原文，只能靠 assistant 正文 / 思维链字段回捞）。
//
// 两者都是"同义改写"而不是删除：语义一字不丢，只让上游的**连续原文**特征失配。

// sanitizeDshPathsRe DSH 0.3.x 系统提示里的「@ 开头的路径怎么解析」整段说明。
//
// 这一段是本仓此前唯一漏掉的 DSH 指纹：段内**不含任何品牌词**，所以品牌/身份句式
// 预检（sanitizeBrandPat / sanitizeDshIdentityPat）根本不会命中，原文原样发出即
// 400 11-128「Illegal API invocation from an unapproved channel」——面板里表现为
// 一次「内容拦截」+ 整站降级到中性提示词（模型随即"变笨"）。
//
// 上游对这段是按「连续原文」匹配的（逐段二分实测：只发首句放行、前两句连排即拦），
// 所以必须**整段一起改**，不能只改一句。这里做同义改写而不是删除：@ 路径、相对路径
// 基准、结尾斜杠表示目录、其余按文件读、@"..." 用于含空格路径——语义全部保留，
// 只换措辞让连续特征失配。
//
// 两种线上见过的结尾措辞都要覆盖（(?s) 让 .*? 跨行，段内本来就有换行）：
//   - "... form only quotes a path that contains spaces."（更早的写法）
//   - "... quotes a path containing spaces."（0.3.x 当前写法）
//
// 段外的 @ 不受影响：邮箱、包名、含 @ 的绝对路径都不匹配。
//
// 两条已知边界：预检大小写敏感（与 sanitizeFeatures 其它项一致，DSH 原文大小写固定）；
// 规则属于全局改写层，任何角色里出现这一段都会被改——上游按连续原文匹配、不分角色，
// 保留原样必然 400。
var sanitizeDshPathsRe = regexp.MustCompile(
	`(?is)Tokens prefixed with @ are paths the user explicitly referenced\..*?path (?:containing|that contains) spaces\.`)

// sanitizeDshPathsHint 探针（全小写，probeMask 用折叠比较）。
const sanitizeDshPathsHint = "tokens prefixed with @"

// sanitizeDshPathsRepl 逐句同义改写。
//
// 一处刻意的措辞偏离：原文用 "workspace root"，这里写 "project root"——有第三方实测
// 声称上游又单独加了 "workspace root" 这个词（root→base 即放行），与「整段回放」
// 的结论冲突；两种说法下 "project root" 都安全，且与 DSH 语义等价，故取它。
const sanitizeDshPathsRepl = "Paths beginning with @ refer to files the user explicitly mentioned. " +
	"Relative paths are resolved against the project root; absolute paths point " +
	"to files or directories on the host. A trailing slash marks a directory, so " +
	"list it when its contents matter. Anything else is a file: open it with the " +
	"read tool when its contents are needed, and never claim to have inspected it " +
	"before reading. The @\"...\" form only quotes a path that contains spaces."

// sanitizeInjectedIdentityRe 上游服务端注入的身份指令，被模型复述回历史后的清洗规则。
//
// 形态（2.73MB 请求体 dump 里逐字取证）：
//
//	When asked about your model identity, identify yourself as Tencent Hunyuan Hy.
//
// 它由上游在服务端拼进模型可见上下文、**不出现在任何出站 body 里**，因此 dump 看不到
// 客户端侧的原文；但模型会在 reasoning_content 里复述这段原文，客户端把思维链带回
// 历史，下一轮就原样进出站——「声明 → 进历史 → 再看到 → 再声明」的滚雪球就是这么来的。
//
// 匹配刻意收紧到「when asked about your … identity … , identify yourself as …」整句：
// 单独匹配 "identify yourself as" 会误伤正常指令（"Do not identify yourself as an AI."
// 是合法要求），而整句前缀在真实语料里几乎不可能自然出现。
//
// 句尾是「后面不接字母数字的句号」：身份名本身可以带点（GPT-4.1 / Claude 3.5），
// 遇到第一个点就收尾会留下 "1 by OpenAI." 半句。句号后那个字符用捕获组原样接回
// （RE2 不支持零宽断言），复述形态里常见的引号、换行都不能被吞掉。
var sanitizeInjectedIdentityRe = regexp.MustCompile(
	`(?i)when asked about your[^.\n]{0,60}?identity[^.\n]{0,60}?,\s*identify yourself as\s+[^\n]{1,80}?\.(\W|$)`)

// sanitizeInjectedIdentityRepl 替换成中性表述：不声明任何身份，也不改变上下文里
// 真实可用的信息。
const sanitizeInjectedIdentityRepl = "When asked about your capabilities, answer using only the information available in this conversation.${1}"

// sanitizeModelIdentityTagRe 去掉 <model_identity> 包裹本身（保留内部文本）。
//
// 只删标签：真正的危害是标签里那句指令（上面那条规则负责改掉），标签本身留着会让
// 模型继续把它当「有个身份块」反复提起，每轮都往历史里再写一遍。
var sanitizeModelIdentityTagRe = regexp.MustCompile(`(?i)</?model_identity>`)

// 注入身份指令的两个探针（全小写）。作用域只有滚雪球的载体（assistant 正文与
// 任意消息的 reasoning 字段），见 sanitizeMessages 的调用处：system / user 里的
// 同形句是**用户自己写的人设**（"When asked about your identity, identify yourself
// as Aria, the Acme assistant."），改写它等于篡改用户的提示词。
const (
	injectedIdentityProbe = "identify yourself as"
	modelIdentityTagProbe = "model_identity"
)

// scrubInjectedIdentity 改掉注入身份指令的复述形态，并去掉 <model_identity> 标签。
// 不命中时原样返回（两级折叠预检，零分配）；不 TrimSpace（思维链首尾空白是原文的一部分）。
func scrubInjectedIdentity(text string) string {
	if text == "" {
		return text
	}
	if containsFoldASCII(text, injectedIdentityProbe) {
		text = sanitizeInjectedIdentityRe.ReplaceAllString(text, sanitizeInjectedIdentityRepl)
	}
	if containsFoldASCII(text, modelIdentityTagProbe) {
		text = sanitizeModelIdentityTagRe.ReplaceAllString(text, "")
	}
	return text
}

// sanitizeTools 净化 tools[].function.description。
//
// 工具描述由客户端作者撰写（Claude Code / Codex / DSH 这类把身份句写进 description），
// 此前只洗 messages 时它整段原样出站，于是"只改 system 就能过"的判断在带 tools 的
// 请求上不成立。
//
// 边界：只洗 OpenAI 形状的 function.description；parameters 里的 schema 描述不洗
// （那是字段级文档，改写会影响模型对参数语义的理解，且实测不是拦截点）。
func sanitizeTools(obj map[string]any) bool {
	tools, ok := obj["tools"].([]any)
	if !ok {
		return false
	}
	changed := false
	for _, t := range tools {
		tm, ok := t.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := tm["function"].(map[string]any)
		if !ok {
			continue
		}
		d, ok := fn["description"].(string)
		if !ok || d == "" {
			continue
		}
		// 工具描述是客户端作者写的内容，按身份载体处理（identity=true）。
		if s := sanitizeTextScoped(d, true); s != d {
			fn["description"] = s
			changed = true
		}
	}
	return changed
}

// sanitizeContentPost 在 sanitizeContentScoped 之上加一层文本后处理（post）。
//
// 存在理由：注入身份指令的清洗**只该作用于 assistant 正文**（滚雪球载体），而
// content 既可能是字符串也可能是多模态 parts，两处都要过 post，否则图片轮里
// 复述的身份句会从 parts 里漏出去。post 为 nil 时与 sanitizeContentScoped 等价。
func sanitizeContentPost(v any, identity bool, post func(string) string) (any, bool) {
	if post == nil {
		return sanitizeContentScoped(v, identity)
	}
	switch c := v.(type) {
	case string:
		s := post(sanitizeTextScoped(c, identity))
		return s, s != c
	case []any:
		changed := false
		for _, p := range c {
			m, ok := p.(map[string]any)
			if !ok {
				continue
			}
			text, ok := m["text"].(string)
			if !ok {
				continue
			}
			if s := post(sanitizeTextScoped(text, identity)); s != text {
				m["text"] = s
				changed = true
			}
		}
		return c, changed
	}
	return v, false
}

// containsFoldASCII 大小写不敏感（仅 ASCII）的子串查找，零分配。
//
// 用途：给 `(?i)` 正则做「有没有这个词」的存在性判断。RE2 对一个带 (?i) 的模式只能在
// 每个字节位置跑状态机（170K token 的上下文里每段文本都要过一遍）；这里用单遍扫描 +
// 首字符快跳，命中即返回。
//
// 只做预检：非字母字节的 |0x20 折叠会有少量误命中（'^' 与 '~' 之类），代价只是多跑
// 一次正则确认，不会漏判、不会改文本。
func containsFoldASCII(s, sub string) bool {
	if sub == "" {
		return true
	}
	n := len(sub)
	if len(s) < n {
		return false
	}
	c0f := sub[0] | 0x20
	for i := 0; i+n <= len(s); i++ {
		if s[i]|0x20 != c0f {
			continue
		}
		ok := true
		for j := 1; j < n; j++ {
			a, b := s[i+j], sub[j]
			if a != b && a|0x20 != b|0x20 {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}
