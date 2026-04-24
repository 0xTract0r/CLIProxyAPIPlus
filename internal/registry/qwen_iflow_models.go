package registry

var qwenStaticModels = []*ModelInfo{
	{
		ID:                  "qwen3-coder-plus",
		Object:              "model",
		Created:             1753228800,
		OwnedBy:             "qwen",
		Type:                "qwen",
		DisplayName:         "Qwen3 Coder Plus",
		Version:             "3.0",
		Description:         "Advanced code generation and understanding model",
		ContextLength:       32768,
		MaxCompletionTokens: 8192,
		SupportedParameters: []string{"temperature", "top_p", "max_tokens", "stream", "stop"},
	},
	{
		ID:                  "qwen3-coder-flash",
		Object:              "model",
		Created:             1753228800,
		OwnedBy:             "qwen",
		Type:                "qwen",
		DisplayName:         "Qwen3 Coder Flash",
		Version:             "3.0",
		Description:         "Fast code generation model",
		ContextLength:       8192,
		MaxCompletionTokens: 2048,
		SupportedParameters: []string{"temperature", "top_p", "max_tokens", "stream", "stop"},
	},
	{
		ID:                  "coder-model",
		Object:              "model",
		Created:             1771171200,
		OwnedBy:             "qwen",
		Type:                "qwen",
		DisplayName:         "Qwen 3.5 Plus",
		Version:             "3.5",
		Description:         "efficient hybrid model with leading coding performance",
		ContextLength:       1048576,
		MaxCompletionTokens: 65536,
		SupportedParameters: []string{"temperature", "top_p", "max_tokens", "stream", "stop"},
	},
	{
		ID:                  "vision-model",
		Object:              "model",
		Created:             1758672000,
		OwnedBy:             "qwen",
		Type:                "qwen",
		DisplayName:         "Qwen3 Vision Model",
		Version:             "3.0",
		Description:         "Vision model model",
		ContextLength:       32768,
		MaxCompletionTokens: 2048,
		SupportedParameters: []string{"temperature", "top_p", "max_tokens", "stream", "stop"},
	},
}

var iFlowReasoningLevels = &ThinkingSupport{
	Levels: []string{"none", "auto", "minimal", "low", "medium", "high", "xhigh"},
}

var iflowStaticModels = []*ModelInfo{
	{ID: "qwen3-coder-plus", Object: "model", Created: 1753228800, OwnedBy: "iflow", Type: "iflow", DisplayName: "Qwen3-Coder-Plus", Description: "Qwen3 Coder Plus code generation"},
	{ID: "qwen3-max", Object: "model", Created: 1758672000, OwnedBy: "iflow", Type: "iflow", DisplayName: "Qwen3-Max", Description: "Qwen3 flagship model"},
	{ID: "qwen3-vl-plus", Object: "model", Created: 1758672000, OwnedBy: "iflow", Type: "iflow", DisplayName: "Qwen3-VL-Plus", Description: "Qwen3 multimodal vision-language"},
	{ID: "qwen3-max-preview", Object: "model", Created: 1757030400, OwnedBy: "iflow", Type: "iflow", DisplayName: "Qwen3-Max-Preview", Description: "Qwen3 Max preview build", Thinking: iFlowReasoningLevels},
	{ID: "glm-4.6", Object: "model", Created: 1759190400, OwnedBy: "iflow", Type: "iflow", DisplayName: "GLM-4.6", Description: "Zhipu GLM 4.6 general model", Thinking: iFlowReasoningLevels},
	{ID: "kimi-k2", Object: "model", Created: 1752192000, OwnedBy: "iflow", Type: "iflow", DisplayName: "Kimi-K2", Description: "Moonshot Kimi K2 general model"},
	{ID: "deepseek-v3.2", Object: "model", Created: 1759104000, OwnedBy: "iflow", Type: "iflow", DisplayName: "DeepSeek-V3.2-Exp", Description: "DeepSeek V3.2 experimental", Thinking: iFlowReasoningLevels},
	{ID: "deepseek-v3.1", Object: "model", Created: 1756339200, OwnedBy: "iflow", Type: "iflow", DisplayName: "DeepSeek-V3.1-Terminus", Description: "DeepSeek V3.1 Terminus", Thinking: iFlowReasoningLevels},
	{ID: "deepseek-r1", Object: "model", Created: 1737331200, OwnedBy: "iflow", Type: "iflow", DisplayName: "DeepSeek-R1", Description: "DeepSeek reasoning model R1"},
	{ID: "deepseek-v3", Object: "model", Created: 1734307200, OwnedBy: "iflow", Type: "iflow", DisplayName: "DeepSeek-V3-671B", Description: "DeepSeek V3 671B"},
	{ID: "qwen3-32b", Object: "model", Created: 1747094400, OwnedBy: "iflow", Type: "iflow", DisplayName: "Qwen3-32B", Description: "Qwen3 32B"},
	{ID: "qwen3-235b-a22b-thinking-2507", Object: "model", Created: 1753401600, OwnedBy: "iflow", Type: "iflow", DisplayName: "Qwen3-235B-A22B-Thinking", Description: "Qwen3 235B A22B Thinking (2507)"},
	{ID: "qwen3-235b-a22b-instruct", Object: "model", Created: 1753401600, OwnedBy: "iflow", Type: "iflow", DisplayName: "Qwen3-235B-A22B-Instruct", Description: "Qwen3 235B A22B Instruct"},
	{ID: "qwen3-235b", Object: "model", Created: 1753401600, OwnedBy: "iflow", Type: "iflow", DisplayName: "Qwen3-235B-A22B", Description: "Qwen3 235B A22B"},
	{ID: "iflow-rome-30ba3b", Object: "model", Created: 1736899200, OwnedBy: "iflow", Type: "iflow", DisplayName: "iFlow-ROME", Description: "iFlow Rome 30BA3B model"},
}

// GetQwenModels returns the static Qwen model definitions kept by CLIProxyAPIPlus.
func GetQwenModels() []*ModelInfo {
	return cloneModelInfos(qwenStaticModels)
}

// GetIFlowModels returns the static iFlow model definitions kept by CLIProxyAPIPlus.
func GetIFlowModels() []*ModelInfo {
	return cloneModelInfos(iflowStaticModels)
}
