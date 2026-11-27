package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ChatClient calls a local Ollama chat/completion model — distinct from
// Client, which only ever calls the embedding endpoint. bge-m3 and other
// embedding models can't serve this; callers must point ChatClient at an
// actual chat model (qwen2.5-coder, llama3.2, mistral, ...).
type ChatClient struct {
	URL   string
	Model string
	http  *http.Client
}

func NewChat(url, model string) *ChatClient {
	if url == "" {
		url = DefaultURL
	}
	return &ChatClient{URL: url, Model: model, http: &http.Client{Timeout: 30 * time.Second}}
}

// Expand asks the chat model for up to n alternate phrasings of a codebase
// search query, using concrete implementation vocabulary (the kind of word
// that shows up literally in identifiers/comments) instead of the query's
// own abstract wording — "prevent a duplicate request" has no lexical
// overlap with the code that actually handles it, which might be named and
// commented around "idempotency key" instead. Returns query itself first,
// followed by whatever alternates the model produced (best-effort: a
// malformed response, an unpulled model, or a network error all just yield
// []string{query} — expansion is a quality booster, never a hard
// dependency, so its failure must never fail the search it's expanding).
func (c *ChatClient) Expand(ctx context.Context, query string, n int) []string {
	out := []string{query}
	if c == nil || n <= 0 {
		return out
	}
	// Naming the pattern outright ("idempotency", "memoization", "debouncing",
	// "rate limiting") lands closer to real identifier/file names than a
	// generic rephrasing does — tested directly against ollama: asking flat
	// for "N alternate phrasings" missed the file name it was going for in
	// 3 of 4 tries; asking for the pattern name first hit it 3 of 3.
	prompt := "A developer is searching a codebase with a plain-English description of what they want. Your job: " +
		"name the standard software engineering pattern or technical term for what they describe (e.g. idempotency, " +
		"memoization, debouncing, rate limiting, caching, backoff, deduplication) if one clearly applies, as the " +
		"FIRST line. Then give " + strconv.Itoa(n-1) + " more short phrasing(s) using concrete implementation " +
		"vocabulary — the kind of word that shows up in a file name, identifier, or code comment, not the query's " +
		"own abstract wording. One item per line, no numbering, no extra commentary.\n\nQuery: " + query

	body, err := json.Marshal(map[string]any{
		"model":  c.Model,
		"prompt": prompt,
		"stream": false,
	})
	if err != nil {
		return out
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL+"/api/generate", bytes.NewReader(body))
	if err != nil {
		return out
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return out
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return out
	}
	var parsed struct {
		Response string `json:"response"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return out
	}
	for line := range strings.SplitSeq(parsed.Response, "\n") {
		line = strings.TrimSpace(strings.Trim(line, "-*• \t"))
		if line == "" || strings.EqualFold(line, query) {
			continue
		}
		out = append(out, line)
		if len(out) > n {
			break
		}
	}
	return out
}
