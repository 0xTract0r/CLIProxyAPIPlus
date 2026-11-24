package executor

import "net/http"

// ordinaryCodexWebsocketPreflight checks the already framed request bytes.
func ordinaryCodexWebsocketPreflight(framed []byte, budget int, allowHTTP bool) (bool, error) {
	if len(framed) <= budget {
		return false, nil
	}
	if allowHTTP {
		return true, nil
	}
	return false, codexWebsocketMessageTooBigError{statusErr: statusErr{
		code: http.StatusRequestEntityTooLarge,
		msg:  "websocket request exceeds the configured message limit",
	}}
}
