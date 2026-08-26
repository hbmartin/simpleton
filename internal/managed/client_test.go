package managed

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/haroldmartin/simpleton/internal/domain"
)

func TestEvidenceRedactorRemovesSecretsRecursively(t *testing.T) {
	pack := domain.EvidencePack{Methods: []domain.MethodResult{{Reason: "token=super-secret-value"}}}
	payload, count, err := NewRedactor().Evidence(pack)
	if err != nil {
		t.Fatal(err)
	}
	if count == 0 || strings.Contains(string(payload), "super-secret-value") || !strings.Contains(string(payload), "[REDACTED]") {
		t.Fatalf("secret was not redacted: %s", payload)
	}
}

func TestTelemetryUploadCarriesDataControls(t *testing.T) {
	deleted := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodDelete {
			deleted = true
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if request.Header.Get("X-Simpleton-Retention-Days") != "30" || request.Header.Get("X-Simpleton-Training") != "prohibited" || request.Header.Get("X-Simpleton-Cross-Customer-Use") != "prohibited" {
			t.Errorf("missing data-control headers: %#v", request.Header)
		}
		if request.Header.Get("X-Simpleton-Organization") != "acme" || request.Header.Get("X-Simpleton-Repository") != "acme/repo" {
			t.Errorf("missing tenant headers")
		}
		_, _ = io.WriteString(w, `{"id":"remote-1"}`)
	}))
	defer server.Close()
	client := TelemetryClient{Endpoint: server.URL, AllowInsecure: true}
	identifier, err := client.Upload(context.Background(), []byte(`{}`), "acme", "acme/repo")
	if err != nil || identifier != "remote-1" {
		t.Fatalf("unexpected upload result id=%q err=%v", identifier, err)
	}
	if err := client.Delete(context.Background(), identifier); err != nil || !deleted {
		t.Fatalf("managed evidence deletion failed: deleted=%t err=%v", deleted, err)
	}
}

func TestFMReviewRemainsAdvisory(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		if strings.Contains(string(body), "model-secret-value") {
			t.Error("FM request was not locally redacted")
		}
		_, _ = io.WriteString(w, `{"explanation":"suspicious branch","suspicion":"medium"}`)
	}))
	defer server.Close()
	review := (FMClient{Endpoint: server.URL, ModelID: "model", PromptID: "prompt", AllowInsecure: true}).Review(context.Background(), FMRequest{
		Methods: []domain.MethodResult{{Reason: "token=model-secret-value"}},
	})
	if review.Status != domain.StatusRan || review.Suspicion != "medium" {
		t.Fatalf("unexpected review: %#v", review)
	}
}

func TestManagedEndpointsRequireEncryption(t *testing.T) {
	if _, err := (TelemetryClient{Endpoint: "http://example.com"}).Upload(context.Background(), nil, "org", "repo"); err == nil {
		t.Fatal("unencrypted telemetry endpoint must be rejected")
	}
	review := (FMClient{Endpoint: "http://example.com"}).Review(context.Background(), FMRequest{})
	if review.Status != domain.StatusExecutionFailed {
		t.Fatalf("unencrypted FM endpoint must fail: %#v", review)
	}
}
