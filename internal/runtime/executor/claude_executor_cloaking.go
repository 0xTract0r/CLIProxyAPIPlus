package executor

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	log "github.com/sirupsen/logrus"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

func isValidClaudeCacheControl(cc ojson.Result) bool {
	if !cc.Exists() || cc.Type == ojson.Null || !cc.IsObject() {
		return false
	}
	typ := cc.Get("type")
	return typ.Type == ojson.String && typ.String() == "ephemeral"
}
func buildTextBlock(text string, cacheControl *claudeCacheControl) string {
	block := `{"type":"text","text":` + marshalJSONStringWithoutHTMLEscape(text)
	if cacheControl != nil && cacheControl.Type != "" {
		block += `,"cache_control":{"type":` + marshalJSONStringWithoutHTMLEscape(cacheControl.Type)
		if cacheControl.TTL != "" {
			block += `,"ttl":` + marshalJSONStringWithoutHTMLEscape(cacheControl.TTL)
		}
		if cacheControl.Scope != "" {
			block += `,"scope":` + marshalJSONStringWithoutHTMLEscape(cacheControl.Scope)
		}
		block += "}"
	}
	return block + "}"
}
func marshalJSONStringWithoutHTMLEscape(value string) string {
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(value)
	return strings.TrimSuffix(encoded.String(), "\n")
}

type claudeCacheControl struct {
	Type  string `json:"type"`
	TTL   string `json:"ttl,omitempty"`
	Scope string `json:"scope,omitempty"`
}

var defaultClaudeCacheControl = claudeCacheControl{
	Type: "ephemeral",
}

const claudeCacheControlTTL1h = "1h"

func ensureCacheControl(payload []byte) []byte {
	doc := ojson.NewDoc(payload)
	if !ensureCacheControlDoc(doc) {
		return payload
	}
	return doc.Bytes()
}

func ensureCacheControlDoc(doc *ojson.Doc) bool {
	if doc == nil {
		return false
	}
	changed, materializedInMessages := materializeTopLevelCacheControlDoc(doc)
	if !claudeDocHasCacheableSystem(doc) {
		if injectToolsCacheControlDoc(doc) {
			changed = true
		}
	}
	if injectSystemCacheControlDoc(doc) {
		changed = true
	}
	if !materializedInMessages && injectMessagesCacheControlDoc(doc) {
		changed = true
	}
	return changed
}

func isBareEphemeralClaudeCacheControl(cc ojson.Result) bool {
	if !isValidClaudeCacheControl(cc) {
		return false
	}
	return len(cc.Map()) == 1
}

func buildTextBlockWithRawCacheControl(text, rawCC string) string {
	if rawCC != "" {
		return `{"type":"text","text":` + marshalJSONStringWithoutHTMLEscape(text) + `,"cache_control":` + rawCC + `}`
	}
	return buildTextBlock(text, nil)
}

func applyTopLevelCacheControlToBlockDoc(doc *ojson.Doc, blockPath string, topCC ojson.Result) bool {
	ccPath := blockPath + ".cache_control"
	existingCC := doc.Get(ccPath)
	if !isValidClaudeCacheControl(existingCC) || (isBareEphemeralClaudeCacheControl(existingCC) && !isBareEphemeralClaudeCacheControl(topCC)) {
		if errSet := doc.SetRawBytes(ccPath, []byte(topCC.Raw)); errSet != nil {
			return false
		}
	}
	return doc.Delete("cache_control") == nil
}

func materializeTopLevelCacheControlDoc(doc *ojson.Doc) (bool, bool) {
	if doc == nil || doc.Len() == 0 {
		return false, false
	}
	topCC := doc.Get("cache_control")
	if !isValidClaudeCacheControl(topCC) {
		return false, false
	}

	var scanner claudeStreamDocScanner
	scanner.scan(doc, false, false)
	if scanner.messagesIsArray && scanner.lastMessageIndex >= 0 {
		if scanner.lastEligibleIndex >= 0 && scanner.lastMsgRole == "system" && scanner.lastMsgContentIsNonEmptyString {
			contentPath := fmt.Sprintf("messages.%d.content", scanner.lastMessageIndex)
			finalContent := doc.Get(contentPath)
			if finalContent.Type == ojson.String && strings.TrimSpace(finalContent.String()) != "" {
				newContent := "[" + buildTextBlockWithRawCacheControl(finalContent.String(), topCC.Raw) + "]"
				if doc.SetRawBytes(contentPath, []byte(newContent)) == nil && doc.Delete("cache_control") == nil {
					return true, true
				}
			}
		}
		if scanner.lastEligibleIndex >= 0 {
			if scanner.lastEligibleContentIsArray && scanner.lastEligibleTargetBlockIndex >= 0 {
				blockPath := fmt.Sprintf("messages.%d.content.%d", scanner.lastEligibleIndex, scanner.lastEligibleTargetBlockIndex)
				if applyTopLevelCacheControlToBlockDoc(doc, blockPath, topCC) {
					return true, true
				}
			}
			if scanner.lastEligibleContentIsNonEmptyStr {
				contentPath := fmt.Sprintf("messages.%d.content", scanner.lastEligibleIndex)
				contentRes := doc.Get(contentPath)
				if contentRes.Type == ojson.String && strings.TrimSpace(contentRes.String()) != "" {
					newContent := "[" + buildTextBlockWithRawCacheControl(contentRes.String(), topCC.Raw) + "]"
					if doc.SetRawBytes(contentPath, []byte(newContent)) == nil && doc.Delete("cache_control") == nil {
						return true, true
					}
				}
			}
		}
	}

	system := doc.Get("system")
	if system.IsArray() {
		if lastSysIdx := lastCacheableClaudeSystemIndex(system); lastSysIdx >= 0 {
			return applyTopLevelCacheControlToBlockDoc(doc, fmt.Sprintf("system.%d", lastSysIdx), topCC), false
		}
	} else if system.Type == ojson.String && strings.TrimSpace(system.String()) != "" {
		newSystem := "[" + buildTextBlockWithRawCacheControl(system.String(), topCC.Raw) + "]"
		if doc.SetRawBytes("system", []byte(newSystem)) == nil {
			return doc.Delete("cache_control") == nil, false
		}
	}

	tools := doc.Get("tools")
	if tools.IsArray() {
		lastEligibleToolIndex := -1
		idx := 0
		tools.ForEach(func(_, tool ojson.Result) bool {
			if !tool.Get("defer_loading").Bool() {
				lastEligibleToolIndex = idx
			}
			idx++
			return true
		})
		if lastEligibleToolIndex >= 0 {
			return applyTopLevelCacheControlToBlockDoc(doc, fmt.Sprintf("tools.%d", lastEligibleToolIndex), topCC), false
		}
	}

	return false, false
}

func claudePayloadHasCacheableSystem(payload []byte) bool {
	return claudeSystemHasCacheableBlocks(ojson.GetBytesNoCopy(payload, "system"))
}

func claudeDocHasCacheableSystem(doc *ojson.Doc) bool {
	if doc == nil {
		return false
	}
	return claudeSystemHasCacheableBlocks(doc.Get("system"))
}

func claudeSystemHasCacheableBlocks(system ojson.Result) bool {
	switch {
	case !system.Exists():
		return false
	case system.IsArray():
		return lastCacheableClaudeSystemIndex(system) >= 0
	case system.Type == ojson.String:
		return strings.TrimSpace(system.String()) != ""
	default:
		return false
	}
}

func claudeSystemBlockCanHostCacheControl(item ojson.Result) bool {
	if !item.IsObject() {
		return false
	}
	if typ := item.Get("type").String(); typ != "" && typ != "text" {
		return false
	}
	return strings.TrimSpace(item.Get("text").String()) != ""
}

func lastCacheableClaudeSystemIndex(system ojson.Result) int {
	if !system.IsArray() {
		return -1
	}
	lastEligible := -1
	system.ForEach(func(index, item ojson.Result) bool {
		if claudeSystemBlockCanHostCacheControl(item) {
			lastEligible = int(index.Int())
		}
		return true
	})
	return lastEligible
}

func upgradeClaudeCacheControlTTL(payload []byte, ttl string) []byte {
	if ttl == "" || len(payload) == 0 || !ojson.ValidBytes(payload) {
		return payload
	}
	doc := ojson.NewDoc(payload)
	if !upgradeClaudeCacheControlTTLDoc(doc, ttl) {
		return payload
	}
	return doc.Bytes()
}

func claudeCacheControlPath(path string) string {
	if path == "" {
		return "cache_control"
	}
	return path + ".cache_control"
}

func claudeCacheControlTTLPath(path string) string {
	if path == "" {
		return "cache_control.ttl"
	}
	return path + ".cache_control.ttl"
}

func upgradeClaudeCacheControlTTLDoc(doc *ojson.Doc, ttl string) bool {
	if ttl == "" || doc == nil || doc.Len() == 0 {
		return false
	}
	changed := false
	forEachClaudeCacheControlPathDoc(doc, func(path string, cacheControl ojson.Result) bool {
		if !cacheControl.IsObject() || cacheControl.Get("ttl").Exists() {
			return true
		}
		blockType := cacheControl.Get("type")
		if blockType.Type != ojson.String {
			return true
		}
		// Rebuild the object so the native {type, ttl, scope} key order survives
		// instead of appending ttl after a caller-supplied scope.
		upgraded := `{"type":` + marshalJSONStringWithoutHTMLEscape(blockType.String()) +
			`,"ttl":` + marshalJSONStringWithoutHTMLEscape(ttl)
		if scope := cacheControl.Get("scope"); scope.Exists() {
			upgraded += `,"scope":` + scope.Raw
		}
		upgraded += "}"
		if errSet := doc.SetRawBytes(claudeCacheControlPath(path), []byte(upgraded)); errSet == nil {
			changed = true
		}
		return true
	})
	return changed
}

func stripClaudeCacheControlTTL(payload []byte) []byte {
	if len(payload) == 0 || !ojson.ValidBytes(payload) {
		return payload
	}
	doc := ojson.NewDoc(payload)
	if !stripClaudeCacheControlTTLDoc(doc) {
		return payload
	}
	return doc.Bytes()
}

func stripClaudeCacheControlTTLDoc(doc *ojson.Doc) bool {
	if doc == nil || doc.Len() == 0 {
		return false
	}
	changed := false
	forEachClaudeCacheControlPathDoc(doc, func(path string, cacheControl ojson.Result) bool {
		if !cacheControl.IsObject() || !cacheControl.Get("ttl").Exists() {
			return true
		}
		if errDel := doc.Delete(claudeCacheControlTTLPath(path)); errDel == nil {
			changed = true
		}
		return true
	})
	return changed
}

var errStopClaudeScan = errors.New("stop claude scan")

const (
	claudeSecNone uint8 = iota
	claudeSecTools
	claudeSecSystem
	claudeSecMessages
	claudeSecTopCacheControl
)

const (
	claudeK3None uint8 = iota
	claudeK3CacheControl
	claudeK3Role
	claudeK3Content
)

const (
	claudeK5None uint8 = iota
	claudeK5Type
	claudeK5Text
	claudeK5CacheControl
)

const (
	claudeScanTokNone uint8 = iota
	claudeScanTokString
	claudeScanTokPrimitive
)

const (
	claudeStrIgnore uint8 = iota
	claudeStrCapShort
	claudeStrCheckNonSpace
)

type claudeScannedCC struct {
	idx1 int
	idx2 int
	raw  []byte
}

type claudeStreamDocScanner struct {
	collectCC       bool
	stopOnFirstUser bool

	depth      int
	isObj      [64]bool
	inKey      [64]bool
	topSection uint8
	elemIdx    int
	lvl3Key    uint8
	blockIdx   int
	lvl5Key    uint8

	tok            uint8
	strEsc         uint8
	uVal           uint16
	strIsKey       bool
	strTarget      uint8
	strBuf         [256]byte
	strExtra       []byte
	strLen         int
	strHasEsc      bool
	strHasNonSpace bool
	utfBuf         [4]byte
	utfLen         uint8

	captureActive bool
	captureDepth  int
	capStart      int
	ccBuf         [256]byte
	ccExtra       []byte
	ccLen         int

	ccArena     [1024]byte
	ccArenaUsed int
	toolCC      []claudeScannedCC
	toolCCBuf   [8]claudeScannedCC
	sysCC       []claudeScannedCC
	sysCCBuf    [8]claudeScannedCC
	msgCC       []claudeScannedCC
	msgCCBuf    [16]claudeScannedCC
	topCC       []byte

	messagesIsArray                  bool
	messagesLen                      int
	firstUserIdx                     int
	lastMessageIndex                 int
	lastMsgRole                      string
	lastMsgContentIsNonEmptyString   bool
	lastEligibleIndex                int
	lastEligibleContentIsArray       bool
	lastEligibleContentIsNonEmptyStr bool
	lastEligibleHasCacheControl      bool
	lastEligibleTargetBlockIndex     int

	curMsgRole              string
	curMsgContentIsString   bool
	curMsgContentHasNonSpc  bool
	curMsgContentIsArray    bool
	curMsgHasCacheControl   bool
	curMsgLastCacheableUser int
	curMsgLastCacheableAsst int

	curBlockIsObj         bool
	curBlockType          string
	curBlockTextHasNonSpc bool

	err error
}

func (s *claudeStreamDocScanner) scan(doc *ojson.Doc, collectCC, stopOnFirstUser bool) {
	s.collectCC = collectCC
	s.stopOnFirstUser = stopOnFirstUser
	s.firstUserIdx = -1
	s.lastMessageIndex = -1
	s.lastEligibleIndex = -1
	s.lastEligibleTargetBlockIndex = -1
	s.toolCC = s.toolCCBuf[:0]
	s.sysCC = s.sysCCBuf[:0]
	s.msgCC = s.msgCCBuf[:0]
	s.topCC = nil
	_, _ = doc.WriteTo(s)
}

func (s *claudeStreamDocScanner) appendCaptureBytes(b []byte) {
	if len(b) == 0 {
		return
	}
	if len(s.ccExtra) > 0 {
		s.ccExtra = append(s.ccExtra, b...)
		s.ccLen = len(s.ccExtra)
		return
	}
	if s.ccLen+len(b) <= len(s.ccBuf) {
		copy(s.ccBuf[s.ccLen:], b)
		s.ccLen += len(b)
		return
	}
	s.ccExtra = append(s.ccExtra[:0], s.ccBuf[:s.ccLen]...)
	s.ccExtra = append(s.ccExtra, b...)
	s.ccLen = len(s.ccExtra)
}

func (s *claudeStreamDocScanner) startCapture(depth, idx int) {
	s.captureActive = true
	s.captureDepth = depth
	s.capStart = idx
	s.ccLen = 0
	if s.ccExtra != nil {
		s.ccExtra = s.ccExtra[:0]
	}
}

func (s *claudeStreamDocScanner) finishCapture(p []byte, endIdx int) {
	s.appendCaptureBytes(p[s.capStart:endIdx])
	s.captureActive = false
	if s.ccLen == 0 {
		return
	}
	var raw []byte
	if len(s.ccExtra) > 0 {
		raw = s.ccExtra
	} else {
		raw = s.ccBuf[:s.ccLen]
	}
	if s.captureDepth == 5 {
		if isValidClaudeCacheControl(util.ParseGJSONBytesNoCopy(raw)) {
			s.curMsgHasCacheControl = true
		}
	}
	if !s.collectCC {
		return
	}
	var stored []byte
	if s.ccArenaUsed+len(raw) <= len(s.ccArena) {
		stored = s.ccArena[s.ccArenaUsed : s.ccArenaUsed+len(raw)]
		copy(stored, raw)
		s.ccArenaUsed += len(raw)
	} else {
		stored = bytes.Clone(raw)
	}
	switch s.topSection {
	case claudeSecTools:
		s.toolCC = append(s.toolCC, claudeScannedCC{idx1: s.elemIdx, raw: stored})
	case claudeSecSystem:
		s.sysCC = append(s.sysCC, claudeScannedCC{idx1: s.elemIdx, raw: stored})
	case claudeSecMessages:
		s.msgCC = append(s.msgCC, claudeScannedCC{idx1: s.elemIdx, idx2: s.blockIdx, raw: stored})
	case claudeSecTopCacheControl:
		s.topCC = stored
	}
}

func (s *claudeStreamDocScanner) resetShortToken() {
	s.strTarget = claudeStrCapShort
	s.strLen = 0
	s.strHasEsc = false
	if s.strExtra != nil {
		s.strExtra = s.strExtra[:0]
	}
}

func (s *claudeStreamDocScanner) appendShortByte(c byte) {
	if len(s.strExtra) > 0 {
		s.strExtra = append(s.strExtra, c)
		s.strLen = len(s.strExtra)
		return
	}
	if s.strLen < len(s.strBuf) {
		s.strBuf[s.strLen] = c
		s.strLen++
		return
	}
	s.strExtra = append(s.strExtra[:0], s.strBuf[:s.strLen]...)
	s.strExtra = append(s.strExtra, c)
	s.strLen = len(s.strExtra)
}

func (s *claudeStreamDocScanner) decodeShortToken() string {
	var rawStr []byte
	if len(s.strExtra) > 0 {
		rawStr = s.strExtra
	} else {
		rawStr = s.strBuf[:s.strLen]
	}
	if !s.strHasEsc {
		switch string(rawStr) {
		case "":
			return ""
		case "tools":
			return "tools"
		case "system":
			return "system"
		case "messages":
			return "messages"
		case "cache_control":
			return "cache_control"
		case "role":
			return "role"
		case "content":
			return "content"
		case "type":
			return "type"
		case "text":
			return "text"
		case "user":
			return "user"
		case "assistant":
			return "assistant"
		case "thinking":
			return "thinking"
		case "redacted_thinking":
			return "redacted_thinking"
		default:
			return "other"
		}
	}
	var stackRaw [258]byte
	var quoted []byte
	if 2+len(rawStr) <= len(stackRaw) {
		stackRaw[0] = '"'
		copy(stackRaw[1:], rawStr)
		stackRaw[1+len(rawStr)] = '"'
		quoted = stackRaw[:2+len(rawStr)]
	} else {
		quoted = make([]byte, 2+len(rawStr))
		quoted[0] = '"'
		copy(quoted[1:], rawStr)
		quoted[1+len(rawStr)] = '"'
	}
	decoded := util.ParseGJSONBytesNoCopy(quoted).String()
	switch decoded {
	case "":
		return ""
	case "tools":
		return "tools"
	case "system":
		return "system"
	case "messages":
		return "messages"
	case "cache_control":
		return "cache_control"
	case "role":
		return "role"
	case "content":
		return "content"
	case "type":
		return "type"
	case "text":
		return "text"
	case "user":
		return "user"
	case "assistant":
		return "assistant"
	case "thinking":
		return "thinking"
	case "redacted_thinking":
		return "redacted_thinking"
	default:
		return "other"
	}
}

func (s *claudeStreamDocScanner) finishKey() {
	if s.strTarget != claudeStrCapShort {
		return
	}
	key := s.decodeShortToken()
	switch s.depth {
	case 1:
		switch key {
		case "tools":
			s.topSection = claudeSecTools
		case "system":
			s.topSection = claudeSecSystem
		case "messages":
			s.topSection = claudeSecMessages
		case "cache_control":
			s.topSection = claudeSecTopCacheControl
		default:
			s.topSection = claudeSecNone
		}
	case 3:
		if s.isObj[2] || !s.isObj[3] {
			s.lvl3Key = claudeK3None
			return
		}
		if s.topSection == claudeSecTools || s.topSection == claudeSecSystem {
			if key == "cache_control" {
				s.lvl3Key = claudeK3CacheControl
			} else {
				s.lvl3Key = claudeK3None
			}
		} else if s.topSection == claudeSecMessages {
			switch key {
			case "role":
				s.lvl3Key = claudeK3Role
			case "content":
				s.lvl3Key = claudeK3Content
			default:
				s.lvl3Key = claudeK3None
			}
		}
	case 5:
		if s.topSection != claudeSecMessages || s.lvl3Key != claudeK3Content || s.isObj[4] || !s.isObj[5] {
			s.lvl5Key = claudeK5None
			return
		}
		switch key {
		case "type":
			s.lvl5Key = claudeK5Type
		case "text":
			s.lvl5Key = claudeK5Text
		case "cache_control":
			s.lvl5Key = claudeK5CacheControl
		default:
			s.lvl5Key = claudeK5None
		}
	}
}

func (s *claudeStreamDocScanner) startValue(c byte, idx int) {
	switch s.depth {
	case 1:
		if s.topSection == claudeSecTopCacheControl && s.collectCC && s.isObj[1] {
			s.startCapture(1, idx)
		}
	case 2:
		if s.topSection != claudeSecNone && !s.isObj[2] {
			if s.topSection == claudeSecMessages {
				s.messagesLen = s.elemIdx + 1
				s.curMsgRole = ""
				s.curMsgContentIsString = false
				s.curMsgContentHasNonSpc = false
				s.curMsgContentIsArray = false
				s.curMsgHasCacheControl = false
				s.curMsgLastCacheableUser = -1
				s.curMsgLastCacheableAsst = -1
			}
		}
	case 3:
		if !s.isObj[2] && s.isObj[3] {
			if s.lvl3Key == claudeK3CacheControl && s.collectCC {
				s.startCapture(3, idx)
			} else if s.lvl3Key == claudeK3Content && s.topSection == claudeSecMessages {
				if c == '"' {
					s.curMsgContentIsString = true
				} else if c == '[' {
					s.curMsgContentIsArray = true
					s.blockIdx = 0
				}
			}
		}
	case 4:
		if s.topSection == claudeSecMessages && s.lvl3Key == claudeK3Content && !s.isObj[4] {
			s.curBlockIsObj = c == '{'
			s.curBlockType = ""
			s.curBlockTextHasNonSpc = false
		}
	case 5:
		if s.topSection == claudeSecMessages && s.lvl3Key == claudeK3Content && !s.isObj[4] && s.isObj[5] {
			if s.lvl5Key == claudeK5CacheControl {
				s.startCapture(5, idx)
			}
		}
	}
}

func (s *claudeStreamDocScanner) finishValue(p []byte, endIdx int) {
	if s.captureActive && s.depth == s.captureDepth {
		s.finishCapture(p, endIdx)
	}
	switch s.depth {
	case 1:
		s.topSection = claudeSecNone
	case 2:
		if s.topSection == claudeSecMessages && !s.isObj[2] {
			s.lastMessageIndex = s.elemIdx
			s.lastMsgRole = s.curMsgRole
			s.lastMsgContentIsNonEmptyString = s.curMsgContentIsString && s.curMsgContentHasNonSpc
			if s.curMsgRole == "user" && s.firstUserIdx < 0 {
				s.firstUserIdx = s.elemIdx
				if s.stopOnFirstUser {
					s.err = errStopClaudeScan
				}
			}
			if s.curMsgRole == "user" || s.curMsgRole == "assistant" {
				targetBlock := s.curMsgLastCacheableUser
				if s.curMsgRole == "assistant" {
					targetBlock = s.curMsgLastCacheableAsst
				}
				eligible := false
				if s.curMsgContentIsString {
					eligible = s.curMsgContentHasNonSpc
				} else if s.curMsgContentIsArray {
					eligible = targetBlock >= 0
				}
				if eligible {
					s.lastEligibleIndex = s.elemIdx
					s.lastEligibleContentIsArray = s.curMsgContentIsArray
					s.lastEligibleContentIsNonEmptyStr = s.curMsgContentIsString && s.curMsgContentHasNonSpc
					s.lastEligibleHasCacheControl = s.curMsgHasCacheControl
					s.lastEligibleTargetBlockIndex = targetBlock
				}
			}
		}
	case 3:
		if s.isObj[3] {
			s.lvl3Key = claudeK3None
		}
	case 4:
		if s.topSection == claudeSecMessages && s.lvl3Key == claudeK3Content && !s.isObj[4] {
			if s.curBlockIsObj {
				switch s.curBlockType {
				case "text":
					if s.curBlockTextHasNonSpc {
						s.curMsgLastCacheableUser = s.blockIdx
						s.curMsgLastCacheableAsst = s.blockIdx
					}
				case "thinking", "redacted_thinking":
					s.curMsgLastCacheableAsst = -1
				case "":
				default:
					s.curMsgLastCacheableUser = s.blockIdx
					s.curMsgLastCacheableAsst = s.blockIdx
				}
			}
		}
	case 5:
		if s.isObj[5] {
			s.lvl5Key = claudeK5None
		}
	}
}

func claudeHexValue(c byte) uint16 {
	switch {
	case c >= '0' && c <= '9':
		return uint16(c - '0')
	case c >= 'a' && c <= 'f':
		return uint16(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return uint16(c-'A') + 10
	default:
		return 0
	}
}

func (s *claudeStreamDocScanner) Write(p []byte) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	if s.captureActive {
		s.capStart = 0
	}
	i := 0
	for i < len(p) {
		switch s.tok {
		case claudeScanTokString:
			if s.strEsc == 0 && (s.strTarget == claudeStrIgnore || (s.strTarget == claudeStrCheckNonSpace && s.strHasNonSpace)) {
				for i < len(p) && p[i] != '"' && p[i] != '\\' {
					i++
				}
				if i >= len(p) {
					break
				}
			}
			c := p[i]
			switch s.strEsc {
			case 0:
				if c == '\\' {
					if s.strTarget == claudeStrCapShort {
						s.strHasEsc = true
						s.appendShortByte(c)
					} else if s.strTarget == claudeStrCheckNonSpace && s.utfLen > 0 {
						s.strHasNonSpace = true
						s.utfLen = 0
					}
					s.strEsc = 1
					i++
					continue
				}
				if c == '"' {
					if s.strTarget == claudeStrCheckNonSpace && s.utfLen > 0 {
						s.strHasNonSpace = true
						s.utfLen = 0
					}
					s.tok = claudeScanTokNone
					if s.strIsKey {
						s.strIsKey = false
						s.finishKey()
					} else {
						if s.depth == 3 && s.topSection == claudeSecMessages && !s.isObj[2] && s.isObj[3] {
							if s.lvl3Key == claudeK3Role {
								s.curMsgRole = s.decodeShortToken()
								if s.curMsgRole == "user" && s.firstUserIdx < 0 {
									s.firstUserIdx = s.elemIdx
									if s.stopOnFirstUser {
										s.err = errStopClaudeScan
										return len(p), s.err
									}
								}
							} else if s.lvl3Key == claudeK3Content {
								s.curMsgContentHasNonSpc = s.strHasNonSpace
							}
						} else if s.depth == 5 && s.topSection == claudeSecMessages && s.lvl3Key == claudeK3Content && !s.isObj[4] && s.isObj[5] {
							if s.lvl5Key == claudeK5Type {
								s.curBlockType = s.decodeShortToken()
							} else if s.lvl5Key == claudeK5Text {
								s.curBlockTextHasNonSpc = s.strHasNonSpace
							}
						}
						s.finishValue(p, i+1)
						if s.err != nil {
							return len(p), s.err
						}
					}
					i++
					continue
				}
				if s.strTarget == claudeStrCapShort {
					s.appendShortByte(c)
				} else if s.strTarget == claudeStrCheckNonSpace && !s.strHasNonSpace {
					if c < 0x80 {
						if s.utfLen > 0 || (c > ' ') || (c != ' ' && c != '\t' && c != '\n' && c != '\v' && c != '\f' && c != '\r') {
							s.strHasNonSpace = true
							s.utfLen = 0
						}
					} else {
						s.utfBuf[s.utfLen] = c
						s.utfLen++
						if utf8.FullRune(s.utfBuf[:s.utfLen]) {
							r, _ := utf8.DecodeRune(s.utfBuf[:s.utfLen])
							s.utfLen = 0
							if !unicode.IsSpace(r) {
								s.strHasNonSpace = true
							}
						} else if int(s.utfLen) == len(s.utfBuf) {
							s.utfLen = 0
							s.strHasNonSpace = true
						}
					}
				}
				i++
			case 1:
				if s.strTarget == claudeStrCapShort {
					s.appendShortByte(c)
				} else if s.strTarget == claudeStrCheckNonSpace && !s.strHasNonSpace {
					switch c {
					case '"', '\\', '/', 'b':
						s.strHasNonSpace = true
					}
				}
				if c == 'u' {
					s.strEsc = 2
					s.uVal = 0
				} else {
					s.strEsc = 0
				}
				i++
			default:
				if s.strTarget == claudeStrCapShort {
					s.appendShortByte(c)
				} else if s.strTarget == claudeStrCheckNonSpace && !s.strHasNonSpace {
					s.uVal = (s.uVal << 4) | claudeHexValue(c)
					if s.strEsc == 5 && !unicode.IsSpace(rune(s.uVal)) {
						s.strHasNonSpace = true
					}
				}
				if s.strEsc == 5 {
					s.strEsc = 0
				} else {
					s.strEsc++
				}
				i++
			}
			continue

		case claudeScanTokPrimitive:
			for i < len(p) && p[i] > ' ' && p[i] != ',' && p[i] != '}' && p[i] != ']' && p[i] != ':' {
				i++
			}
			if i >= len(p) {
				break
			}
			s.tok = claudeScanTokNone
			s.finishValue(p, i)
			if s.err != nil {
				return len(p), s.err
			}
		}

		for i < len(p) && (p[i] == ' ' || p[i] == '\t' || p[i] == '\r' || p[i] == '\n') {
			i++
		}
		if i >= len(p) {
			break
		}
		c := p[i]
		if c == ':' {
			if s.depth > 0 && s.depth < len(s.inKey) {
				s.inKey[s.depth] = false
			}
			i++
			continue
		}
		if c == ',' {
			if s.depth > 0 && s.depth < len(s.isObj) {
				if s.isObj[s.depth] {
					s.inKey[s.depth] = true
				} else if s.depth == 2 && s.topSection != claudeSecNone {
					s.elemIdx++
				} else if s.depth == 4 && s.topSection == claudeSecMessages && s.lvl3Key == claudeK3Content {
					s.blockIdx++
				}
			}
			i++
			continue
		}
		if c == '}' || c == ']' {
			if s.depth > 0 {
				s.depth--
				if s.depth >= 1 {
					s.finishValue(p, i+1)
					if s.err != nil {
						return len(p), s.err
					}
				}
			}
			i++
			continue
		}
		if s.depth > 0 && s.depth < len(s.isObj) && s.isObj[s.depth] && s.inKey[s.depth] {
			if c == '"' {
				s.tok = claudeScanTokString
				s.strEsc = 0
				s.strIsKey = true
				if s.depth == 1 || s.depth == 3 || s.depth == 5 {
					s.resetShortToken()
				} else {
					s.strTarget = claudeStrIgnore
				}
			}
			i++
			continue
		}

		s.startValue(c, i)
		switch c {
		case '{', '[':
			s.depth++
			if s.depth < len(s.isObj) {
				s.isObj[s.depth] = c == '{'
				s.inKey[s.depth] = c == '{'
			}
			if s.depth == 2 && c == '[' && s.topSection != claudeSecNone {
				s.elemIdx = 0
				if s.topSection == claudeSecMessages {
					s.messagesIsArray = true
				}
			}
			i++
		case '"':
			s.tok = claudeScanTokString
			s.strEsc = 0
			s.strIsKey = false
			s.strTarget = claudeStrIgnore
			if s.depth == 3 && s.topSection == claudeSecMessages && !s.isObj[2] && s.isObj[3] {
				if s.lvl3Key == claudeK3Role {
					s.resetShortToken()
				} else if s.lvl3Key == claudeK3Content {
					s.strTarget = claudeStrCheckNonSpace
					s.strHasNonSpace = false
					s.utfLen = 0
				}
			} else if s.depth == 5 && s.topSection == claudeSecMessages && s.lvl3Key == claudeK3Content && !s.isObj[4] && s.isObj[5] {
				if s.lvl5Key == claudeK5Type {
					s.resetShortToken()
				} else if s.lvl5Key == claudeK5Text {
					s.strTarget = claudeStrCheckNonSpace
					s.strHasNonSpace = false
					s.utfLen = 0
				}
			}
			i++
		default:
			s.tok = claudeScanTokPrimitive
			i++
		}
	}
	if s.captureActive {
		s.appendCaptureBytes(p[s.capStart:len(p)])
	}
	return len(p), nil
}

func forEachClaudeCacheControlPathDoc(doc *ojson.Doc, visit func(path string, cacheControl ojson.Result) bool) {
	if doc == nil || doc.Len() == 0 {
		return
	}
	var scanner claudeStreamDocScanner
	scanner.scan(doc, true, false)
	for _, entry := range scanner.toolCC {
		ccRes := util.ParseGJSONBytesNoCopy(entry.raw)
		if !ccRes.Exists() {
			continue
		}
		if !visit("tools."+strconv.Itoa(entry.idx1), ccRes) {
			return
		}
	}
	for _, entry := range scanner.sysCC {
		ccRes := util.ParseGJSONBytesNoCopy(entry.raw)
		if !ccRes.Exists() {
			continue
		}
		if !visit("system."+strconv.Itoa(entry.idx1), ccRes) {
			return
		}
	}
	for _, entry := range scanner.msgCC {
		ccRes := util.ParseGJSONBytesNoCopy(entry.raw)
		if !ccRes.Exists() {
			continue
		}
		path := "messages." + strconv.Itoa(entry.idx1) + ".content." + strconv.Itoa(entry.idx2)
		if !visit(path, ccRes) {
			return
		}
	}
	if len(scanner.topCC) > 0 {
		ccRes := util.ParseGJSONBytesNoCopy(scanner.topCC)
		if ccRes.Exists() {
			if !visit("", ccRes) {
				return
			}
		}
	}
}

func forEachClaudeCacheControlBlock(payload []byte, visit func(path string, block ojson.Result)) {
	forEachClaudeCacheControlBlockResult(
		ojson.GetBytesNoCopy(payload, "tools"),
		ojson.GetBytesNoCopy(payload, "system"),
		ojson.GetBytesNoCopy(payload, "messages"),
		visit,
	)
}

func forEachClaudeCacheControlBlockResult(tools, system, messages ojson.Result, visit func(path string, block ojson.Result)) {
	if tools.IsArray() {
		tools.ForEach(func(idx, item ojson.Result) bool {
			visit(fmt.Sprintf("tools.%d", int(idx.Int())), item)
			return true
		})
	}
	if system.IsArray() {
		system.ForEach(func(idx, item ojson.Result) bool {
			visit(fmt.Sprintf("system.%d", int(idx.Int())), item)
			return true
		})
	}
	if messages.IsArray() {
		messages.ForEach(func(msgIdx, message ojson.Result) bool {
			content := message.Get("content")
			if !content.IsArray() {
				return true
			}
			content.ForEach(func(itemIdx, item ojson.Result) bool {
				visit(fmt.Sprintf("messages.%d.content.%d", int(msgIdx.Int()), int(itemIdx.Int())), item)
				return true
			})
			return true
		})
	}
}

func isExplicitPromptCacheMode(payloads ...[]byte) bool {
	for _, payload := range payloads {
		if len(payload) == 0 {
			continue
		}
		mode := ojson.GetBytesNoCopy(payload, "prompt_cache_options.mode").String()
		if strings.EqualFold(strings.TrimSpace(mode), "explicit") {
			return true
		}
	}
	return false
}

func isExplicitPromptCacheModeDoc(doc *ojson.Doc, candidates ...[]byte) bool {
	if doc != nil && doc.Len() > 0 {
		mode := doc.Get("prompt_cache_options.mode").String()
		if strings.EqualFold(strings.TrimSpace(mode), "explicit") {
			return true
		}
	}
	return isExplicitPromptCacheMode(candidates...)
}

func stripPromptCacheOptions(payload []byte) []byte {
	doc := ojson.NewDoc(payload)
	if !stripPromptCacheOptionsDoc(doc) {
		return payload
	}
	return doc.Bytes()
}

func stripPromptCacheOptionsDoc(doc *ojson.Doc) bool {
	if doc == nil || !doc.Get("prompt_cache_options").Exists() {
		return false
	}
	return doc.Delete("prompt_cache_options") == nil
}

func shouldEnsureCacheControl(payload []byte, cloaked, confirmedClaudeCode bool, candidates ...[]byte) bool {
	if isExplicitPromptCacheMode(append([][]byte{payload}, candidates...)...) {
		return false
	}
	return !confirmedClaudeCode && (cloaked || countCacheControls(payload) == 0)
}

func shouldEnsureCacheControlDoc(doc *ojson.Doc, cloaked, confirmedClaudeCode bool, candidates ...[]byte) bool {
	if isExplicitPromptCacheModeDoc(doc, candidates...) {
		return false
	}
	return !confirmedClaudeCode && (cloaked || countCacheControlsDoc(doc) == 0)
}

func countCacheControls(payload []byte) int {
	count := countCacheControlsResult(
		ojson.GetBytesNoCopy(payload, "system"),
		ojson.GetBytesNoCopy(payload, "tools"),
		ojson.GetBytesNoCopy(payload, "messages"),
	)
	if isValidClaudeCacheControl(ojson.GetBytesNoCopy(payload, "cache_control")) {
		count++
	}
	return count
}

func countCacheControlsDoc(doc *ojson.Doc) int {
	if doc == nil {
		return 0
	}
	count := 0
	forEachClaudeCacheControlPathDoc(doc, func(_ string, cc ojson.Result) bool {
		if isValidClaudeCacheControl(cc) {
			count++
		}
		return true
	})
	return count
}

func countCacheControlsResult(system, tools, messages ojson.Result) int {
	count := 0

	if system.IsArray() {
		system.ForEach(func(_, item ojson.Result) bool {
			if isValidClaudeCacheControl(item.Get("cache_control")) {
				count++
			}
			return true
		})
	}

	if tools.IsArray() {
		tools.ForEach(func(_, item ojson.Result) bool {
			if isValidClaudeCacheControl(item.Get("cache_control")) {
				count++
			}
			return true
		})
	}

	if messages.IsArray() {
		messages.ForEach(func(_, msg ojson.Result) bool {
			content := msg.Get("content")
			if content.IsArray() {
				content.ForEach(func(_, item ojson.Result) bool {
					if isValidClaudeCacheControl(item.Get("cache_control")) {
						count++
					}
					return true
				})
			}
			return true
		})
	}

	return count
}

func normalizeCacheControlTTL(payload []byte) []byte {
	if len(payload) == 0 || !ojson.ValidBytes(payload) {
		return payload
	}
	doc := ojson.NewDoc(payload)
	if !normalizeCacheControlTTLDoc(doc) {
		return payload
	}
	return doc.Bytes()
}

func normalizeCacheControlTTLDoc(doc *ojson.Doc) bool {
	if doc == nil || doc.Len() == 0 {
		return false
	}

	seen5m := false
	modified := false
	forEachClaudeCacheControlPathDoc(doc, func(path string, cc ojson.Result) bool {
		if !isValidClaudeCacheControl(cc) {
			return true
		}
		ttl := cc.Get("ttl")
		if ttl.Type != ojson.String || ttl.String() != "1h" {
			seen5m = true
			return true
		}
		if !seen5m {
			return true
		}
		if errDel := doc.Delete(claudeCacheControlTTLPath(path)); errDel == nil {
			modified = true
		}
		return true
	})
	return modified
}

func enforceCacheControlLimit(payload []byte, maxBlocks int) []byte {
	if len(payload) == 0 || !ojson.ValidBytes(payload) {
		return payload
	}
	doc := ojson.NewDoc(payload)
	if !enforceCacheControlLimitDoc(doc, maxBlocks) {
		return payload
	}
	return doc.Bytes()
}

func enforceCacheControlLimitDoc(doc *ojson.Doc, maxBlocks int) bool {
	if doc == nil || doc.Len() == 0 {
		return false
	}

	thread := doc.Get("thread")
	if thread.Exists() && thread.Type != ojson.Null && maxBlocks > 0 {
		// Anthropic reserves one cache breakpoint for thread continuation.
		maxBlocks--
	}

	var toolPaths, systemPaths, messagePaths, topPaths []string
	forEachClaudeCacheControlPathDoc(doc, func(path string, cc ojson.Result) bool {
		if !isValidClaudeCacheControl(cc) {
			return true
		}
		ccPath := claudeCacheControlPath(path)
		switch {
		case path == "":
			topPaths = append(topPaths, ccPath)
		case strings.HasPrefix(path, "tools."):
			toolPaths = append(toolPaths, ccPath)
		case strings.HasPrefix(path, "system."):
			systemPaths = append(systemPaths, ccPath)
		default:
			messagePaths = append(messagePaths, ccPath)
		}
		return true
	})

	total := len(toolPaths) + len(systemPaths) + len(messagePaths) + len(topPaths)
	if total <= maxBlocks {
		return false
	}

	excess := total - maxBlocks
	changed := false
	deleteList := func(paths []string) {
		for _, path := range paths {
			if excess <= 0 {
				return
			}
			if errDel := doc.Delete(path); errDel == nil {
				changed = true
				excess--
			}
		}
	}

	if len(systemPaths) > 1 {
		deleteList(systemPaths[:len(systemPaths)-1])
	}
	if excess <= 0 {
		return changed
	}
	if len(toolPaths) > 1 {
		deleteList(toolPaths[:len(toolPaths)-1])
	}
	if excess <= 0 {
		return changed
	}
	deleteList(messagePaths)
	if excess <= 0 {
		return changed
	}
	if len(systemPaths) > 0 {
		deleteList(systemPaths[len(systemPaths)-1:])
	}
	if excess <= 0 {
		return changed
	}
	if len(toolPaths) > 0 {
		deleteList(toolPaths[len(toolPaths)-1:])
	}
	if excess <= 0 {
		return changed
	}
	deleteList(topPaths)
	return changed
}

func injectMessagesCacheControl(payload []byte) []byte {
	doc := ojson.NewDoc(payload)
	if !injectMessagesCacheControlDoc(doc) {
		return payload
	}
	return doc.Bytes()
}

func injectMessagesCacheControlDoc(doc *ojson.Doc) bool {
	if doc == nil || doc.Len() == 0 {
		return false
	}
	var scanner claudeStreamDocScanner
	scanner.scan(doc, false, false)
	if !scanner.messagesIsArray || scanner.lastMessageIndex < 0 {
		return false
	}

	if scanner.lastEligibleIndex >= 0 && scanner.lastMsgRole == "system" && scanner.lastMsgContentIsNonEmptyString {
		finalContent := doc.Get(fmt.Sprintf("messages.%d.content", scanner.lastMessageIndex))
		if finalContent.Type == ojson.String && strings.TrimSpace(finalContent.String()) != "" {
			return injectClaudeFinalSystemCacheControlDoc(doc, scanner.lastMessageIndex, finalContent.String())
		}
	}
	if scanner.lastEligibleIndex < 0 || scanner.lastEligibleHasCacheControl {
		return false
	}

	if scanner.lastEligibleContentIsArray {
		if scanner.lastEligibleTargetBlockIndex >= 0 {
			cacheControlPath := fmt.Sprintf("messages.%d.content.%d.cache_control", scanner.lastEligibleIndex, scanner.lastEligibleTargetBlockIndex)
			if errSet := doc.Set(cacheControlPath, defaultClaudeCacheControl); errSet != nil {
				log.Warnf("failed to inject cache_control into messages: %v", errSet)
				return false
			}
			return true
		}
		return false
	}

	if scanner.lastEligibleContentIsNonEmptyStr {
		contentPath := fmt.Sprintf("messages.%d.content", scanner.lastEligibleIndex)
		contentRes := doc.Get(contentPath)
		if contentRes.Type == ojson.String && strings.TrimSpace(contentRes.String()) != "" {
			newContent := "[" + buildTextBlock(contentRes.String(), &defaultClaudeCacheControl) + "]"
			if errSet := doc.SetRawBytes(contentPath, []byte(newContent)); errSet != nil {
				log.Warnf("failed to inject cache_control into message string content: %v", errSet)
				return false
			}
			return true
		}
	}

	return false
}

func isEmptyClaudeTextBlock(block ojson.Result) bool {
	return block.IsObject() &&
		block.Get("type").String() == "text" &&
		strings.TrimSpace(block.Get("text").String()) == ""
}

func claudeContentBlockCanHostCacheControl(block ojson.Result) bool {
	if !block.IsObject() {
		return false
	}
	switch block.Get("type").String() {
	case "", "thinking", "redacted_thinking":
		return false
	case "text":
		return strings.TrimSpace(block.Get("text").String()) != ""
	default:
		return true
	}
}

func lastCacheableClaudeMessageContentBlockIndex(message ojson.Result) int {
	content := message.Get("content")
	if !content.IsArray() {
		return -1
	}
	blocks := content.Array()
	if len(blocks) == 0 {
		return -1
	}
	isAssistant := message.Get("role").String() == "assistant"
	for i := len(blocks) - 1; i >= 0; i-- {
		block := blocks[i]
		if isEmptyClaudeTextBlock(block) {
			continue
		}
		if isAssistant {
			switch block.Get("type").String() {
			case "thinking", "redacted_thinking":
				return -1
			}
		}
		if claudeContentBlockCanHostCacheControl(block) {
			return i
		}
	}
	return -1
}

func claudeMessageEligibleForRollingCache(message ojson.Result) bool {
	content := message.Get("content")
	if content.Type == ojson.String {
		return strings.TrimSpace(content.String()) != ""
	}
	return lastCacheableClaudeMessageContentBlockIndex(message) >= 0
}

func injectClaudeFinalSystemCacheControl(payload []byte, messageIndex int, text string) []byte {
	doc := ojson.NewDoc(payload)
	if !injectClaudeFinalSystemCacheControlDoc(doc, messageIndex, text) {
		return payload
	}
	return doc.Bytes()
}

func injectClaudeFinalSystemCacheControlDoc(doc *ojson.Doc, messageIndex int, text string) bool {
	if doc == nil {
		return false
	}
	contentPath := fmt.Sprintf("messages.%d.content", messageIndex)
	newContent := "[" + buildTextBlock(text, &defaultClaudeCacheControl) + "]"
	if errSet := doc.SetRawBytes(contentPath, []byte(newContent)); errSet != nil {
		log.Warnf("failed to inject cache_control into trailing system message: %v", errSet)
		return false
	}
	return true
}

func messageContentHasCacheControl(content ojson.Result) bool {
	if content.IsArray() {
		found := false
		content.ForEach(func(_, item ojson.Result) bool {
			if isValidClaudeCacheControl(item.Get("cache_control")) {
				found = true
				return false
			}
			return true
		})
		return found
	}
	return false
}

func injectToolsCacheControl(payload []byte) []byte {
	doc := ojson.NewDoc(payload)
	if !injectToolsCacheControlDoc(doc) {
		return payload
	}
	return doc.Bytes()
}

func injectToolsCacheControlDoc(doc *ojson.Doc) bool {
	if doc == nil {
		return false
	}
	tools := doc.Get("tools")
	if !tools.IsArray() {
		return false
	}

	// Check if ANY tool already has cache_control and find the last eligible tool.
	hasCacheControlInTools := false
	lastEligibleToolIndex := -1
	idx := 0
	tools.ForEach(func(_, tool ojson.Result) bool {
		if isValidClaudeCacheControl(tool.Get("cache_control")) {
			hasCacheControlInTools = true
			return false
		}
		if !tool.Get("defer_loading").Bool() {
			lastEligibleToolIndex = idx
		}
		idx++
		return true
	})
	if hasCacheControlInTools || lastEligibleToolIndex < 0 {
		return false
	}

	lastToolPath := fmt.Sprintf("tools.%d.cache_control", lastEligibleToolIndex)
	if errSet := doc.Set(lastToolPath, defaultClaudeCacheControl); errSet != nil {
		log.Warnf("failed to inject cache_control into tools array: %v", errSet)
		return false
	}

	return true
}

func injectSystemCacheControl(payload []byte) []byte {
	doc := ojson.NewDoc(payload)
	if !injectSystemCacheControlDoc(doc) {
		return payload
	}
	return doc.Bytes()
}

func injectSystemCacheControlDoc(doc *ojson.Doc) bool {
	if doc == nil {
		return false
	}
	system := doc.Get("system")
	if system.IsArray() {
		// Check if ANY system element already has cache_control and find the last cacheable element.
		hasCacheControlInSystem := false
		lastEligibleSystemIndex := -1
		idx := 0
		system.ForEach(func(_, item ojson.Result) bool {
			if isValidClaudeCacheControl(item.Get("cache_control")) {
				hasCacheControlInSystem = true
				return false
			}
			if claudeSystemBlockCanHostCacheControl(item) {
				lastEligibleSystemIndex = idx
			}
			idx++
			return true
		})
		if hasCacheControlInSystem || lastEligibleSystemIndex < 0 {
			return false
		}

		// Add cache_control to the last cacheable system element
		lastSystemPath := fmt.Sprintf("system.%d.cache_control", lastEligibleSystemIndex)
		if errSet := doc.Set(lastSystemPath, defaultClaudeCacheControl); errSet != nil {
			log.Warnf("failed to inject cache_control into system array: %v", errSet)
			return false
		}
		return true
	}
	if !system.Exists() {
		return false
	}
	if system.Type == ojson.String {
		// Empty/blank strings are not cacheable hosts. claudePayloadHasCacheableSystem
		// already treats them as missing so tools can cover the prefix; converting them
		// here would create a second, useless breakpoint on whitespace.
		if strings.TrimSpace(system.String()) == "" {
			return false
		}
		// Convert string system prompt to an ordered native text block.
		newSystem := "[" + buildTextBlock(system.String(), &defaultClaudeCacheControl) + "]"
		if errSet := doc.SetRawBytes("system", []byte(newSystem)); errSet != nil {
			log.Warnf("failed to inject cache_control into system string: %v", errSet)
			return false
		}
		return true
	}

	return false
}

func ensureModelMaxTokens(body []byte, modelID string) []byte {
	if len(body) == 0 || !ojson.ValidBytes(body) {
		return body
	}
	doc := ojson.NewDoc(body)
	if !ensureModelMaxTokensDoc(doc, modelID) {
		return body
	}
	return doc.Bytes()
}
