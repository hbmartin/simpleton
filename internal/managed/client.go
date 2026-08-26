package managed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/haroldmartin/simpleton/internal/domain"
)

type Redactor struct {
	patterns []*regexp.Regexp
}

func NewRedactor() Redactor {
	return Redactor{patterns: []*regexp.Regexp{
		regexp.MustCompile(`(?i)-----BEGIN (?:RSA |EC |OPENSSH )?PRIVATE KEY-----[\s\S]*?-----END (?:RSA |EC |OPENSSH )?PRIVATE KEY-----`),
		regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9_]{20,}\b`),
		regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{20,}\b`),
		regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
		regexp.MustCompile(`\beyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\b`),
		regexp.MustCompile(`(?i)(password|passwd|token|secret|api[_-]?key)\s*[:=]\s*[^\s,;"']+`),
	}}
}

func (r Redactor) Evidence(pack domain.EvidencePack) ([]byte, int, error) {
	return r.JSON(pack)
}

func (r Redactor) JSON(input any) ([]byte, int, error) {
	raw, err := json.Marshal(input)
	if err != nil {
		return nil, 0, err
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, 0, err
	}
	redacted, count := r.walk(value)
	out, err := json.Marshal(redacted)
	return out, count, err
}

func (r Redactor) walk(value any) (any, int) {
	switch typed := value.(type) {
	case map[string]any:
		total := 0
		for key, item := range typed {
			redacted, count := r.walk(item)
			typed[key] = redacted
			total += count
		}
		return typed, total
	case []any:
		total := 0
		for index, item := range typed {
			redacted, count := r.walk(item)
			typed[index] = redacted
			total += count
		}
		return typed, total
	case string:
		result := typed
		total := 0
		for _, pattern := range r.patterns {
			matches := pattern.FindAllStringIndex(result, -1)
			if len(matches) > 0 {
				total += len(matches)
				result = pattern.ReplaceAllString(result, "[REDACTED]")
			}
		}
		return result, total
	default:
		return value, 0
	}
}

type TelemetryClient struct {
	Endpoint      string
	Token         string
	HTTPClient    *http.Client
	AllowInsecure bool
}

func (c TelemetryClient) Upload(ctx context.Context, payload []byte, organization, repository string) (string, error) {
	if c.Endpoint == "" {
		return "", errors.New("managed telemetry endpoint is not configured")
	}
	if err := requireEncryptedEndpoint(c.Endpoint, c.AllowInsecure); err != nil {
		return "", err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+c.Token)
	request.Header.Set("X-Simpleton-Organization", organization)
	request.Header.Set("X-Simpleton-Repository", repository)
	request.Header.Set("X-Simpleton-Retention-Days", "30")
	request.Header.Set("X-Simpleton-Training", "prohibited")
	request.Header.Set("X-Simpleton-Cross-Customer-Use", "prohibited")
	client := c.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	response, err := client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("managed telemetry returned %s: %s", response.Status, strings.TrimSpace(string(body)))
	}
	var result struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", err
	}
	return result.ID, nil
}

func (c TelemetryClient) Delete(ctx context.Context, identifier string) error {
	if c.Endpoint == "" || identifier == "" {
		return errors.New("managed telemetry endpoint and evidence identifier are required")
	}
	if err := requireEncryptedEndpoint(c.Endpoint, c.AllowInsecure); err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodDelete, strings.TrimRight(c.Endpoint, "/")+"/"+url.PathEscape(identifier), nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+c.Token)
	client := c.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		return fmt.Errorf("managed deletion returned %s: %s", response.Status, strings.TrimSpace(string(body)))
	}
	return nil
}

type FMClient struct {
	Endpoint      string
	Token         string
	ModelID       string
	PromptID      string
	HTTPClient    *http.Client
	AllowInsecure bool
}

type FMRequest struct {
	Repository   string                      `json:"repository"`
	BaseRevision string                      `json:"base_revision"`
	HeadRevision string                      `json:"head_revision"`
	Targets      []domain.VerificationTarget `json:"targets"`
	Methods      []domain.MethodResult       `json:"methods"`
	Instruction  string                      `json:"instruction"`
}

func (c FMClient) Review(ctx context.Context, input FMRequest) domain.AdvisoryReview {
	started := time.Now()
	if c.Endpoint == "" {
		return domain.AdvisoryReview{Status: domain.StatusUnsupported, Reason: "Simpleton-managed FM endpoint is not configured"}
	}
	if err := requireEncryptedEndpoint(c.Endpoint, c.AllowInsecure); err != nil {
		return domain.AdvisoryReview{Status: domain.StatusExecutionFailed, Reason: err.Error()}
	}
	payload, _, err := NewRedactor().JSON(input)
	if err != nil {
		return domain.AdvisoryReview{Status: domain.StatusExecutionFailed, Reason: err.Error()}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint, bytes.NewReader(payload))
	if err != nil {
		return domain.AdvisoryReview{Status: domain.StatusExecutionFailed, Reason: err.Error()}
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+c.Token)
	client := c.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	response, err := client.Do(request)
	if err != nil {
		return domain.AdvisoryReview{Status: domain.StatusExecutionFailed, Reason: err.Error()}
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil || response.StatusCode < 200 || response.StatusCode >= 300 {
		if err == nil {
			err = fmt.Errorf("managed FM returned %s", response.Status)
		}
		return domain.AdvisoryReview{Status: domain.StatusExecutionFailed, Reason: err.Error()}
	}
	var result struct {
		Explanation string   `json:"explanation"`
		Suspicion   string   `json:"suspicion"`
		CostUSD     *float64 `json:"cost_usd"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return domain.AdvisoryReview{Status: domain.StatusExecutionFailed, Reason: err.Error()}
	}
	return domain.AdvisoryReview{
		Status: domain.StatusRan, ModelID: c.ModelID, PromptID: c.PromptID,
		Explanation: result.Explanation, Suspicion: result.Suspicion,
		DurationMS: time.Since(started).Milliseconds(), CostUSD: result.CostUSD,
	}
}

func requireEncryptedEndpoint(endpoint string, allowInsecure bool) error {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return err
	}
	if parsed.Scheme != "https" && !allowInsecure {
		return errors.New("managed endpoint must use HTTPS")
	}
	return nil
}
