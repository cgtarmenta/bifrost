package deepseek_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/maximhq/bifrost/core/internal/llmtests"
	schemas "github.com/maximhq/bifrost/core/schemas"
)

func newOpenAIChatResponse() string {
	return `{"id":"chatcmpl_1","object":"chat.completion","model":"deepseek-v4-flash","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
}

// captureDeepSeekChatBody drives request through DeepSeek's native
// OpenAI-compatible chat path and returns the decoded outbound wire body.
func captureDeepSeekChatBody(t *testing.T, request *schemas.BifrostChatRequest) map[string]any {
	t.Helper()

	var captured map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %q, want /chat/completions", r.URL.Path)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
			return
		}
		if err := json.Unmarshal(body, &captured); err != nil {
			t.Errorf("decode body: %v", err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, newOpenAIChatResponse())
	}))
	defer server.Close()

	provider, err := newTestDeepSeekProvider(server.URL)
	if err != nil {
		t.Fatalf("NewDeepSeekProvider: %v", err)
	}

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	key := schemas.Key{Value: schemas.SecretVar{Val: "test-api-key"}}
	if _, bifrostErr := provider.ChatCompletion(ctx, key, request); bifrostErr != nil {
		t.Fatalf("ChatCompletion: %v", bifrostErr.Error.Message)
	}
	return captured
}

// assistantMessageAt returns the decoded message at index i of the wire body.
func assistantMessageAt(t *testing.T, captured map[string]any, i int) map[string]any {
	t.Helper()
	messages, ok := captured["messages"].([]any)
	if !ok || len(messages) <= i {
		t.Fatalf("expected at least %d messages in wire payload, got %#v", i+1, captured["messages"])
	}
	msg, ok := messages[i].(map[string]any)
	if !ok {
		t.Fatalf("expected message object at index %d, got %#v", i, messages[i])
	}
	return msg
}

// TestChatCompletion_OpenAIEndpointKeepsThinkingForPlainMultiTurn reproduces #5887.
// DeepSeek enables thinking by default and does not require reasoning_content to be
// replayed when the history contains no tool calls, so Bifrost must not send
// thinking:{"type":"disabled"} for an ordinary multi-turn conversation.
func TestChatCompletion_OpenAIEndpointKeepsThinkingForPlainMultiTurn(t *testing.T) {
	t.Parallel()

	first := "What's the weather in Beijing?"
	answer := "Sunny, 32°C."
	followUp := "Which is bigger, 9.9 or 9.11? Think carefully."

	captured := captureDeepSeekChatBody(t, &schemas.BifrostChatRequest{
		Provider: schemas.DeepSeek,
		Model:    "deepseek-v4-flash",
		Input: []schemas.ChatMessage{
			{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: &first}},
			{
				Role:                 schemas.ChatMessageRoleAssistant,
				Content:              &schemas.ChatMessageContent{ContentStr: &answer},
				ChatAssistantMessage: &schemas.ChatAssistantMessage{},
			},
			{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: &followUp}},
		},
		// Params must be non-nil: the gate short-circuits on nil Params, so a
		// nil-Params request would pass this test without exercising anything.
		Params: &schemas.ChatParameters{MaxCompletionTokens: schemas.Ptr(3000)},
	})

	if thinking, ok := captured["thinking"]; ok {
		t.Fatalf("thinking must be absent for plain multi-turn (DeepSeek defaults thinking on), got %#v", thinking)
	}
}

// TestChatCompletion_OpenAIEndpointKeepsThinkingWhenToolCallReasoningReplayed covers the
// tool-calling variation of #5887: the caller correctly replays reasoning_content on the
// assistant tool-call turn, so thinking must stay on AND the replayed reasoning_content
// must survive to the wire (DeepSeek 400s on a tool-call turn missing it).
func TestChatCompletion_OpenAIEndpointKeepsThinkingWhenToolCallReasoningReplayed(t *testing.T) {
	t.Parallel()

	chatTool := llmtests.GetSampleChatTool(llmtests.SampleToolTypeTime)
	if chatTool == nil {
		t.Fatal("GetSampleChatTool returned nil")
	}

	ask := "what time is it in UTC?"
	reasoning := "the user wants the current UTC time, call the tool"
	toolResult := "2026-08-05T12:00:00Z"
	answer := "It is 12:00 UTC."
	followUp := "Which is bigger, 9.9 or 9.11? Think carefully."
	toolCallID := "call_1"
	toolName := "get_current_time"

	captured := captureDeepSeekChatBody(t, &schemas.BifrostChatRequest{
		Provider: schemas.DeepSeek,
		Model:    "deepseek-v4-flash",
		Input: []schemas.ChatMessage{
			{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: &ask}},
			{
				Role: schemas.ChatMessageRoleAssistant,
				ChatAssistantMessage: &schemas.ChatAssistantMessage{
					Reasoning: &reasoning,
					ToolCalls: []schemas.ChatAssistantMessageToolCall{{
						ID:       &toolCallID,
						Function: schemas.ChatAssistantMessageToolCallFunction{Name: &toolName, Arguments: `{}`},
					}},
				},
			},
			{
				Role:            schemas.ChatMessageRoleTool,
				Content:         &schemas.ChatMessageContent{ContentStr: &toolResult},
				ChatToolMessage: &schemas.ChatToolMessage{ToolCallID: &toolCallID},
			},
			{
				Role:                 schemas.ChatMessageRoleAssistant,
				Content:              &schemas.ChatMessageContent{ContentStr: &answer},
				ChatAssistantMessage: &schemas.ChatAssistantMessage{},
			},
			{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: &followUp}},
		},
		Params: &schemas.ChatParameters{
			MaxCompletionTokens: schemas.Ptr(3000),
			Tools:               []schemas.ChatTool{*chatTool},
		},
	})

	if thinking, ok := captured["thinking"]; ok {
		t.Fatalf("thinking must stay on when tool-call reasoning_content is replayed, got %#v", thinking)
	}
	toolCallMsg := assistantMessageAt(t, captured, 1)
	if got, ok := toolCallMsg["reasoning_content"].(string); !ok || got != reasoning {
		t.Fatalf("reasoning_content must be preserved on the assistant tool-call turn, got %#v", toolCallMsg["reasoning_content"])
	}
}

// TestChatCompletion_OpenAIEndpointKeepsThinkingForReasoningDetailsOnly covers callers that
// replay reasoning as reasoning_details rather than reasoning_content.
func TestChatCompletion_OpenAIEndpointKeepsThinkingForReasoningDetailsOnly(t *testing.T) {
	t.Parallel()

	answer := "Sunny, 32°C."
	followUp := "Which is bigger, 9.9 or 9.11? Think carefully."
	reasoning := "recalling Beijing weather"

	captured := captureDeepSeekChatBody(t, &schemas.BifrostChatRequest{
		Provider: schemas.DeepSeek,
		Model:    "deepseek-v4-flash",
		Input: []schemas.ChatMessage{
			{
				Role:    schemas.ChatMessageRoleAssistant,
				Content: &schemas.ChatMessageContent{ContentStr: &answer},
				ChatAssistantMessage: &schemas.ChatAssistantMessage{
					ReasoningDetails: []schemas.ChatReasoningDetails{{
						Index: 0,
						Type:  schemas.BifrostReasoningDetailsTypeText,
						Text:  &reasoning,
					}},
				},
			},
			{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: &followUp}},
		},
		Params: &schemas.ChatParameters{MaxCompletionTokens: schemas.Ptr(3000)},
	})

	if thinking, ok := captured["thinking"]; ok {
		t.Fatalf("thinking must stay on when reasoning is replayed as reasoning_details, got %#v", thinking)
	}
}

// TestChatCompletion_OpenAIEndpointDisablesThinkingForToolCallWithoutReasoning guards the
// safety net that must survive the fix: a tool-call turn genuinely missing reasoning_content
// would 400 upstream with thinking on, so thinking is forced off.
// This test is expected to pass both before and after the fix.
func TestChatCompletion_OpenAIEndpointDisablesThinkingForToolCallWithoutReasoning(t *testing.T) {
	t.Parallel()

	chatTool := llmtests.GetSampleChatTool(llmtests.SampleToolTypeTime)
	if chatTool == nil {
		t.Fatal("GetSampleChatTool returned nil")
	}

	ask := "what time is it in UTC?"
	toolResult := "2026-08-05T12:00:00Z"
	toolCallID := "call_1"
	toolName := "get_current_time"

	captured := captureDeepSeekChatBody(t, &schemas.BifrostChatRequest{
		Provider: schemas.DeepSeek,
		Model:    "deepseek-v4-flash",
		Input: []schemas.ChatMessage{
			{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: &ask}},
			{
				Role: schemas.ChatMessageRoleAssistant,
				ChatAssistantMessage: &schemas.ChatAssistantMessage{
					ToolCalls: []schemas.ChatAssistantMessageToolCall{{
						ID:       &toolCallID,
						Function: schemas.ChatAssistantMessageToolCallFunction{Name: &toolName, Arguments: `{}`},
					}},
				},
			},
			{
				Role:            schemas.ChatMessageRoleTool,
				Content:         &schemas.ChatMessageContent{ContentStr: &toolResult},
				ChatToolMessage: &schemas.ChatToolMessage{ToolCallID: &toolCallID},
			},
		},
		Params: &schemas.ChatParameters{
			MaxCompletionTokens: schemas.Ptr(3000),
			Tools:               []schemas.ChatTool{*chatTool},
		},
	})

	thinking, ok := captured["thinking"].(map[string]any)
	if !ok {
		t.Fatalf("expected thinking block in outbound body, got %#v", captured)
	}
	if got := thinking["type"]; got != "disabled" {
		t.Fatalf("thinking.type = %v, want disabled", got)
	}
}

// TestChatCompletion_DoesNotMutateCallerExtraParams ensures the thinking compatibility
// shim does not write through to the caller's shared ChatParameters, which would leak
// thinking:disabled into fallback attempts against other providers.
func TestChatCompletion_DoesNotMutateCallerExtraParams(t *testing.T) {
	t.Parallel()

	chatTool := llmtests.GetSampleChatTool(llmtests.SampleToolTypeTime)
	if chatTool == nil {
		t.Fatal("GetSampleChatTool returned nil")
	}

	ask := "what time is it in UTC?"
	toolCallID := "call_1"
	toolName := "get_current_time"
	params := &schemas.ChatParameters{
		MaxCompletionTokens: schemas.Ptr(3000),
		Tools:               []schemas.ChatTool{*chatTool},
	}

	captureDeepSeekChatBody(t, &schemas.BifrostChatRequest{
		Provider: schemas.DeepSeek,
		Model:    "deepseek-v4-flash",
		Input: []schemas.ChatMessage{
			{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: &ask}},
			{
				Role: schemas.ChatMessageRoleAssistant,
				ChatAssistantMessage: &schemas.ChatAssistantMessage{
					ToolCalls: []schemas.ChatAssistantMessageToolCall{{
						ID:       &toolCallID,
						Function: schemas.ChatAssistantMessageToolCallFunction{Name: &toolName, Arguments: `{}`},
					}},
				},
			},
		},
		Params: params,
	})

	if _, ok := params.ExtraParams["thinking"]; ok {
		t.Fatalf("caller ChatParameters must not be mutated, got ExtraParams=%#v", params.ExtraParams)
	}
}
