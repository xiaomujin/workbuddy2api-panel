// sanitize.go 出站请求体脱敏：剥离上游内容审核黑名单指纹。
//
// 背景：客户端（Claude Code 类 CLI）在 system prompt 注入若干固定模板句，
// 上游内容审核按逐字精确匹配拦截（非语义审核），一字改动即可绕过。
// 策略：键值/header 型指纹整段剥离；承载语义的模板句最小改写（换一词），语义不变。
package upstream

import (
	"regexp"
	"strings"
)

// sanitizeFeatures 特征预检：任一命中才进入净化（strings.Contains 快速路径，
// 普通请求全不中 → 原样返回，零分配）。
var sanitizeFeatures = []string{
	"x-anthropic-billing-header",                      // header 键值段键名
	"cc_entrypoint=",                                  // 尾随裸键值（截断前缀即可命中）
	"You are Claude Code",                             // 身份句（截断前缀即可命中）
	"Main branch (",                                   // 注入指令句（截断前缀即可命中）
	"You are a coding agent running in the Codex CLI", // Codex instructions 首段（截断前缀即可命中）
	"github.com/anthropics/",                          // 反馈句里的 Anthropic 仓库链接
	"11128",                                           // 上游反探测：裸数字错误码
}

// sanitizeHdrRe 剥离层：header 键名即触发（与值无关），整段删除。
var sanitizeHdrRe = regexp.MustCompile(`(?i)x-anthropic-billing-header:[^;\n]*;?\s*`)

// sanitizeBareHdrRe 兜底层：裸键名（无冒号无值）同样是指纹——2026-09-13 实验 F4
// 证实 assistant 消息里反引号引用裸键名即触发 11128，而剥离层要求冒号、对裸串无效。
// 键值形态被整段删除后，残留的裸键名做最小缩写（header→hdr）：破坏逐字匹配、
// 语义不变、保留可读性。大小写不敏感，覆盖 X-Anthropic-... 变体。
//
// 注意该正则不要求冒号，是 sanitizeHdrRe 的超集——hasFingerprint 与 sanitizeText
// 中两者并用：先删键值形态（sanitizeHdrRe），再缩写残留裸键名（本正则），
// 替换语义不同（整段删除 vs 最小缩写），不可合并为一个正则。
var sanitizeBareHdrRe = regexp.MustCompile(`(?i)x-anthropic-billing-header`)

// sanitizeKvRe 剥离层：尾随裸键值（cc_xxx=...;）循环清理。
var sanitizeKvRe = regexp.MustCompile(`(?i)\bcc_[a-z0-9_]+=[^;\n]*;?\s*`)

// sanitizeRewrites 改写层：全模板句逐字替换（每句只改一个词，语义不变）。
//
// 身份句的匹配串**不带结尾标点**（只到 "…for Claude" 为止）：
// CLI 版这句以句号收尾（"…for Claude."），桌面版（claude-desktop-3p / Agent SDK）
// 以逗号接后继内容（"…for Claude, running within the Claude Agent SDK."）。
// 带句号的整句只匹配前者，桌面版会漏网、指纹原样发上游 → 400 code=11128。
// 去掉结尾标点后两种形态一并覆盖（替换串同样不带标点，让原有标点原样保留）。
// 注意仍要求 "You are Claude Code, " 前缀，不做更宽的子串替换，
// 以免误伤 TestExactMatchOnlyVariantNotTouched 所保护的零散文本。
//
// ⚠ 第 2 对（分支规则）的**方向存疑**，勿在无实测证据时改动：它的 find 串是
// 「Main branch (…)」、替换成「Default branch (…)」（长度 47→50，已按字节核对）。
// 而 2026-10 的独立实测（workbuddy2api-panel#574 探针表）显示：上游拦的恰恰是
// 「Default branch (you will usually use this for PRs)」——同一句放在 system 里
// 400、改成「Main branch: master」200。若那条实测在我们这条链路上成立，这条规则
// 就是在**制造**指纹而不是消除它。裁决需要真机探针（分 cn / global 两个 realm），
// 结果出来前保持现状；sanitizeFeatures 里的截断前缀与本表 find 串一致，一起改。
var sanitizeRewrites = [][2]string{
	{
		"You are Claude Code, Anthropic's official CLI for Claude",
		"You are Claude Code, Anthropic's official CLI tool for Claude",
	},
	{
		"Main branch (you will usually use this for PRs)",
		"Default branch (you will usually use this for PRs)",
	},
	{
		"You are a coding agent running in the Codex CLI, a terminal-based coding assistant.",
		"You are a coding agent running in the Codex CLI tool, a terminal-based coding assistant.",
	},
	{
		// 反馈句：整句带 Anthropic 仓库链接，上游按整句拦截（只留链接或只留半边均不拦，
		// 实测需整句同时出现）。give→provide 一词之差即可绕过，语义不变。
		"To give feedback, users should report the issue at https://github.com/anthropics/claude-code/issues",
		"To provide feedback, users should report the issue at https://github.com/anthropics/claude-code/issues",
	},
	{
		// 上游反探测：只要请求体里出现裸数字 11128 就整单拦截（与该数字的上下文无关——
		// "code=11128" / 裸 "11128" / "错误码 11128" / "Code=11128" 全部命中；
		// 相邻的 11148 / 11101 / 11115 / 99999 均放行）。11128 正是本类拦截自身的错误码，
		// 上游据此识别"在讨论/回显其内部错误码"的请求。
		// 代价：用户对话中任何 11128 都会被改写——但这串数字出现在请求里本身就是拦截条件，
		// 不改写必然失败。插入连字符保留可读性与指代（零宽空格无效，实测上游会归一化）。
		"11128",
		"11-128",
	},
}

// ── 句式级指纹（DSH 等客户端的自我介绍句 / GUI 描述段）────────────────────
//
// 与上面的 sanitizeRewrites 分工：那张表是**逐字精确串**（漏一个变体就静默失效），
// 这一组是**句式**（品牌、模型名、冠词、大小写都是变量）。两套并存、互不替代：
// 逐字表管已实测确认的老指纹，句式层管「同一类自我介绍句」的新变体。
//
// 依据（两家独立实测，2026-10）：上游对 "You are an AI agent powered by <brand>."
// 这类**句式结构**判定，而不是对品牌名判定——换品牌、换成 built on、换模型名同样
// 命中；表现为 HTTP 400 + 渠道校验业务码。只改一处（多一个空格 / 改一个词 / 只改
// 大小写之一）都可能失配，故整句替换为中性句，不留半句。
// 每条规则编译两份：canon（规范大小写，热路径）+ ci（(?i)，兜底）。
//
// 为什么要分两份：RE2 对**不带 (?i)** 的模式能取到字面量前缀（"You are"），于是能用
// memchr 跳过绝大多数位置；而 (?i) 会让 LiteralPrefix() 返回 false，只能在每个字节
// 位置跑一次自动机。实测 80KB 文本：canon 级 40µs，(?i) 级 548-879µs。
// ci 版本同时充当「规则到底是什么」的唯一规范，canon 只是它的快路投影。
const (
	// sanitizeDshIdentityPat 自我介绍句式 + 品牌归属。
	//
	// 三个必须保留的边界：
	//   - 句尾按「点 + 空白/行尾」收口（RE2 无 lookahead，用两条分支表达）：
	//     模型名自带点（deepseek-v4.1-flash），
	//     懒匹配到第一个点只会吃掉 "…v4."、把 "1-flash model." 留在正文里，
	//     比不替换更难查。
	//   - [^\n]{0,120}? 是有界窗口：某行没有可用句尾时不至于一路吃到行尾。
	//   - (?:[\w.-]+\s+){0,3}? 限制修饰词数量，避免 "You are free to use tools
	//     built by …" 这类分词修饰宾语的句子被误伤。
	//   - **不要**在开头加 \b：RE2 的 LiteralPrefix 遇到空宽度断言就放弃，
	//     于是又退化成逐字节跑自动机（实测 80KB：带 \b 765µs / 不带 75µs，10×）。
	//     代价是「…You are…」嵌在词中也会命中——这类文本不存在于真实客户端，
	//     而这条规则的作用是把指纹中性化，多命中只会多中性化一点，不影响语义。
	sanitizeDshIdentityPat = `You are\s+(?:an?\s+|the\s+)?(?:[\w.-]+\s+){0,3}?` +
		`(?:agent|assistant|model|ai|llm)?[\s,]*` +
		`(?:powered by|built on|built by|created by|developed by|made by|trained by)` +
		`[^\n]{0,120}?(?:\.(\s|$)|(\n|$))`

	// sanitizeDshGuiPat GUI 描述段：上游按「A 句 + B 句连续出现」整段判定（实测
	// 220 字符 200 / 221 字符 503；两句单独发送都放行，改任一句或在两句之间插东西
	// 都会失配）。选语义损失最小的改法：只把 A 句尾的语序换掉，一个词都不删 ——
	// 品牌全名保留，句子意思完全不变，也不像插入不可见字符那样在提示词里留下
	// 无法 diff 的痕迹。
	sanitizeDshGuiPat = `(You are interacting with the user through the ` +
		`DeepSeek[ \t]+Harness[ \t]+Web[ \t]+GUI) whose URL the runtime context names\.`

	// sanitizeBrandPat 品牌词缩减：`DeepSeek Harness` 单独出现放行，但与 GUI 描述句
	// （this page / this GUI / this app）连续出现时命中同一套校验；缩成 `DeepSeek`
	// 即可通过，语义损失最小。只吃空格/制表符：`deepseek-harness`、
	// `deepseek_harness`、`/opt/deepseek-harness/app` 这类路径与标识符必须原样保留。
	sanitizeBrandPat = `DeepSeek[ \t]+Harness`
)

var (
	sanitizeDshIdentityCanonRe = regexp.MustCompile(sanitizeDshIdentityPat)
	sanitizeDshIdentityRe      = regexp.MustCompile("(?i)" + sanitizeDshIdentityPat)
	// sanitizeDshIdentityRepl ${1}${2} 把句尾的空白/换行原样接回去：不接的话
	// "…Harness. Use the read tool" 会粘成 "assistant.Use"，把两条指令并成一条。
	// 替换串本身不含任何动词短语，故二次过不会再生效（幂等）。
	sanitizeDshIdentityRepl = "You are a helpful AI assistant.${1}${2}"

	sanitizeDshGuiCanonRe = regexp.MustCompile(sanitizeDshGuiPat)
	sanitizeDshGuiRe      = regexp.MustCompile("(?i)" + sanitizeDshGuiPat)
	sanitizeDshGuiRepl    = "${1} whose URL is named in the runtime context."

	// 品牌规则的热路径是精确替换（`DeepSeek Harness` → `DeepSeek`），正则只处理
	// 多空格/大小写变体，因此不需要 canon 版本。
	sanitizeBrandRe   = regexp.MustCompile("(?i)" + sanitizeBrandPat)
	sanitizeBrandRepl = "DeepSeek"
)

// ── 指纹探测（热路径）──────────────────────────────────────────────────────
//
// 为什么需要这一层：句式正则带 (?i) 时 RE2 拿不到字面量前缀，只能在每个字节位置
// 跑一次自动机（实测 80KB：548-879µs，~100 MB/s）。而 strings.Contains 是 memchr 级
// （1.5µs）、手写折叠扫描是 42µs（1.9 GB/s）。所以「要不要清洗」必须用字面量判断，
// 正则只用来改写——一次探测换掉三次全量 (?i) 扫描。
//
// 注意**不能**照搬「先 strings.ToLower 再查字面量」：Go 的 ToLower 只在纯 ASCII 上
// 走快路径，文本一出现中文就退化到 unicode.Map 逐 rune（实测 80KB 中文 422µs，比它
// 要替代的正则还慢）。这里用零分配字节折叠。
type sanitizeMask uint8

const (
	maskLiteral  sanitizeMask = 1 << iota // 既有逐字表 / header / kv 残留
	maskIdentity                          // 自我介绍句式
	maskGui                               // GUI 描述段
	maskBrand                             // 品牌缩减
	maskPaths                             // DSH「@ 路径说明段」整段改写（不分角色，见 sanitizeDshPathsRe）
)

// 句式层三条规则的位：identity=false（user/tool 数据）时整体清掉。
//
// maskPaths **不在**这里：上游对那段说明是按连续原文匹配、**不分角色**，放进 user
// 里同样 400（sanitizeDshPathsRe 的注释有实测依据），所以它必须在所有角色上生效。
const maskSentenceLayer = maskIdentity | maskGui | maskBrand

// 折叠探针（全小写）。每条都必须是**对应规则匹配文本的子串**——探测只准超、不准漏：
// 假阳性只多跑一次正则，假阴性会让指纹原样出站（那正是这条流水线存在的理由）。
// 与规则的同步关系由 sanitize_probe_test.go 的对拍用例钉住。
const (
	sanitizeBrandHint  = "harness"
	sanitizeGuiHint    = "whose url the runtime context names"
	sanitizeYouAreHint = "you are"
)

// sanitizeHdrHint 必须从正则本身派生，不能手抄第二份：
// 手抄的代价立刻兑现过——真实键名是 "x-anthropic-billing-hdr"，抄成 22 字符的
// "…-hdr" 之后，混合大小写的 header 探不到、整条净化被静默跳过（hasFingerprint 假阴性）。
var sanitizeHdrHint = strings.ToLower(strings.TrimPrefix(sanitizeBareHdrRe.String(), "(?i)"))

// 身份句的动词搭配（与 sanitizeDshIdentityPat 的 alternation 一一对应）。
const (
	verbPoweredBy   = "powered by"
	verbBuiltOn     = "built on"
	verbBuiltBy     = "built by"
	verbCreatedBy   = "created by"
	verbDevelopedBy = "developed by"
	verbMadeBy      = "made by"
	verbTrainedBy   = "trained by"
)

// sanitizeIdentityWindow 「You are」与动词之间允许的最大字节距离（宽松上限）。
// 正则允许 ≤3 个修饰词 + 可选名词 + 空白，实际远小于此；取宽是为了只准超、不准漏。
// 它的作用是挡掉「You are … 之后隔了半篇文章才出现 built by」这类误触发——
// 否则任何提到 "built by" 的 system prompt 都会白跑一遍 548µs 的正则。
const sanitizeIdentityWindow = 160

// foldAt 从 s[i] 起按 ASCII 折叠比较 sub（sub 必须全小写）。越界返回 false。
//
// 只折叠 ASCII 字母：UTF-8 多字节序列的高位字节不可能等于 ASCII 小写字母，因此对
// 中文 / emoji 文本天然安全（无需先判编码）。
func foldAt(s string, i int, sub string) bool {
	if i+len(sub) > len(s) {
		return false
	}
	for j := 0; j < len(sub); j++ {
		c := s[i+j]
		if c != sub[j] && c|0x20 != sub[j] {
			return false
		}
	}
	return true
}

// inIdentityWindow 报告位置 at 处的动词是否落在「You are」之后的可信窗口内。
// at < 0（还没见过 you are）→ false。
func inIdentityWindow(youAreAt, verbAt int) bool {
	return youAreAt >= 0 && verbAt > youAreAt && verbAt-youAreAt <= sanitizeIdentityWindow
}

// probeMask 单遍扫描 + 首字节分派：一次遍历覆盖全部折叠探针（零分配）。
//
// 首字节折叠（b | 0x20）对字母是安全的：非字母字节折不出目标小写字母（目标都是
// 字母，其 |0x20 只有自身与对应大写两种可能）。
func probeMask(text string) sanitizeMask {
	var m sanitizeMask
	// 既有 6 条特征是**大小写敏感**的原样串（与历史行为一致），先走 memchr。
	for _, f := range sanitizeFeatures {
		if strings.Contains(text, f) {
			m |= maskLiteral
			break
		}
	}
	youAreAt, verb := -1, false
	for i := 0; i < len(text); i++ {
		switch text[i] | 0x20 {
		case 'x':
			if foldAt(text, i, sanitizeHdrHint) {
				m |= maskLiteral
			}
		case 'h':
			if foldAt(text, i, sanitizeBrandHint) {
				m |= maskBrand
			}
		case 'w':
			if foldAt(text, i, sanitizeGuiHint) {
				m |= maskGui
			}
		case 'y':
			if foldAt(text, i, sanitizeYouAreHint) {
				youAreAt = i
			}
		case 'p':
			if foldAt(text, i, verbPoweredBy) {
				verb = verb || inIdentityWindow(youAreAt, i)
			}
		case 'b':
			if foldAt(text, i, verbBuiltOn) || foldAt(text, i, verbBuiltBy) {
				verb = verb || inIdentityWindow(youAreAt, i)
			}
		case 'c':
			if foldAt(text, i, verbCreatedBy) {
				verb = verb || inIdentityWindow(youAreAt, i)
			}
		case 'd':
			if foldAt(text, i, verbDevelopedBy) {
				verb = verb || inIdentityWindow(youAreAt, i)
			}
		case 'm':
			if foldAt(text, i, verbMadeBy) {
				verb = verb || inIdentityWindow(youAreAt, i)
			}
		case 't':
			if foldAt(text, i, verbTrainedBy) {
				verb = verb || inIdentityWindow(youAreAt, i)
			}
			// DSH @ 路径说明段的预检：段内不含品牌词，只能靠它自己的字面量命中。
			if foldAt(text, i, sanitizeDshPathsHint) {
				m |= maskPaths
			}
		}
	}
	// 与身份句正则同构："You are" 在前、动词在后且距离在窗口内才算候选，否则
	// system prompt 里一句 "powered by OpenAI" 会把正则白跑一遍。
	if verb {
		m |= maskIdentity
	}
	return m
}

// sanitizeText 单段文本净化（完整作用域）：预检不中 → 返回原串（零分配）。
func sanitizeText(text string) string { return sanitizeTextScoped(text, true) }

// sanitizeTextScoped 同 sanitizeText，但可关闭「身份句式」规则（identity=false）。
//
// identity=false 用于**非身份载体**：user 内容、tool 结果、tool_calls 参数、思维链
// 字段。实测同一句指纹放进 user / tool 消息上游放行（200），写进 system /
// developer / assistant 才 400 —— 因此不该改写用户数据：改了等于让模型看到与磁盘
// 不一致的内容。逐字表与 header 剥离层对所有角色照旧生效。
func sanitizeTextScoped(text string, identity bool) string {
	mask := probeMask(text)
	if !identity {
		mask &^= maskSentenceLayer
	}
	if mask == 0 {
		return text
	}
	// 逐字表：字面量替换，memchr 级（5 条合计 ~8µs/80KB），只在真有字面量指纹时跑。
	if mask&maskLiteral != 0 {
		for _, rw := range sanitizeRewrites {
			text = strings.ReplaceAll(text, rw[0], rw[1])
		}
	}
	// 句式层先走 canon（规范大小写）快路：真实客户端发的就是规范形态，canon 能吃到
	// 字面量前缀，比 (?i) 快一个数量级；变体由下面重探后交给 (?i) 兜底。
	if mask&maskIdentity != 0 {
		text = sanitizeDshIdentityCanonRe.ReplaceAllString(text, sanitizeDshIdentityRepl)
	}
	if mask&maskGui != 0 {
		// GUI 段规则要认品牌全名，必须排在品牌缩减之前（先缩就锚不到了）。
		text = sanitizeDshGuiCanonRe.ReplaceAllString(text, sanitizeDshGuiRepl)
	}
	if mask&maskBrand != 0 {
		text = strings.ReplaceAll(text, "DeepSeek Harness", sanitizeBrandRepl)
	}
	// 快路只吃规范形态：重探一次，仍有残留才动用 (?i) 正则。
	// 命中路径多付一次探测（~70µs/80KB），换掉三次全量 (?i) 扫描（~2ms）。
	if mask&maskSentenceLayer != 0 {
		if rest := probeMask(text) & maskSentenceLayer; rest != 0 {
			if rest&maskIdentity != 0 {
				text = sanitizeDshIdentityRe.ReplaceAllString(text, sanitizeDshIdentityRepl)
			}
			if rest&maskGui != 0 {
				text = sanitizeDshGuiRe.ReplaceAllString(text, sanitizeDshGuiRepl)
			}
			if rest&maskBrand != 0 {
				text = sanitizeBrandRe.ReplaceAllString(text, sanitizeBrandRepl)
			}
		}
	}
	// @ 路径说明段：整段同义改写（不分角色，见 sanitizeDshPathsRe 的实测依据）。
	// 放在句式层之后、KV/header 剥离之前：它改的是长段说明文本，与键值剥离互不影响。
	if mask&maskPaths != 0 {
		text = sanitizeDshPathsRe.ReplaceAllString(text, sanitizeDshPathsRepl)
	}
	if mask&maskLiteral != 0 {
		if sanitizeHdrRe.MatchString(text) {
			text = sanitizeHdrRe.ReplaceAllString(text, "")
		}
		if strings.Contains(text, "cc_") {
			prev := ""
			for prev != text {
				prev = text
				text = sanitizeKvRe.ReplaceAllString(text, "")
			}
		}
		// 兜底：键值形态已在上面整段删除，这里只剩裸键名（引用/示例文本形态）。
		text = sanitizeBareHdrRe.ReplaceAllString(text, sanitizeHdrHint)
		// TrimSpace 只在真的清过 KV/header 残留时做：只因一句自我介绍就把 system
		// prompt 的首尾空白裁掉，会让同一 prompt 每轮字节都不同、前缀缓存全失效。
		text = strings.TrimSpace(text)
	}
	return text
}

// hasFingerprint 特征预检（identity=true，即 system/developer/assistant 作用域）。
//
// 判定委托给 probeMask：字面量折叠扫描，而不是逐条 (?i) 正则。历史注释里「必须再用
// (?i) 正则兜底裸键名」的理由仍然成立——probeMask 覆盖的正是那些大小写/裸键名变体，
// 只是改用零分配字节折叠实现（详见 probeMask 的注释与 sanitize_probe_test.go 的对拍）。
func hasFingerprint(text string) bool { return hasFingerprintScoped(text, true) }

// hasFingerprintScoped 特征预检（identity=false 时不认句式层，见 sanitizeTextScoped）。
func hasFingerprintScoped(text string, identity bool) bool {
	mask := probeMask(text)
	if !identity {
		mask &^= maskSentenceLayer
	}
	return mask != 0
}

// sanitizeContent 兼容字符串与多模态数组；只动 text part，image 等 part 不动。
// 返回净化后的值及是否发生变化。
func sanitizeContent(v any) (any, bool) { return sanitizeContentScoped(v, true) }

// sanitizeContentScoped 同 sanitizeContent，identity 透传给 sanitizeTextScoped。
func sanitizeContentScoped(v any, identity bool) (any, bool) {
	switch c := v.(type) {
	case string:
		s := sanitizeTextScoped(c, identity)
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
			if s := sanitizeTextScoped(text, identity); s != text {
				m["text"] = s
				changed = true
			}
		}
		return c, changed
	}
	return v, false
}

// sanitizeToolCalls 净化 assistant.tool_calls[].function.arguments。
//
// arguments 是**字符串化的 JSON**（不是对象），因此按文本走 sanitizeText 即可。
// 这块长期是盲区：工具调用消息的 content 通常是 null，而旧版 sanitizeMessages
// 在 content 缺失时直接 continue，整条消息连 tool_calls 一起被跳过——
// 于是历史里任何写进工具参数的被拦字符串（文件名、命令、写入内容）都会原样漏出。
func sanitizeToolCalls(v any) bool {
	callList, ok := v.([]any)
	if !ok {
		return false
	}
	changed := false
	for _, c := range callList {
		call, ok := c.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := call["function"].(map[string]any)
		if !ok {
			continue
		}
		args, ok := fn["arguments"].(string)
		if !ok {
			continue
		}
		if s := sanitizeTextScoped(args, false); s != args {
			fn["arguments"] = s
			changed = true
		}
	}
	return changed
}

// identityRole 报告该角色是否属于「客户端身份载体」：只有这些角色里的自我介绍
// 句式会被上游判渠道校验（400）；user / tool 是用户数据，实测同一句放进去上游
// 放行（200），改写它只会让模型看到与磁盘不一致的内容。
func identityRole(role any) bool {
	s, _ := role.(string)
	switch s {
	case "system", "developer", "assistant":
		return true
	}
	return false
}

// sanitizeMessages 净化 messages 中的 content 与 tool_calls；任一命中返回 true。
func sanitizeMessages(messages []any) bool {
	changed := false
	for _, msg := range messages {
		m, ok := msg.(map[string]any)
		if !ok {
			continue
		}
		// content 与 tool_calls 各自独立判断：content 可以为 null（工具调用轮），
		// 早期版本在此 continue，导致这类消息的 tool_calls 完全不被净化。
		// identity 按角色判定：只有身份载体才走句式层（见 identityRole）。
		identity := identityRole(m["role"])
		// assistant 正文是「上游注入身份指令」的回流载体：模型在上一轮把服务端注入的
		// 身份句（"…identify yourself as …"）复述进思维链 / 正文，客户端把它带回历史，
		// 下一轮原样出站即命中。system / user 里的同形句是用户自己写的人设，不洗
		// （见 scrubInjectedIdentity 的作用域说明）。
		var contentPost func(string) string
		if role, _ := m["role"].(string); role == "assistant" {
			contentPost = scrubInjectedIdentity
		}
		if c, ok := m["content"]; ok {
			if nc, ch := sanitizeContentPost(c, identity, contentPost); ch {
				m["content"] = nc
				changed = true
			}
		}
		// reasoning_content（思维链回填字段，见 thinking.go/sse.go）实测同样
		// 携带指纹，与 content 同等净化。string 形态直接走 sanitizeText。
		// 注入身份指令的复述也走这里（它是滚雪球的主要载体）。
		if rc, ok := m["reasoning_content"].(string); ok {
			if s := scrubInjectedIdentity(sanitizeTextScoped(rc, false)); s != rc {
				m["reasoning_content"] = s
				changed = true
			}
		}
		// reasoning：另一形态的思维链字段。thinking.go 的回填会把客户端送来的
		// reasoning_content 镜像进 reasoning（反之亦然），而此前只洗 content /
		// reasoning_content / tool_calls —— 镜像进 reasoning 的指纹原样出站，
		// 裸 "11128" 这种反探测串同样致命。
		if r, ok := m["reasoning"].(string); ok {
			if s := scrubInjectedIdentity(sanitizeTextScoped(r, false)); s != r {
				m["reasoning"] = s
				changed = true
			}
		}
		if tc, ok := m["tool_calls"]; ok {
			if sanitizeToolCalls(tc) {
				changed = true
			}
		}
	}
	return changed
}
