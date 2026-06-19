package helps

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"regexp"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// Codex 出站 cwd / git / CODEX_HOME 路径归一（需求 ⑦-codex，对齐 claude⑦）。
//
// Codex 客户端在出站请求里会泄漏开发机的真实身份路径，与 claude 的
// environment block 同源风险：多名员工共用同一个上游账号时，这些真实路径让
// OpenAI 能把单一账号反关联到多台不同开发机。泄漏点共 5 处：
//
//	#1 header  X-Codex-Turn-Metadata（JSON 字符串）：
//	   workspaces 的 KEY 是真实 cwd；其 value 里 latest_git_commit_hash 是真实
//	   git commit、associated_remote_urls.origin 是真实 git remote。
//	#2 body    client_metadata["x-codex-turn-metadata"]：#1 的逐字 JSON 副本。
//	#3 body    <environment_context> 里的 <cwd> 和
//	   <filesystem><workspace_roots><root>：真实 cwd。
//	#4 body    skills 清单 "(file: <CODEX_HOME>/skills/.system/...)"：真实
//	   CODEX_HOME 前缀（可能不在 /Users|/home 下，例如 /private/tmp/...）。
//	#5 body    "# AGENTS.md instructions for <cwd>" 头：真实 cwd。
//
// 归一目标（每请求只算一次派生值，header 与 body 处处用同一组值，保证一致 +
// 幂等 + 跨账号不同）：
//   - canonical cwd        = AccountCanonicalCwd(auth, apiKey)
//   - canonical CODEX_HOME = /home/agent/codex-home-<hex8>
//   - git commit hash      = 合法 40-hex
//   - git remote           = git@github.com:<hex8>/<hex8>.git
//
// 全部归一都是无条件的（不依赖任何开关），与 claude⑦ 的 cwd 无条件归一一致。
//
// 指纹边界（绝不改动）：<timezone> / <shell> / <current_date>、UA、出站时间、
// Kiro 相关字段一律原样透传。#3/#4/#5 的 body 自由文本只对"探测到的真实
// cwd / 真实 CODEX_HOME"两个已知字面串做精确替换，不对 AGENTS.md / 用户
// prompt 正文做宽泛 /Users|/home 正则 sweep（正文可能含他人合法绝对路径，
// 宽泛 sweep 有误改风险）。

// codexCanonicalValues 是一次请求派生出的全部归一目标值。同一请求里 header 与
// body 共用同一个实例，保证 #1/#2 逐值一致。
type codexCanonicalValues struct {
	// scopeKey 是账号作用域 key，所有派生值的种子。
	scopeKey string
	// cwd 是每账号 canonical 工作目录（复用 claude⑦ 的派生）。
	cwd string
	// codexHome 是每账号 canonical CODEX_HOME。
	codexHome string
	// gitCommit 是每账号派生的合法 40-hex git commit hash。
	gitCommit string
	// gitRemote 是每账号派生的 git remote URL。
	gitRemote string
}

// resolveCodexCanonicalValues 从账号作用域派生出全部归一目标值。结果对同一账号
// 跨请求稳定、跨账号不同，且不混入 server salt（这些值不含秘密，只需稳定可复现）。
func resolveCodexCanonicalValues(auth *cliproxyauth.Auth, apiKey string) codexCanonicalValues {
	scopeKey := ClaudeAccountScopeKey(auth, apiKey)
	return codexCanonicalValues{
		scopeKey:  scopeKey,
		cwd:       AccountCanonicalCwd(auth, apiKey),
		codexHome: canonicalCodexHome(scopeKey),
		gitCommit: canonicalCodexGitCommit(scopeKey),
		gitRemote: canonicalCodexGitRemote(scopeKey),
	}
}

// canonicalCodexHome 派生每账号 canonical CODEX_HOME，复用 canonicalHomeRoot 根，
// 形如 /home/agent/codex-home-<hex8>。
func canonicalCodexHome(scopeKey string) string {
	sum := sha256.Sum256([]byte("cliproxy-canonical-codex-home\x00" + scopeKey))
	id := binary.BigEndian.Uint32(sum[:4])
	return canonicalHomeRoot + "/codex-home-" + uint32ToHex(id)
}

// canonicalCodexGitCommit 派生一个合法的 40-hex git commit hash（取 sha256 前 20
// 字节 → 40 个十六进制字符）。
func canonicalCodexGitCommit(scopeKey string) string {
	sum := sha256.Sum256([]byte("cliproxy-codex-git-commit\x00" + scopeKey))
	return hex.EncodeToString(sum[:20])
}

// canonicalCodexGitRemote 派生 git remote，owner 与 repo 各取一个独立种子的
// sha256 前 4 字节 → hex8，形如 git@github.com:<hex8>/<hex8>.git。
func canonicalCodexGitRemote(scopeKey string) string {
	ownerSum := sha256.Sum256([]byte("cliproxy-codex-git-remote-owner\x00" + scopeKey))
	repoSum := sha256.Sum256([]byte("cliproxy-codex-git-remote-repo\x00" + scopeKey))
	owner := uint32ToHex(binary.BigEndian.Uint32(ownerSum[:4]))
	repo := uint32ToHex(binary.BigEndian.Uint32(repoSum[:4]))
	return "git@github.com:" + owner + "/" + repo + ".git"
}

// codexEnvCwdTagPattern 抓 <cwd>...</cwd>（environment_context #3 用）。group 1 是
// 内部真实 cwd 值。
var codexEnvCwdTagPattern = regexp.MustCompile(`(?s)<cwd>(.*?)</cwd>`)

// codexEnvRootTagPattern 抓 <root>...</root>（workspace_roots #3 用）。
var codexEnvRootTagPattern = regexp.MustCompile(`(?s)<root>(.*?)</root>`)

// codexAgentsHeaderPattern 抓 "AGENTS.md instructions for <cwd>" 头（#5 用），
// 抓到行尾。group 1 是真实 cwd。
var codexAgentsHeaderPattern = regexp.MustCompile(`AGENTS\.md instructions for ([^\n]+)`)

// codexSkillFilePattern 抓 "(file: <X>/skills/.system/..." 反推真实 CODEX_HOME。
// group 1 是 skills 之前的目录前缀（即真实 CODEX_HOME）。只匹配 .system 内置
// skill 路径，避免抓到 cwd 下的 .codex/skills 用户 skill。
var codexSkillFilePattern = regexp.MustCompile(`\(file:\s*(\S+?)/skills/\.system/`)

// NormalizeCodexTurnMetadataHeader 归一一个 turn-metadata header（#1）。header key
// 大小写不敏感取值；归一后写回同一 key。无解析失败即原样返回（不抛错、不丢 header）。
func NormalizeCodexTurnMetadataHeader(headers map[string][]string, headerKey string, auth *cliproxyauth.Auth, apiKey string) {
	if headers == nil {
		return
	}
	// 大小写不敏感定位实际 key。
	actualKey := ""
	for k := range headers {
		if strings.EqualFold(k, headerKey) {
			actualKey = k
			break
		}
	}
	if actualKey == "" {
		return
	}
	values := headers[actualKey]
	if len(values) == 0 {
		return
	}
	vals := resolveCodexCanonicalValues(auth, apiKey)
	for i, v := range values {
		if strings.TrimSpace(v) == "" {
			continue
		}
		values[i] = normalizeCodexTurnMetadataJSON(v, vals)
	}
	headers[actualKey] = values
}

// normalizeCodexTurnMetadataJSON 归一一段 turn-metadata JSON 字符串（#1/#2 共用）：
//   - workspaces 是 object，KEY 是真实 cwd → 改 KEY 为 canonical cwd；
//   - 其 value 里 latest_git_commit_hash → 派生 commit；
//   - associated_remote_urls.origin → 派生 remote。
//
// 只改这三类字段，其余（installation_id / session_id / turn_id 等身份字段由
// 既有 identity-confuse 负责）一律不动。解析失败原样返回。
func normalizeCodexTurnMetadataJSON(raw string, vals codexCanonicalValues) string {
	if !gjson.Valid(raw) {
		return raw
	}
	workspaces := gjson.Get(raw, "workspaces")
	if !workspaces.Exists() || !workspaces.IsObject() {
		return raw
	}

	// 先收集每个 workspace 的归一后子对象（key 改为 canonical cwd）。先删旧 key
	// 再写新 key，避免真实路径残留。
	type wsEntry struct {
		canonicalKey string
		valueJSON    string
	}
	var entries []wsEntry
	var realKeys []string
	workspaces.ForEach(func(key, value gjson.Result) bool {
		realKeys = append(realKeys, key.String())
		ws := value.Raw
		// git commit。
		if gjson.Get(ws, "latest_git_commit_hash").Exists() {
			ws, _ = sjson.Set(ws, "latest_git_commit_hash", vals.gitCommit)
		}
		// git remote origin。
		if gjson.Get(ws, "associated_remote_urls.origin").Exists() {
			ws, _ = sjson.Set(ws, "associated_remote_urls.origin", vals.gitRemote)
		}
		entries = append(entries, wsEntry{canonicalKey: vals.cwd, valueJSON: ws})
		return true
	})

	// 删除所有真实 cwd key。sjson 删除 object key 需要转义点号路径。
	for _, rk := range realKeys {
		raw, _ = sjson.Delete(raw, "workspaces."+escapeSjsonKey(rk))
	}
	// 写回 canonical key。多个真实 workspace 都映射到同一 canonical cwd，重复写
	// 同 key 是幂等的（最后一个生效），符合"每账号单一 canonical cwd"语义。
	for _, e := range entries {
		raw, _ = sjson.SetRaw(raw, "workspaces."+escapeSjsonKey(e.canonicalKey), e.valueJSON)
	}
	return raw
}

// escapeSjsonKey 转义 sjson 路径里的特殊字符（点号、星号、问号），让含 "/" 与
// "." 的真实/canonical 路径能作为单个 object key 处理。sjson 用 "\\" 转义。
func escapeSjsonKey(key string) string {
	replacer := strings.NewReplacer(".", `\.`, "*", `\*`, "?", `\?`)
	return replacer.Replace(key)
}

// NormalizeCodexPaths 归一 codex 出站 body 里的全部路径泄漏点（#2/#3/#4/#5）。
// 无条件执行；解析失败或无匹配时原样返回。
func NormalizeCodexPaths(body []byte, auth *cliproxyauth.Auth, apiKey string) []byte {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return body
	}
	vals := resolveCodexCanonicalValues(auth, apiKey)

	// #2 body client_metadata["x-codex-turn-metadata"]（#1 逐字副本，复用同一
	// 派生值，保证 header/body 跨位置一致）。
	if tm := gjson.GetBytes(body, "client_metadata.x-codex-turn-metadata"); tm.Exists() && strings.TrimSpace(tm.String()) != "" {
		normalized := normalizeCodexTurnMetadataJSON(tm.String(), vals)
		if normalized != tm.String() {
			body, _ = sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata", normalized)
		}
	}

	// #3/#4/#5 在自由文本字段里：instructions（string）+ input[].content[].text。
	body = normalizeCodexBodyText(body, "instructions", vals)
	body = normalizeCodexInputText(body, vals)
	return body
}

// normalizeCodexBodyText 归一一个 string 字段（如 instructions）。
func normalizeCodexBodyText(body []byte, path string, vals codexCanonicalValues) []byte {
	field := gjson.GetBytes(body, path)
	if field.Type != gjson.String {
		return body
	}
	text := field.String()
	rewritten := rewriteCodexText(text, vals)
	if rewritten != text {
		body, _ = sjson.SetBytes(body, path, rewritten)
	}
	return body
}

// normalizeCodexInputText 遍历 input[].content[].text 并归一每段文本。
func normalizeCodexInputText(body []byte, vals codexCanonicalValues) []byte {
	input := gjson.GetBytes(body, "input")
	if !input.Exists() || !input.IsArray() {
		return body
	}
	input.ForEach(func(inKey, item gjson.Result) bool {
		content := item.Get("content")
		if !content.Exists() {
			return true
		}
		basePath := "input." + inKey.String()
		if content.Type == gjson.String {
			text := content.String()
			rewritten := rewriteCodexText(text, vals)
			if rewritten != text {
				body, _ = sjson.SetBytes(body, basePath+".content", rewritten)
			}
			return true
		}
		if content.IsArray() {
			content.ForEach(func(cKey, block gjson.Result) bool {
				t := block.Get("text")
				if t.Type != gjson.String {
					return true
				}
				text := t.String()
				rewritten := rewriteCodexText(text, vals)
				if rewritten != text {
					body, _ = sjson.SetBytes(body, basePath+".content."+cKey.String()+".text", rewritten)
				}
				return true
			})
		}
		return true
	})
	return body
}

// rewriteCodexText 对一段自由文本做 #3/#4/#5 归一：保守地只替换"从文本自身探测到
// 的真实 cwd / 真实 CODEX_HOME"两个字面串，不动 timezone/shell/current_date，也
// 不做宽泛 /Users|/home sweep。
func rewriteCodexText(text string, vals codexCanonicalValues) string {
	if text == "" {
		return text
	}

	// 探测真实 cwd：优先 <cwd>，其次 <root>，再次 AGENTS.md 头。收集到的所有真实
	// cwd 字面串都会被精确替换为 canonical cwd。
	realCwds := collectCodexRealCwds(text)
	// 探测真实 CODEX_HOME：反推自 skills .system 文件路径。
	realHomes := collectCodexRealHomes(text)

	// 先替换 CODEX_HOME（更长前缀，含其下的 cwd 不冲突），再替换 cwd。两者互不
	// 包含（CODEX_HOME 是 /private/tmp/...，cwd 是 /Users/...），顺序不敏感，但
	// 为稳妥先长后短。
	for _, home := range realHomes {
		if home != "" && home != vals.codexHome {
			text = strings.ReplaceAll(text, home, vals.codexHome)
		}
	}
	for _, cwd := range realCwds {
		if cwd != "" && cwd != vals.cwd {
			text = strings.ReplaceAll(text, cwd, vals.cwd)
		}
	}
	return text
}

// collectCodexRealCwds 从文本里探测真实 cwd 字面串（去重）。
func collectCodexRealCwds(text string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	for _, m := range codexEnvCwdTagPattern.FindAllStringSubmatch(text, -1) {
		add(m[1])
	}
	for _, m := range codexEnvRootTagPattern.FindAllStringSubmatch(text, -1) {
		add(m[1])
	}
	for _, m := range codexAgentsHeaderPattern.FindAllStringSubmatch(text, -1) {
		add(m[1])
	}
	return out
}

// collectCodexRealHomes 从 skills .system 文件路径反推真实 CODEX_HOME（去重）。
func collectCodexRealHomes(text string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range codexSkillFilePattern.FindAllStringSubmatch(text, -1) {
		s := strings.TrimSpace(m[1])
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
