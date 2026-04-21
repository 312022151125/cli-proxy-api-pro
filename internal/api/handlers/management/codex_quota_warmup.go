package management

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	codexWarmupBaseURL    = "https://chatgpt.com/backend-api/codex"
	codexWarmupModel      = "gpt-5.4-mini"
	codexWarmupUserAgent  = "codex-tui/0.118.0 (Mac OS 26.3.1; arm64) iTerm.app/3.6.9 (codex-tui; 0.118.0)"
	codexWarmupOriginator = "codex-tui"
)

type codexWarmupResult struct {
	AuthIndex string `json:"auth_index"`
	Email     string `json:"email,omitempty"`
	Status    string `json:"status"` // "warmed_up", "skipped", "error"
	Message   string `json:"message,omitempty"`
}

type codexWarmupResponse struct {
	Results  []codexWarmupResult `json:"results"`
	Total    int                 `json:"total"`
	WarmedUp int                 `json:"warmed_up"`
	Skipped  int                 `json:"skipped"`
	Failed   int                 `json:"failed"`
}

// HotQuotaRefresh iterates over all Codex OAuth auth credentials and warms up the weekly
// quota for those that have not yet made any request in the current week (quota at 100%).
// Credentials that already have partial usage (quota below 100%) are skipped.
//
// Endpoint:
//
//	POST /v0/management/codex-quota-warmup
//
// Response:
//
//	JSON object with per-auth results and aggregate counts.
func (h *Handler) HotQuotaRefresh(c *gin.Context) {
	if h == nil || h.authManager == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "auth manager unavailable"})
		return
	}

	auths := h.authManager.List()
	var codexAuths []*coreauth.Auth
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
			continue
		}
		// Only process OAuth auth (access token stored in metadata), not API-key auth.
		if auth.Attributes != nil && strings.TrimSpace(auth.Attributes["api_key"]) != "" {
			continue
		}
		if auth.Metadata == nil {
			continue
		}
		token, _ := auth.Metadata["access_token"].(string)
		if strings.TrimSpace(token) == "" {
			continue
		}
		auth.EnsureIndex()
		codexAuths = append(codexAuths, auth)
	}

	results := make([]codexWarmupResult, len(codexAuths))
	var wg sync.WaitGroup
	for i, auth := range codexAuths {
		wg.Add(1)
		go func(idx int, a *coreauth.Auth) {
			defer wg.Done()
			results[idx] = h.warmupCodexAuth(c.Request.Context(), a)
		}(i, auth)
	}
	wg.Wait()

	var warmedUp, skipped, failed int
	for _, r := range results {
		switch r.Status {
		case "warmed_up":
			warmedUp++
		case "skipped":
			skipped++
		default:
			failed++
		}
	}

	c.JSON(http.StatusOK, codexWarmupResponse{
		Results:  results,
		Total:    len(results),
		WarmedUp: warmedUp,
		Skipped:  skipped,
		Failed:   failed,
	})
}

func (h *Handler) warmupCodexAuth(ctx context.Context, auth *coreauth.Auth) codexWarmupResult {
	if ctx == nil {
		ctx = context.Background()
	}

	var email string
	if auth.Metadata != nil {
		email, _ = auth.Metadata["email"].(string)
	}

	result := codexWarmupResult{
		AuthIndex: auth.Index,
		Email:     strings.TrimSpace(email),
	}

	token, _ := auth.Metadata["access_token"].(string)
	token = strings.TrimSpace(token)
	if token == "" {
		result.Status = "error"
		result.Message = "missing access token"
		return result
	}

	// Check if the weekly quota is already used (below 100%).
	alreadyUsed, errCheck := h.checkCodexWeeklyQuotaUsed(ctx, auth, token)
	if errCheck != nil {
		log.WithError(errCheck).Debugf("codex warmup: quota check failed for auth %s", auth.Index)
		result.Status = "error"
		result.Message = fmt.Sprintf("quota check failed: %v", errCheck)
		return result
	}
	if alreadyUsed {
		result.Status = "skipped"
		result.Message = "weekly quota already partially used"
		return result
	}

	// Quota is at 100% — send a minimal warmup request.
	if errWarmup := h.sendCodexWarmupRequest(ctx, auth, token); errWarmup != nil {
		log.WithError(errWarmup).Debugf("codex warmup: warmup request failed for auth %s", auth.Index)
		result.Status = "error"
		result.Message = fmt.Sprintf("warmup request failed: %v", errWarmup)
		return result
	}

	result.Status = "warmed_up"
	return result
}

// checkCodexWeeklyQuotaUsed calls the Codex usage endpoint and returns true when the
// weekly quota has already been partially consumed (quota below 100%).
// Returns false when no requests have been made in the current period (quota at 100%).
func (h *Handler) checkCodexWeeklyQuotaUsed(ctx context.Context, auth *coreauth.Auth, token string) (bool, error) {
	baseURL := codexWarmupBaseURL
	if auth.Attributes != nil {
		if u := strings.TrimSpace(auth.Attributes["base_url"]); u != "" {
			baseURL = u
		}
	}
	usageURL := strings.TrimSuffix(baseURL, "/") + "/usage"

	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, usageURL, nil)
	if errReq != nil {
		return false, fmt.Errorf("build request: %w", errReq)
	}
	applyCodexWarmupHeaders(req, auth, token, false)

	// Use the same timeout as other management API calls (intentional exception for management endpoints,
	// consistent with defaultAPICallTimeout used in api_tools.go).
	httpClient := &http.Client{
		Timeout:   defaultAPICallTimeout,
		Transport: h.apiCallTransport(auth),
	}

	resp, errDo := httpClient.Do(req)
	if errDo != nil {
		return false, fmt.Errorf("execute request: %w", errDo)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("codex warmup: close usage response body error: %v", errClose)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return false, fmt.Errorf("usage endpoint returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}

	body, errRead := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if errRead != nil {
		return false, fmt.Errorf("read usage response: %w", errRead)
	}

	return parseCodexQuotaUsed(body), nil
}

// parseCodexQuotaUsed returns true when the response indicates that requests have been
// made in the current period (quota is below 100%). Returns false when usage is zero
// (quota is still at 100%).
func parseCodexQuotaUsed(body []byte) bool {
	// usedCountFields lists JSON field names that represent the number of requests already
	// consumed in the current period. A zero value means the quota has not been used yet.
	usedCountFields := []string{
		"period_requests", "requests", "request_count", "count",
		"requests_used", "weekly_requests_used", "five_hour_requests_used",
		"used", "weekly_used", "five_hour_used",
		"tokens_used", "weekly_tokens_used",
	}

	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		// Cannot parse the response — assume quota is NOT used so warmup proceeds.
		// Skipping a needed warmup defeats the purpose of this endpoint.
		log.Debugf("codex warmup: cannot parse usage response JSON, assuming fresh quota: %v", err)
		return false
	}

	// Check top-level fields for request-count style responses.
	if used, determined := extractCodexUsedFromMap(raw, usedCountFields); determined {
		return used
	}

	// Check nested objects for multi-period responses, e.g.:
	//   {"weekly": {"requests": 0, "limit": 100}, "five_hour": {"requests": 0, "limit": 40}}
	for _, key := range []string{"weekly", "five_hour", "usage", "data", "limits"} {
		if nested, ok := raw[key].(map[string]any); ok {
			if used, determined := extractCodexUsedFromMap(nested, usedCountFields); determined {
				return used
			}
		}
	}

	// Cannot determine usage from the response — assume quota is NOT used so warmup proceeds.
	// Doing an unnecessary warmup consumes at most one request, which is acceptable.
	// Skipping a needed warmup defeats the entire purpose of this endpoint.
	log.Debugf("codex warmup: no recognized usage fields in response, assuming fresh quota")
	return false
}

// extractCodexUsedFromMap scans m for any field in usedFields.
// It returns (used bool, determined bool) where determined=true means a conclusion was reached.
func extractCodexUsedFromMap(m map[string]any, usedFields []string) (used bool, determined bool) {
	for _, field := range usedFields {
		val, ok := m[field]
		if !ok {
			continue
		}
		n := codexWarmupToFloat64(val)
		if n == 0 {
			return false, true // no requests consumed — quota at 100%
		}
		if n > 0 {
			return true, true // quota already partially used
		}
	}
	return false, false
}

func codexWarmupToFloat64(v any) float64 {
	switch typed := v.(type) {
	case float64:
		return typed
	case float32:
		return float64(typed)
	case int:
		return float64(typed)
	case int64:
		return float64(typed)
	case json.Number:
		if f, err := typed.Float64(); err == nil {
			return f
		}
	}
	return -1
}

// codexWarmupRequestBody is the minimal Codex Responses API request used for quota warmup.
type codexWarmupRequestBody struct {
	Model        string               `json:"model"`
	Stream       bool                 `json:"stream"`
	Instructions string               `json:"instructions"`
	Input        []codexWarmupMessage `json:"input"`
}

type codexWarmupMessage struct {
	Type    string               `json:"type"`
	Role    string               `json:"role"`
	Content []codexWarmupContent `json:"content"`
}

type codexWarmupContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// sendCodexWarmupRequest sends a minimal request to gpt-5.4-mini to activate the weekly quota.
func (h *Handler) sendCodexWarmupRequest(ctx context.Context, auth *coreauth.Auth, token string) error {
	baseURL := codexWarmupBaseURL
	if auth.Attributes != nil {
		if u := strings.TrimSpace(auth.Attributes["base_url"]); u != "" {
			baseURL = u
		}
	}
	responsesURL := strings.TrimSuffix(baseURL, "/") + "/responses"

	warmupBody := codexWarmupRequestBody{
		Model:        codexWarmupModel,
		Stream:       false,
		Instructions: "",
		Input: []codexWarmupMessage{
			{
				Type: "message",
				Role: "user",
				Content: []codexWarmupContent{
					{Type: "input_text", Text: "hi"},
				},
			},
		},
	}
	bodyBytes, errMarshal := json.Marshal(warmupBody)
	if errMarshal != nil {
		return fmt.Errorf("marshal warmup body: %w", errMarshal)
	}

	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, responsesURL, bytes.NewReader(bodyBytes))
	if errReq != nil {
		return fmt.Errorf("build request: %w", errReq)
	}
	applyCodexWarmupHeaders(req, auth, token, false)

	// Use the same timeout as other management API calls (intentional exception for management endpoints,
	// consistent with defaultAPICallTimeout used in api_tools.go).
	httpClient := &http.Client{
		Timeout:   defaultAPICallTimeout,
		Transport: h.apiCallTransport(auth),
	}

	resp, errDo := httpClient.Do(req)
	if errDo != nil {
		return fmt.Errorf("execute request: %w", errDo)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("codex warmup: close warmup response body error: %v", errClose)
		}
	}()

	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("warmup request returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}

	return nil
}

// applyCodexWarmupHeaders sets the standard Codex OAuth headers on r.
func applyCodexWarmupHeaders(r *http.Request, auth *coreauth.Auth, token string, stream bool) {
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("User-Agent", codexWarmupUserAgent)
	r.Header.Set("Originator", codexWarmupOriginator)
	if stream {
		r.Header.Set("Accept", "text/event-stream")
	} else {
		r.Header.Set("Accept", "application/json")
	}
	if auth != nil && auth.Metadata != nil {
		if accountID, ok := auth.Metadata["account_id"].(string); ok && strings.TrimSpace(accountID) != "" {
			r.Header.Set("Chatgpt-Account-Id", strings.TrimSpace(accountID))
		}
	}
}
