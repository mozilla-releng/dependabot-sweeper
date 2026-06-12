// Package llmutil holds small helpers shared by the agents that call the
// Anthropic API (analyser, reviewer) for extracting usable content from a
// model response.
package llmutil

import (
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
)

// FirstText returns the text of the first text content block in the message,
// skipping non-text blocks such as thinking / redacted-thinking blocks. When
// extended thinking is enabled the first block is a thinking block, so indexing
// Content[0].Text directly would yield an empty string — this walks the blocks
// and returns the first actual text. Returns "" if there is no text block.
func FirstText(msg *anthropic.Message) string {
	for i := range msg.Content {
		if msg.Content[i].Type == "text" {
			return msg.Content[i].Text
		}
	}
	return ""
}

// Truncated reports whether the model stopped because it hit the max_tokens
// output cap. When true the response is cut off and any JSON in it is almost
// certainly incomplete, so callers should treat it as a failure and surface it
// — an undersized output budget is then visible in logs/output instead of
// masquerading as a vague JSON parse error.
func Truncated(msg *anthropic.Message) bool {
	return msg.StopReason == anthropic.StopReasonMaxTokens
}

// ExtractJSON pulls the outermost JSON object out of a model response that may
// be wrapped in markdown code fences or surrounded by prose (a preamble
// sentence before the block, a trailing note after it). It returns the
// substring from the first '{' to the last '}' inclusive. If no braces are
// found it returns the trimmed input unchanged, so the caller's json.Unmarshal
// still produces a meaningful error.
func ExtractJSON(raw string) string {
	s := strings.TrimSpace(raw)
	start := strings.IndexByte(s, '{')
	end := strings.LastIndexByte(s, '}')
	if start == -1 || end == -1 || end < start {
		return s
	}
	return s[start : end+1]
}
