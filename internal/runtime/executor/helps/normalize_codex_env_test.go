package helps

import (
	"encoding/json"
	"encoding/xml"
	"regexp"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// Codex 出站 cwd/git/CODEX_HOME 路径归一测试（需求 ⑦-codex）。fixture 取自真实
// codex 出站抓包脱敏样本（/tmp/codex_cwd_sample_redacted.txt）：header turn-metadata
// JSON、body client_metadata 副本、environment_context XML、skills CODEX_HOME 字面、
// AGENTS.md 头。

const (
	// codexRealCwd 是样本里的真实 cwd（#1 header workspaces KEY / #3 env ctx /
	// #5 AGENTS 头）。
	codexRealCwd = "/Users/corylin/Project/ai/cliproxy-stack/.worktrees/anticorr-hardening"
	// codexRealGitCommit 是样本里的真实 git commit（#1 latest_git_commit_hash）。
	codexRealGitCommit = "e2b18565b7d477866f1bb502d3c017f129f4f03d"
	// codexRealGitRemote 是样本里的真实 git remote（#1 associated_remote_urls.origin）。
	codexRealGitRemote = "git@github.com:0xTract0r/cliproxy-stack.git"
	// codexRealHome 是样本里的真实 CODEX_HOME（#4 skills .system 前缀，刻意不在
	// /Users|/home 下，验证字面探测而非前缀白名单）。
	codexRealHome = "/private/tmp/codex_cwd_home_1781863485"
)

// codexTurnMetadataFixture 复刻样本里的 turn-metadata JSON（header #1 与 body
// client_metadata 副本 #2 共用同一字符串）。
const codexTurnMetadataFixture = `{"installation_id":"6a9aea66-9c05-4a26-8c27-038f82fabaed","session_id":"019edf57-9c1e-78b3-860a-c6ff641bdeac","thread_id":"019edf57-9c1e-78b3-860a-c6ff641bdeac","turn_id":"019edf57-9c73-7363-8ca0-8bb8ef833483","window_id":"019edf57-9c1e-78b3-860a-c6ff641bdeac:0","request_kind":"turn","sandbox":"none","workspaces":{"/Users/corylin/Project/ai/cliproxy-stack/.worktrees/anticorr-hardening":{"associated_remote_urls":{"origin":"git@github.com:0xTract0r/cliproxy-stack.git"},"latest_git_commit_hash":"e2b18565b7d477866f1bb502d3c017f129f4f03d","has_changes":true}},"turn_started_at_unix_ms":1781863521404}`

// codexEnvContextFixture 复刻样本里的 environment_context（含指纹边界字段
// shell/current_date/timezone，必须原样不动）。
const codexEnvContextFixture = "<environment_context>\n  <cwd>/Users/corylin/Project/ai/cliproxy-stack/.worktrees/anticorr-hardening</cwd>\n  <shell>zsh</shell>\n  <current_date>2026-06-19</current_date>\n  <timezone>America/Los_Angeles</timezone>\n  <filesystem><workspace_roots><root>/Users/corylin/Project/ai/cliproxy-stack/.worktrees/anticorr-hardening</root></workspace_roots><permission_profile type=\"disabled\"><file_system type=\"unrestricted\" /></permission_profile></filesystem>\n</environment_context>"

// codexSkillsFixture 复刻样本里的 skills .system 文件清单（#4 反推 CODEX_HOME）。
const codexSkillsFixture = "Available skills:\n- imagegen (file: /private/tmp/codex_cwd_home_1781863485/skills/.system/imagegen/SKILL.md)\n- openai-docs (file: /private/tmp/codex_cwd_home_1781863485/skills/.system/openai-docs/SKILL.md)"

// codexAgentsFixture 复刻样本里的 AGENTS.md 头（#5）。
const codexAgentsFixture = "# AGENTS.md instructions for /Users/corylin/Project/ai/cliproxy-stack/.worktrees/anticorr-hardening\nFollow these conventions."

func codexAuth(fileName string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{ProxyURL: "direct", FileName: fileName}
}

// codexBodyFixture 组一份覆盖 #2/#3/#4/#5 的完整 body：client_metadata 副本 +
// instructions（含 skills + env ctx + AGENTS 头）+ input[].content[].text。
func codexBodyFixture(t *testing.T) []byte {
	t.Helper()
	instructions := codexSkillsFixture + "\n\n" + codexAgentsFixture
	inputText := "User request.\n\n" + codexEnvContextFixture
	root := map[string]any{
		"model":        "gpt-5",
		"instructions": instructions,
		"client_metadata": map[string]any{
			"x-codex-turn-metadata": codexTurnMetadataFixture,
		},
		"input": []any{
			map[string]any{
				"type": "message",
				"role": "user",
				"content": []any{
					map[string]any{"type": "input_text", "text": inputText},
				},
			},
		},
	}
	raw, err := json.Marshal(root)
	if err != nil {
		t.Fatalf("marshal body fixture: %v", err)
	}
	return raw
}

// TestNormalizeCodexPaths_RealValuesGone：归一后真实 cwd 字面完全消失、canonical 出现。
func TestNormalizeCodexPaths_RealValuesGone(t *testing.T) {
	auth := codexAuth("codex-a.json")
	out := NormalizeCodexPaths(codexBodyFixture(t), auth, "key-1")
	canonical := AccountCanonicalCwd(auth, "key-1")

	if strings.Contains(string(out), codexRealCwd) {
		t.Fatalf("real cwd 仍残留:\n%s", out)
	}
	if !strings.Contains(string(out), canonical) {
		t.Fatalf("缺 canonical cwd %q:\n%s", canonical, out)
	}
}

// TestNormalizeCodexPaths_GitFieldsDerived：真实 git hash/远端消失，派生值格式合法。
func TestNormalizeCodexPaths_GitFieldsDerived(t *testing.T) {
	auth := codexAuth("codex-a.json")
	out := NormalizeCodexPaths(codexBodyFixture(t), auth, "key-1")

	if strings.Contains(string(out), codexRealGitCommit) {
		t.Fatalf("真实 git commit 仍残留:\n%s", out)
	}
	if strings.Contains(string(out), codexRealGitRemote) {
		t.Fatalf("真实 git remote 仍残留:\n%s", out)
	}

	tm := gjson.GetBytes(out, "client_metadata.x-codex-turn-metadata").String()
	ws := gjson.Get(tm, "workspaces").Map()
	if len(ws) != 1 {
		t.Fatalf("expected single canonical workspace, got %d: %s", len(ws), tm)
	}
	var commit, remote string
	for _, v := range ws {
		commit = v.Get("latest_git_commit_hash").String()
		remote = v.Get("associated_remote_urls.origin").String()
	}
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(commit) {
		t.Fatalf("派生 git commit 非 40-hex: %q", commit)
	}
	if !regexp.MustCompile(`^git@github\.com:[0-9a-f]{8}/[0-9a-f]{8}\.git$`).MatchString(remote) {
		t.Fatalf("派生 git remote 格式非法: %q", remote)
	}
}

// TestNormalizeCodexPaths_FingerprintBoundaryUntouched：指纹边界字段原样不变。
func TestNormalizeCodexPaths_FingerprintBoundaryUntouched(t *testing.T) {
	auth := codexAuth("codex-a.json")
	out := NormalizeCodexPaths(codexBodyFixture(t), auth, "key-1")
	// 取解码后的 input 文本断言（JSON 内 "<" 被转义成 <，必须看解码值而非
	// 裸字节）。
	got := gjson.GetBytes(out, "input.0.content.0.text").String()

	for _, marker := range []string{
		"<shell>zsh</shell>",
		"<current_date>2026-06-19</current_date>",
		"<timezone>America/Los_Angeles</timezone>",
	} {
		if !strings.Contains(got, marker) {
			t.Fatalf("指纹边界字段被改动，缺 %q:\n%s", marker, got)
		}
	}
}

// TestNormalizeCodexPaths_StillValidJSONAndXML：归一后 turn-metadata JSON 合法、
// environment_context XML 标签闭合。
func TestNormalizeCodexPaths_StillValidJSONAndXML(t *testing.T) {
	auth := codexAuth("codex-a.json")
	out := NormalizeCodexPaths(codexBodyFixture(t), auth, "key-1")

	if !gjson.ValidBytes(out) {
		t.Fatalf("归一后 body 非合法 JSON:\n%s", out)
	}
	tm := gjson.GetBytes(out, "client_metadata.x-codex-turn-metadata").String()
	if !gjson.Valid(tm) {
		t.Fatalf("归一后 turn-metadata 非合法 JSON: %s", tm)
	}

	inputText := gjson.GetBytes(out, "input.0.content.0.text").String()
	start := strings.Index(inputText, "<environment_context>")
	end := strings.Index(inputText, "</environment_context>")
	if start < 0 || end < 0 {
		t.Fatalf("缺 environment_context:\n%s", inputText)
	}
	xmlSeg := inputText[start : end+len("</environment_context>")]
	if err := xml.Unmarshal([]byte(xmlSeg), new(struct{})); err != nil {
		t.Fatalf("environment_context XML 标签未闭合: %v\n%s", err, xmlSeg)
	}
}

// TestNormalizeCodexPaths_Idempotent：连跑两次 byte-equal。
func TestNormalizeCodexPaths_Idempotent(t *testing.T) {
	auth := codexAuth("codex-a.json")
	once := NormalizeCodexPaths(codexBodyFixture(t), auth, "key-1")
	twice := NormalizeCodexPaths(append([]byte(nil), once...), auth, "key-1")
	if string(once) != string(twice) {
		t.Fatalf("非幂等:\n第一次=%s\n第二次=%s", once, twice)
	}
}

// TestNormalizeCodexPaths_StablePerAccountDistinctAcrossAccounts：同账号稳定、跨账号不同。
func TestNormalizeCodexPaths_StablePerAccountDistinctAcrossAccounts(t *testing.T) {
	authA := codexAuth("codex-a.json")
	authB := codexAuth("codex-b.json")

	a1 := NormalizeCodexPaths(codexBodyFixture(t), authA, "key-1")
	a2 := NormalizeCodexPaths(codexBodyFixture(t), authA, "key-1")
	b1 := NormalizeCodexPaths(codexBodyFixture(t), authB, "key-1")

	if string(a1) != string(a2) {
		t.Fatalf("同账号不稳定:\n%s\n%s", a1, a2)
	}
	if string(a1) == string(b1) {
		t.Fatalf("跨账号 canonical 值相同，未按账号区分:\n%s", a1)
	}
	if AccountCanonicalCwd(authA, "key-1") == AccountCanonicalCwd(authB, "key-1") {
		t.Fatalf("跨账号 canonical cwd 相同")
	}
}

// TestNormalizeCodexPaths_CodexHomeLiteralProbe：CODEX_HOME 字面探测+替换生效，
// 即便它不在 /Users|/home 前缀下。
func TestNormalizeCodexPaths_CodexHomeLiteralProbe(t *testing.T) {
	auth := codexAuth("codex-a.json")
	out := NormalizeCodexPaths(codexBodyFixture(t), auth, "key-1")

	if strings.Contains(string(out), codexRealHome) {
		t.Fatalf("真实 CODEX_HOME 仍残留:\n%s", out)
	}
	scopeKey := ClaudeAccountScopeKey(auth, "key-1")
	canonicalHome := canonicalCodexHome(scopeKey)
	if !strings.Contains(string(out), canonicalHome) {
		t.Fatalf("缺 canonical CODEX_HOME %q:\n%s", canonicalHome, out)
	}
	if !strings.HasPrefix(canonicalHome, canonicalHomeRoot+"/codex-home-") {
		t.Fatalf("canonical CODEX_HOME 格式非法: %q", canonicalHome)
	}
}

// TestNormalizeCodexTurnMetadataHeader_ConsistentWithBody：header 与 body
// client_metadata 副本逐值一致（git commit / remote / canonical cwd KEY）。
func TestNormalizeCodexTurnMetadataHeader_ConsistentWithBody(t *testing.T) {
	auth := codexAuth("codex-a.json")

	// header 归一。
	headers := map[string][]string{
		"X-Codex-Turn-Metadata": {codexTurnMetadataFixture},
	}
	NormalizeCodexTurnMetadataHeader(headers, "x-codex-turn-metadata", auth, "key-1")
	headerTM := headers["X-Codex-Turn-Metadata"][0]

	// body 归一。
	out := NormalizeCodexPaths(codexBodyFixture(t), auth, "key-1")
	bodyTM := gjson.GetBytes(out, "client_metadata.x-codex-turn-metadata").String()

	headerWS := gjson.Get(headerTM, "workspaces").Map()
	bodyWS := gjson.Get(bodyTM, "workspaces").Map()
	if len(headerWS) != 1 || len(bodyWS) != 1 {
		t.Fatalf("workspaces 数量异常: header=%d body=%d", len(headerWS), len(bodyWS))
	}

	canonical := AccountCanonicalCwd(auth, "key-1")
	for _, m := range []map[string]gjson.Result{headerWS, bodyWS} {
		if _, ok := m[canonical]; !ok {
			t.Fatalf("workspaces KEY 非 canonical cwd %q: %v", canonical, m)
		}
	}

	hCommit := headerWS[canonical].Get("latest_git_commit_hash").String()
	bCommit := bodyWS[canonical].Get("latest_git_commit_hash").String()
	hRemote := headerWS[canonical].Get("associated_remote_urls.origin").String()
	bRemote := bodyWS[canonical].Get("associated_remote_urls.origin").String()

	if hCommit != bCommit {
		t.Fatalf("header/body git commit 不一致: %q vs %q", hCommit, bCommit)
	}
	if hRemote != bRemote {
		t.Fatalf("header/body git remote 不一致: %q vs %q", hRemote, bRemote)
	}
	if strings.Contains(headerTM, codexRealCwd) || strings.Contains(headerTM, codexRealGitCommit) || strings.Contains(headerTM, codexRealGitRemote) {
		t.Fatalf("header 真实值仍残留: %s", headerTM)
	}
}
