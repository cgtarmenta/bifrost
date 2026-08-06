//go:build !tinygo && !wasm

package starlark

import (
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
)

// TestCreateToolResponseMessageMarksError pins the CodeMode copy of the tool
// response builder. CodeMode reports its own failures -- unknown server, unknown
// tool, ambiguous filename, failed sandbox execution -- as ordinary result text,
// so without the marker the model reads a lookup failure as a successful answer.
func TestCreateToolResponseMessageMarksError(t *testing.T) {
	id := "call_1"
	name := "readToolFile"
	toolCall := schemas.ChatAssistantMessageToolCall{
		ID: &id,
		Function: schemas.ChatAssistantMessageToolCallFunction{
			Name:      &name,
			Arguments: `{"path":"servers/missing.pyi"}`,
		},
	}

	failed := createToolResponseMessage(toolCall, "No server found matching 'missing'", true)
	if failed.ChatToolMessage == nil {
		t.Fatal("expected ChatToolMessage to be attached")
	}
	if failed.ChatToolMessage.IsError == nil || !*failed.ChatToolMessage.IsError {
		t.Fatal("a CodeMode failure must set IsError true")
	}

	ok := createToolResponseMessage(toolCall, "def read_file(path: str) -> str", false)
	if ok.ChatToolMessage == nil {
		t.Fatal("expected ChatToolMessage to be attached")
	}
	if ok.ChatToolMessage.IsError != nil {
		t.Fatalf("a successful CodeMode result must leave IsError nil, got %v", *ok.ChatToolMessage.IsError)
	}
}
