package llm

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/daknoblo/ai-ui/internal/foundry"
)

func TestResponsesToolCallPreservesStatelessContext(t *testing.T) {
	var request responseRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/openai/v1/responses" || r.Header.Get("Authorization") == "" {
			t.Errorf("unexpected responses request: %s", r.URL)
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"Creating it.\"}\n\n")
		_, _ = fmt.Fprint(w, `data: {"type":"response.completed","response":{"status":"completed","model":"gpt-6-astra","usage":{"input_tokens":12,"output_tokens":8,"total_tokens":20},"output":[{"type":"reasoning","id":"r1","summary":[],"encrypted_content":"encrypted-test-context"},{"type":"function_call","id":"item1","call_id":"call1","name":"generate_image","arguments":"{\"prompt\":\"A park\",\"edit\":false}"}]}}`+"\n\n")
	}))
	defer server.Close()
	client, store := identityClient(t, server.URL)
	snapshot := store.FoundryStatus().Catalog
	snapshot.Deployments = append(snapshot.Deployments, foundry.Deployment{
		Name: "smart", ModelName: "gpt-6-astra", ModelFormat: "OpenAI", ProvisioningState: "Succeeded",
	})
	if err := store.SetCatalog(snapshot); err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	turn, err := client.ChatStreamWithTools(t.Context(), ChatOptions{Model: "smart", ReasoningEffort: "none"},
		[]Message{{Role: "user", Content: "Draw a park"}},
		[]Tool{{Type: "function", Function: ToolFunction{Name: "generate_image", Parameters: json.RawMessage(`{"type":"object"}`)}}},
		func(delta string) error { output.WriteString(delta); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if request.Model != "smart" || request.Store || !request.Stream ||
		request.Reasoning != nil || !reflect.DeepEqual(request.Include, []string{"reasoning.encrypted_content"}) {
		t.Fatalf("incorrect request settings: %+v", request)
	}
	if output.String() != "Creating it." || turn.Usage.TotalTokens != 20 ||
		len(turn.ToolCalls) != 1 || turn.ToolCalls[0].ID != "call1" || turn.FinishReason != "tool_calls" {
		t.Fatalf("incorrect response: %+v", turn)
	}
	inputs, err := responseInputs([]Message{
		{Role: "assistant", Content: turn.Content, ToolCalls: turn.ToolCalls, ResponseItems: turn.ResponseItems},
		{Role: "tool", ToolCallID: "call1", Content: ""},
	})
	if err != nil || len(inputs) != 3 {
		t.Fatalf("continuation = %v, %v", inputs, err)
	}
	if !strings.Contains(string(inputs[0]), "encrypted-test-context") || !strings.Contains(string(inputs[2]), `"output":""`) {
		t.Fatal("reasoning context or empty tool output was lost")
	}
}

func TestResponsesDoesNotExecuteIncompleteTools(t *testing.T) {
	for _, payload := range []string{
		`data: {"type":"response.incomplete","response":{"status":"incomplete","output":[{"type":"function_call","call_id":"c1","name":"generate_image","arguments":"{}"}]}}` + "\n\n",
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n",
		"data: invalid\n\n",
	} {
		t.Run(payload[:min(40, len(payload))], func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = fmt.Fprint(w, payload)
			}))
			defer server.Close()
			client, _ := identityClient(t, server.URL)
			result, err := client.responsesTurn(t.Context(), ChatOptions{},
				[]Message{{Role: "user", Content: "hello"}}, nil, func(string) error { return nil })
			if err == nil || len(result.ToolCalls) != 0 {
				t.Fatalf("incomplete response made a tool executable: %+v, %v", result, err)
			}
		})
	}
}
