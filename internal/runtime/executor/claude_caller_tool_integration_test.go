package executor

import (
	"bytes"
	"fmt"
	"github.com/tidwall/gjson"
	"sync"
	"testing"
)

func TestClaudeContinuationToolAliasesIsolateCallersAndCopies(t *testing.T) {
	e := &ClaudeExecutor{}
	payload := []byte(`{"thread":{"type":"create"},"tools":[{"name":"Bash","input_schema":{"type":"object"}},{"name":"glob","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"Bash glob remain prose"}]}`)
	next := []byte(`{"thread":{"type":"continue","previous_message_id":"same-upstream-id"}}`)
	tables := make([]map[string]string, 2)
	for i, caller := range []string{"caller-one", "caller-two"} {
		opts := claudeMCPAliasOptions{secret: caller}
		a, m, err := e.prepareClaudeOAuthToolNamesForRequest(payload, opts)
		if err != nil {
			t.Fatal(err)
		}
		b, m2, err := e.prepareClaudeOAuthToolNamesForRequest(payload, opts)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(a, b) || len(m2) != len(m) {
			t.Fatal("same caller aliases changed")
		}
		if gjson.GetBytes(a, "messages.0.content").String() != "Bash glob remain prose" {
			t.Fatal("prose changed")
		}
		e.rememberClaudeOAuthToolAliases(payload, m, "same-upstream-id", opts)
		tables[i] = m
	}
	for alias := range tables[0] {
		if _, ok := tables[1][alias]; ok {
			t.Fatal("different callers shared a wire symbol")
		}
	}
	var wg sync.WaitGroup
	for n := 0; n < 32; n++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			caller := []string{"caller-one", "caller-two"}[i%2]
			_, m, err := e.prepareClaudeOAuthToolNamesForRequest(next, claudeMCPAliasOptions{secret: caller})
			if err != nil {
				t.Error(err)
				return
			}
			for wire, want := range tables[i%2] {
				if m[wire] != want {
					t.Errorf("caller table crossed: got=%v want=%v", m, tables[i%2])
				}
				restored, err := restoreClaudeOAuthToolNamesFromResponse([]byte(fmt.Sprintf(`{"content":[{"type":"tool_use","name":%q,"input":{}}]}`, wire)), m)
				if err != nil || gjson.GetBytes(restored, "content.0.name").String() != want {
					t.Error("exact case roundtrip failed")
				}
			}
			// Mutating a returned map must not mutate the cached or concurrent table.
			for wire := range m {
				m[wire] = "corrupted local copy"
			}
		}(n)
	}
	wg.Wait()
	if _, _, err := e.prepareClaudeOAuthToolNamesForRequest(next, claudeMCPAliasOptions{secret: "unknown-caller"}); err == nil {
		t.Fatal("unknown caller loaded another caller continuation")
	}
	for wire := range tables[1] {
		line := []byte(fmt.Sprintf(`data: {"type":"content_block_start","content_block":{"type":"tool_use","name":%q}}`, wire))
		restored, err := restoreClaudeOAuthToolNamesFromStreamLine(line, tables[0])
		if err != nil || !bytes.Equal(line, restored) {
			t.Fatal("foreign caller symbol rewritten")
		}
	}
}
