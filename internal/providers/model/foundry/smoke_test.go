//go:build foundrysmoke

/*
Copyright 2026 The Shadewright Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package foundry

// A manual smoke test against a real deployment. It is built only with the foundrysmoke tag and skipped
// unless FOUNDRY_ENDPOINT and FOUNDRY_DEPLOYMENT are set; CI never runs it. Each run makes two small model
// calls (a few hundred tokens). It authenticates with workload identity when AZURE_FEDERATED_TOKEN_FILE is
// set, otherwise with the Azure CLI's signed-in user (`az account get-access-token`), which needs the
// Cognitive Services OpenAI User role on the account. It prints the model, the token usage and the
// attempts, never the prompt, the reply or the token:
//
//	FOUNDRY_ENDPOINT=https://<resource>.services.ai.azure.com/ FOUNDRY_DEPLOYMENT=triage \
//	  go test -tags foundrysmoke -run Smoke -v -count=1 ./internal/providers/model/foundry/
//
// FOUNDRY_API_VERSION selects the API version (default v1).

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/pkalin/shadewright/internal/providers"
)

// azCLITokens asks the Azure CLI for a Cognitive Services token. For the manual smoke test only.
type azCLITokens struct{}

func (azCLITokens) Token(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "az", "account", "get-access-token", "--resource",
		cognitiveServicesAudience, "--query", "accessToken", "-o", "tsv").Output()
	if err != nil {
		return "", errors.New("az account get-access-token failed (is the Azure CLI signed in?)")
	}
	return strings.TrimSpace(string(out)), nil
}

func smokeProvider(t *testing.T) *Provider {
	t.Helper()
	endpoint, deployment := os.Getenv("FOUNDRY_ENDPOINT"), os.Getenv("FOUNDRY_DEPLOYMENT")
	if endpoint == "" || deployment == "" {
		t.Skip("FOUNDRY_ENDPOINT and FOUNDRY_DEPLOYMENT are not set")
	}
	version := os.Getenv("FOUNDRY_API_VERSION")
	if version == "" {
		version = "v1"
	}
	var tokens TokenSource = azCLITokens{}
	if os.Getenv(envTokenFile) != "" {
		wi, err := NewWorkloadIdentity()
		if err != nil {
			t.Fatalf("workload identity: %v", err)
		}
		tokens = wi
	}
	p, err := New(Config{
		Endpoint: endpoint, Deployment: deployment, APIVersion: version, Tokens: tokens, MaxOutputTokens: 1024,
		OnCall: func(r CallReport) {
			t.Logf("call: model=%s input=%d cached=%d output=%d attempts=%d duration=%s err=%v",
				r.Model, r.Usage.InputTokens, r.Usage.CachedInputTokens, r.Usage.OutputTokens, r.Attempts,
				r.Duration.Round(time.Millisecond), r.Err)
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func TestSmokeStructuredOutput(t *testing.T) {
	p := smokeProvider(t)
	c, err := p.Complete(context.Background(), providers.CompletionRequest{
		Messages: []providers.Message{
			{Role: providers.RoleSystem, Content: "Classify the Kubernetes symptom. Answer in the schema."},
			{Role: providers.RoleUser, Content: "Pod web-0 restarted 5 times; last state: OOMKilled, exit code 137."},
		},
		ResponseSchema: json.RawMessage(`{"type":"object","properties":{"category":{"type":"string",` +
			`"enum":["oom","crashloop","image_pull","other"]}},"required":["category"],"additionalProperties":false}`),
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	var out struct {
		Category string `json:"category"`
	}
	if err := json.Unmarshal([]byte(c.Content), &out); err != nil || out.Category == "" {
		t.Fatalf("structured output did not decode into the schema's type")
	}
	if c.Usage.InputTokens == 0 || c.Usage.OutputTokens == 0 || c.Model == "" {
		t.Errorf("no usage or model: %+v %q", c.Usage, c.Model)
	}
	t.Logf("category=%s", out.Category) // an enum value, not free text
}

func TestSmokeToolCall(t *testing.T) {
	p := smokeProvider(t)
	c, err := p.Complete(context.Background(), providers.CompletionRequest{
		Messages: []providers.Message{
			{Role: providers.RoleSystem, Content: "Use the tool to read the logs before answering."},
			{Role: providers.RoleUser, Content: "Why is pod web-0 in namespace shop failing?"},
		},
		Tools: []providers.ToolDefinition{{
			Name: "get_pod_logs", Description: "Reads the last log lines of a pod.",
			Parameters: json.RawMessage(`{"type":"object","properties":{"namespace":{"type":"string"},` +
				`"pod":{"type":"string"}},"required":["namespace","pod"],"additionalProperties":false}`),
		}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(c.ToolCalls) == 0 || c.ToolCalls[0].Name != "get_pod_logs" {
		t.Fatalf("expected a call of get_pod_logs, got %d tool calls", len(c.ToolCalls))
	}
}
