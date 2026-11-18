package prompt

import (
	"bytes"
	"encoding/json"
)

// roleProbe 只解出 role 字段：用来做"首条消息角色"的廉价探测。
type roleProbe struct {
	Role string `json:"role"`
}

// FirstMessageRole 在不解析整棵 JSON 树的前提下取 messages[0].role。
//
// 为什么要这个：EnsureSystemFirst 在每个出站请求上都要跑，而它的绝大多数调用
// （代理类客户端恒以 system 开头）只需要回答"首条是不是 system"这一个问题。
// 用 json.Unmarshal 回答它意味着把 100~500KB 请求体整棵解成 map + 拷贝全部历史
// 消息，而这个 body 在下游 prepareBody 里本来还要再解析一次——白白多一遍。
//
// 这里用 json.Decoder 的流式 token：只扫到 messages 数组的第一个对象就停，
// 之前的兄弟键（tools 之类可能很大）用 skipValue 无分配跳过，读到首条消息
// 的 role 立即返回。命中 fast path 时整条请求体一个字节都不复制。
//
// 返回 ("", false) 表示"读不出来"（坏 JSON / 无 messages / 首条不是对象）——
// 调用方必须把它当成"不确定，走完整解析"，不能当成"没有 system"。
func FirstMessageRole(body []byte) (string, bool) {
	if len(body) == 0 {
		return "", false
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	tok, err := dec.Token()
	if err != nil {
		return "", false
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return "", false
	}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return "", false
		}
		key, _ := keyTok.(string)
		if key != "messages" {
			if err := skipValue(dec); err != nil {
				return "", false
			}
			continue
		}
		arrTok, err := dec.Token()
		if err != nil {
			return "", false
		}
		if d, ok := arrTok.(json.Delim); !ok || d != '[' {
			return "", false // messages 不是数组：结构异常，交给完整解析
		}
		if !dec.More() {
			return "", false // 空数组：没有"首条"
		}
		var p roleProbe
		if err := dec.Decode(&p); err != nil {
			return "", false // 首条不是对象
		}
		if p.Role == "" {
			return "", false // 首条没有 role：同样交给完整解析
		}
		return p.Role, true
	}
	return "", false
}

// skipValue 消费一个完整的 JSON 值（标量 / 对象 / 数组），不做任何分配。
//
// 只用于"这个键我们不关心"的场景：逐 token 推进并按分隔符深度配平，
// 遇到标量时第一个 token 就是它本身，深度保持 0 立即返回。
func skipValue(dec *json.Decoder) error {
	depth := 0
	for {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
		}
		if depth <= 0 {
			return nil
		}
	}
}
