package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func (s *Server) ampJSONInference(handler gin.HandlerFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		body, err := handlers.ReadRequestBody(c)
		if err != nil {
			var limit *http.MaxBytesError
			if errors.As(err, &limit) {
				writeAmpInferenceError(c, http.StatusRequestEntityTooLarge, "request body is too large")
			} else {
				writeAmpInferenceError(c, http.StatusBadRequest, "unable to read request body")
			}
			return
		}
		_ = c.Request.Body.Close()
		setAmpRequestBody(c.Request, body)
		c.Request.Header.Del("Content-Encoding")
		if !json.Valid(body) {
			writeAmpInferenceError(c, http.StatusBadRequest, "invalid JSON request")
			return
		}
		model := gjson.GetBytes(body, "model")
		if model.Type != gjson.String || strings.TrimSpace(model.Str) == "" {
			writeAmpInferenceError(c, http.StatusBadRequest, "model is required")
			return
		}
		resolved, forward := s.resolveAmpInference(model.Str)
		if forward && s.forwardUnresolvedAmpInference(c) {
			return
		}
		if resolved != model.Str {
			body, err = sjson.SetBytes(body, "model", resolved)
			if err != nil {
				writeAmpInferenceError(c, http.StatusBadRequest, "unable to apply Amp model mapping")
				return
			}
			mappedRequest := c.Request.Clone(c.Request.Context())
			setAmpRequestBody(mappedRequest, body)
			if !s.authorizeAmpMappedRequest(c, mappedRequest) {
				return
			}
			setAmpRequestBody(c.Request, body)
		}
		handler(c)
	}
}

func (s *Server) ampGeminiInference(handler gin.HandlerFunc, vertexPath bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		action := strings.TrimPrefix(c.Param("action"), "/")
		if vertexPath {
			if !strings.EqualFold(c.Param("provider"), "google") {
				c.AbortWithStatus(http.StatusNotFound)
				return
			}
			_, tail, found := strings.Cut("/"+action, "/models/")
			if !found {
				c.AbortWithStatus(http.StatusNotFound)
				return
			}
			action = tail
		}
		model, method, valid := strings.Cut(action, ":")
		if !valid || model == "" || (method != "generateContent" && method != "streamGenerateContent" && method != "countTokens") {
			writeAmpInferenceError(c, http.StatusNotFound, "unknown Gemini model action")
			return
		}
		resolved, forward := s.resolveAmpInference(model)
		if forward && s.forwardUnresolvedAmpInference(c) {
			return
		}
		if resolved != model {
			mappedRequest := c.Request.Clone(c.Request.Context())
			pathTail := model + ":" + method
			before, found := strings.CutSuffix(mappedRequest.URL.Path, pathTail)
			if !found {
				writeAmpInferenceError(c, http.StatusBadRequest, "invalid Gemini model path")
				return
			}
			mappedRequest.URL.Path = before + resolved + ":" + method
			mappedRequest.URL.RawPath = ""
			body, err := c.GetRawData()
			if err != nil {
				var limit *http.MaxBytesError
				status := http.StatusBadRequest
				if errors.As(err, &limit) {
					status = http.StatusRequestEntityTooLarge
				}
				writeAmpInferenceError(c, status, "unable to read request body")
				return
			}
			_ = c.Request.Body.Close()
			setAmpRequestBody(c.Request, body)
			setAmpRequestBody(mappedRequest, body)
			if !s.authorizeAmpMappedRequest(c, mappedRequest) {
				return
			}
		}
		for index := range c.Params {
			if c.Params[index].Key == "action" {
				c.Params[index].Value = "/" + resolved + ":" + method
				break
			}
		}
		handler(c)
	}
}

// Resolve from the configured catalog, independent of transient provider
// health, authorization failures, and quota. The dispatch path is never retried
// or changed after the ordinary handler begins processing the request.
func (s *Server) resolveAmpInference(model string) (resolved string, forward bool) {
	cfg := s.getConfig()
	if cfg == nil || cfg.Home.Enabled {
		return model, false
	}
	requested := strings.TrimSpace(model)
	if strings.EqualFold(requested, "auto") {
		return model, false
	}
	known := ampRegisteredModel(requested)
	if !known || cfg.AmpCode.ForceModelMappings {
		if target := ampMappedModel(requested, cfg.AmpCode.ModelMappings); target != "" {
			// An explicit mapping is the only chosen route; an absent target is
			// handled by the standard endpoint, never sent to a second upstream.
			return target, false
		}
	}
	return model, !known
}

func ampRegisteredModel(model string) bool {
	models := registry.GetGlobalRegistry()
	if len(models.GetModelProviders(model)) > 0 {
		return true
	}
	base := thinking.ParseSuffix(model).ModelName
	return base != model && len(models.GetModelProviders(base)) > 0
}

func ampMappedModel(requested string, mappings []config.AmpModelMapping) string {
	input := thinking.ParseSuffix(requested)
	apply := func(to string) string {
		if input.HasSuffix && !thinking.ParseSuffix(to).HasSuffix {
			return to + "(" + input.RawSuffix + ")"
		}
		return to
	}
	for _, name := range []string{requested, input.ModelName} {
		for _, mapping := range mappings {
			if !mapping.Regex && strings.EqualFold(strings.TrimSpace(mapping.From), name) {
				if to := strings.TrimSpace(mapping.To); to != "" {
					return apply(to)
				}
			}
		}
	}
	for _, mapping := range mappings {
		from, to := strings.TrimSpace(mapping.From), strings.TrimSpace(mapping.To)
		if !mapping.Regex || from == "" || to == "" {
			continue
		}
		expression, err := regexp.Compile(from)
		if err == nil && (expression.MatchString(requested) || expression.MatchString(input.ModelName)) {
			return apply(to)
		}
	}
	return ""
}

func (s *Server) forwardUnresolvedAmpInference(c *gin.Context) bool {
	state := s.ampForward.Load()
	if state == nil || state.proxy == nil {
		return false
	}
	if readyAmpForward(c, state) {
		serveAmpForward(c, state, true)
	}
	return true
}

// Both model names must be authorized as the same caller. The authorization
// probe has its own body reader so it cannot consume the real handler's body.
func (s *Server) authorizeAmpMappedRequest(c *gin.Context, request *http.Request) bool {
	if s.accessManager == nil {
		return true
	}
	result, err := s.accessManager.Authenticate(request.Context(), request)
	if err != nil {
		c.AbortWithStatusJSON(err.HTTPStatusCode(), gin.H{"error": err.Message})
		return false
	}
	principal, provider := c.GetString("userApiKey"), c.GetString("accessProvider")
	metadata, _ := c.Get("accessMetadata")
	originalMetadata, _ := metadata.(map[string]string)
	if result == nil {
		if principal == "" && provider == "" && len(originalMetadata) == 0 {
			return true
		}
	} else if result.Principal == principal && result.Provider == provider && maps.Equal(result.Metadata, originalMetadata) {
		return true
	}
	c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "mapped model requires the same authenticated access"})
	return false
}

func setAmpRequestBody(request *http.Request, body []byte) {
	request.Body = io.NopCloser(bytes.NewReader(body))
	request.ContentLength = int64(len(body))
}

func writeAmpInferenceError(c *gin.Context, status int, message string) {
	c.AbortWithStatusJSON(status, handlers.ErrorResponse{
		Error: handlers.ErrorDetail{Message: message, Type: "invalid_request_error"},
	})
}
