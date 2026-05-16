package xai

import (
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/model"
)

func TestExtractUpstreamTaskID(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "official request id", body: `{"request_id":"req_123"}`, want: "req_123"},
		{name: "third party id", body: `{"id":"vid_123"}`, want: "vid_123"},
		{name: "legacy task id", body: `{"task_id":"task_upstream"}`, want: "task_upstream"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := extractUpstreamTaskID([]byte(tt.body)); got != tt.want {
				t.Fatalf("extractUpstreamTaskID() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSanitizeSubmitResponse(t *testing.T) {
	got, err := sanitizeSubmitResponse([]byte(`{"request_id":"upstream_secret","status":"pending"}`), "task_public", "grok-video-3-max")
	if err != nil {
		t.Fatal(err)
	}
	body := string(got)
	if body == "" {
		t.Fatal("empty body")
	}
	for _, forbidden := range []string{"upstream_secret"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("sanitized body leaked %q: %s", forbidden, body)
		}
	}
	for _, required := range []string{"task_public", "grok-video-3-max"} {
		if !strings.Contains(body, required) {
			t.Fatalf("sanitized body missing %q: %s", required, body)
		}
	}
}

func TestParseTaskResultPayloadStatusAndURL(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStatus string
		wantURL    string
		wantReason string
	}{
		{
			name:       "pending",
			body:       `{"status":"pending","progress":0.2}`,
			wantStatus: string(model.TaskStatusQueued),
		},
		{
			name:       "done with nested video url",
			body:       `{"status":"done","video":{"url":"https://cdn.example/video.mp4"}}`,
			wantStatus: string(model.TaskStatusSuccess),
			wantURL:    "https://cdn.example/video.mp4",
		},
		{
			name:       "failed with error",
			body:       `{"status":"failed","error":{"message":"quota exceeded"}}`,
			wantStatus: string(model.TaskStatusFailure),
			wantReason: "quota exceeded",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseTaskResultPayload([]byte(tt.body))
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != tt.wantStatus {
				t.Fatalf("status = %q, want %q", got.Status, tt.wantStatus)
			}
			if got.Url != tt.wantURL {
				t.Fatalf("url = %q, want %q", got.Url, tt.wantURL)
			}
			if got.Reason != tt.wantReason {
				t.Fatalf("reason = %q, want %q", got.Reason, tt.wantReason)
			}
		})
	}
}

func TestBuildRequestPathMode(t *testing.T) {
	if useOpenAIVideoCreatePath("https://api.x.ai") {
		t.Fatal("official xAI should use /v1/videos/generations")
	}
	if !useOpenAIVideoCreatePath("https://www.geeknow.top") {
		t.Fatal("geeknow-compatible upstream should use /v1/videos")
	}
}
