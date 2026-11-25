package executor

import (
	"bytes"
	"context"
	"strings"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// The public provider adapters do not have an authorization-bound replay key.
// Preserve request bodies and provider response handling without local replay.
type antigravityReasoningReplayScope struct{}

func (antigravityReasoningReplayScope) valid() bool { return false }

func antigravityUsesReasoningReplayCache(string) bool { return false }

func prepareGeminiTopLevelReasoningReplayPayloadDoc(context.Context, string, cliproxyexecutor.Request, cliproxyexecutor.Options, *ojson.Doc) (antigravityReasoningReplayScope, []byte, bool, error) {
	return antigravityReasoningReplayScope{}, nil, false, nil
}

func prepareGeminiTopLevelCountTokensReasoningReplayPayloadDoc(context.Context, string, cliproxyexecutor.Request, cliproxyexecutor.Options, *ojson.Doc) (bool, error) {
	return false, nil
}

func clearAntigravityReasoningReplayOnInvalidSignature(context.Context, antigravityReasoningReplayScope, int, []byte) error {
	return nil
}

func logAntigravityReasoningReplayDegraded(antigravityReasoningReplayScope, string, error) {}
func cacheAntigravityReasoningReplayFromResponse(context.Context, antigravityReasoningReplayScope, []byte, []byte) {
}

// The legacy adapter hooks remain for call-site compatibility. No accumulator
// is instantiated, and none of these methods retain request or response data.
type antigravityReasoningReplayAccumulator struct{ terminal bool }

func newAntigravityReasoningReplayAccumulator(antigravityReasoningReplayScope, []byte) *antigravityReasoningReplayAccumulator {
	return nil
}
func (*antigravityReasoningReplayAccumulator) ObserveSSELine([]byte)         {}
func (*antigravityReasoningReplayAccumulator) observeResponsePayload([]byte) {}
func (*antigravityReasoningReplayAccumulator) Commit(context.Context)        {}

// Detect an already translated, client-visible completion event.
func antigravityStreamChunksHaveTerminalCommitEvent(chunks [][]byte) bool {
	for _, chunk := range chunks {
		for _, line := range bytes.Split(chunk, []byte("\n")) {
			payload := helps.JSONPayload(line)
			if len(payload) == 0 {
				continue
			}
			switch ojson.GetBytesNoCopy(payload, "type").String() {
			case "response.completed", "response.incomplete", "response.done", "message_stop":
				return true
			}
			if ojson.GetBytesNoCopy(payload, "event_type").String() == "interaction.completed" {
				return true
			}
			for _, path := range []string{
				"choices.0.finish_reason",
				"response.choices.0.finish_reason",
				"candidates.0.finishReason",
				"response.candidates.0.finishReason",
			} {
				if finishReason := ojson.GetBytesNoCopy(payload, path); finishReason.Exists() && strings.TrimSpace(finishReason.String()) != "" {
					return true
				}
			}
		}
	}
	return false
}
