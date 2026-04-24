package helps

import "github.com/tidwall/gjson"

func GetTokenizer(model string) (*TokenizerWrapper, error) {
	return getTokenizer(model)
}

func TokenizerForModel(model string) (*TokenizerWrapper, error) {
	return tokenizerForModel(model)
}

func CountOpenAIChatTokens(enc *TokenizerWrapper, payload []byte) (int64, error) {
	return countOpenAIChatTokens(enc, payload)
}

func CountClaudeChatTokens(enc *TokenizerWrapper, payload []byte) (int64, error) {
	return countClaudeChatTokens(enc, payload)
}

func BuildOpenAIUsageJSON(count int64) []byte {
	return buildOpenAIUsageJSON(count)
}

func CollectOpenAIContent(content gjson.Result, segments *[]string) {
	collectOpenAIContent(content, segments)
}
