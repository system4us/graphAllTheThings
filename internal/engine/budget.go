package engine

import (
	"context"
	"fmt"
	"strings"
)

// TrimToTokenBudget trims a context pack cleanly at section/line boundaries so
// it stays within ~maxTokens (approximated at 4 chars/token). maxTokens <= 0
// returns text unchanged.
func TrimToTokenBudget(text string, maxTokens int) string {
	if maxTokens <= 0 {
		return text
	}
	maxChars := maxTokens * 4
	if len(text) <= maxChars {
		return text
	}
	// Prefer cutting at a section boundary ("\n## ") so we don't slice mid-function.
	cut := strings.LastIndex(text[:maxChars], "\n## ")
	if cut < maxChars/3 {
		cut = strings.LastIndex(text[:maxChars], "\n")
	}
	if cut <= 0 {
		cut = maxChars
	}
	return strings.TrimRight(text[:cut], "\n") +
		fmt.Sprintf("\n\n… (trimmed to ~%d token budget; increase max_tokens for more)\n", maxTokens)
}

// CodeContextPackWithBudget is CodeContextPack with an optional approximate
// token budget (0 = default unbudgeted pack).
func (e *Engine) CodeContextPackWithBudget(ctx context.Context, question string, limit, maxTokens int) (string, error) {
	out, err := e.codeContextPack(ctx, question, limit, maxTokens)
	if err != nil {
		return "", err
	}
	// The pack already chose whole sections by priority; this only catches a
	// single section larger than the whole budget.
	return TrimToTokenBudget(out, maxTokens+maxTokens/10), nil
}

// ContextPackWithBudget is ContextPack with an optional approximate token
// budget (0 = default unbudgeted pack).
func (e *Engine) ContextPackWithBudget(ctx context.Context, question string, limit, maxTokens int) (string, error) {
	out, err := e.ContextPack(ctx, question, limit)
	if err != nil {
		return "", err
	}
	return TrimToTokenBudget(out, maxTokens), nil
}
