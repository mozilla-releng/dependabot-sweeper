package llmutil

import (
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

func TestFirstText(t *testing.T) {
	tests := []struct {
		name   string
		blocks []anthropic.ContentBlockUnion
		want   string
	}{
		{"empty", nil, ""},
		{"single text", []anthropic.ContentBlockUnion{{Type: "text", Text: "hello"}}, "hello"},
		{
			"thinking before text",
			[]anthropic.ContentBlockUnion{
				{Type: "thinking", Thinking: "let me reason..."},
				{Type: "text", Text: "the answer"},
			},
			"the answer",
		},
		{
			"no text block",
			[]anthropic.ContentBlockUnion{{Type: "thinking", Thinking: "only thinking"}},
			"",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := &anthropic.Message{Content: tt.blocks}
			if got := FirstText(msg); got != tt.want {
				t.Errorf("FirstText() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTruncated(t *testing.T) {
	tests := []struct {
		name   string
		reason anthropic.StopReason
		want   bool
	}{
		{"end_turn", anthropic.StopReasonEndTurn, false},
		{"max_tokens", anthropic.StopReasonMaxTokens, true},
		{"stop_sequence", anthropic.StopReasonStopSequence, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := &anthropic.Message{StopReason: tt.reason}
			if got := Truncated(msg); got != tt.want {
				t.Errorf("Truncated(%s) = %v, want %v", tt.reason, got, tt.want)
			}
		})
	}
}

func TestExtractJSON(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain object", `{"a":1}`, `{"a":1}`},
		{"leading/trailing space", "  {\"a\":1}\n", `{"a":1}`},
		{"fenced json", "```json\n{\"a\":1}\n```", `{"a":1}`},
		{"fenced no lang", "```\n{\"a\":1}\n```", `{"a":1}`},
		{"preamble prose", "Here is the result:\n{\"a\":1}", `{"a":1}`},
		{"trailing prose", "{\"a\":1}\nHope that helps!", `{"a":1}`},
		{"nested braces", `{"a":{"b":2}}`, `{"a":{"b":2}}`},
		{"no braces", "not json at all", "not json at all"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ExtractJSON(tt.in); got != tt.want {
				t.Errorf("ExtractJSON(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
