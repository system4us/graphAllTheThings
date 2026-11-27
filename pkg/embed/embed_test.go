package embed

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestOllamaPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/embed" {
			t.Errorf("path = %s, want /api/embed", r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]any{"embeddings": [][]float32{{1, 2}, {3, 4}}})
	}))
	defer srv.Close()
	got, err := New(srv.URL, "m").Embed(context.Background(), []string{"a", "b"})
	if err != nil || !reflect.DeepEqual(got, [][]float32{{1, 2}, {3, 4}}) {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestOpenAIPathOrderAndAuth(t *testing.T) {
	t.Setenv("GATT_EMBED_API_KEY", "secret")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			t.Errorf("path = %s, want /v1/embeddings", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("missing bearer token")
		}
		// out of order on purpose: index is authoritative
		json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{
			{"index": 1, "embedding": []float32{3, 4}},
			{"index": 0, "embedding": []float32{1, 2}},
		}})
	}))
	defer srv.Close()
	for _, u := range []string{srv.URL + "/v1", srv.URL + "/v1/"} {
		got, err := New(u, "m").Embed(context.Background(), []string{"a", "b"})
		if err != nil || !reflect.DeepEqual(got, [][]float32{{1, 2}, {3, 4}}) {
			t.Fatalf("%s: got %v, %v", u, got, err)
		}
	}
}

func TestOpenAIErrorAndCountMismatch(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"message":"no such model"}}`))
	}))
	defer bad.Close()
	if _, err := New(bad.URL+"/v1", "m").Embed(context.Background(), []string{"a"}); err == nil {
		t.Error("expected error on 400")
	}
	short := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"index": 0, "embedding": []float32{1}}}})
	}))
	defer short.Close()
	if _, err := New(short.URL+"/v1", "m").Embed(context.Background(), []string{"a", "b"}); err == nil {
		t.Error("expected error on vector count mismatch")
	}
}

func TestOpenAIActualModelAndProbe(t *testing.T) {
	const onnxModel = "Xenova/paraphrase-multilingual-MiniLM-L12-v2"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "model": onnxModel})
		case "/v1/embeddings":
			json.NewEncoder(w).Encode(map[string]any{
				"object": "list",
				"model":  onnxModel,
				"data":   []map[string]any{{"index": 0, "embedding": []float32{0.1, 0.2}}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	// ProbeModel discovers the worker's model via /health.
	c1 := New(srv.URL+"/v1", "")
	if got := c1.ProbeModel(context.Background()); got != onnxModel {
		t.Errorf("ProbeModel = %q, want %q", got, onnxModel)
	}
	if got := c1.ActualModel(); got != onnxModel {
		t.Errorf("ActualModel after probe = %q, want %q", got, onnxModel)
	}

	// Embed also captures the model field from the /v1/embeddings response even
	// if the client sent a fallback model name like bge-m3.
	c2 := New(srv.URL+"/v1", DefaultModel)
	if _, err := c2.Embed(context.Background(), []string{"hello"}); err != nil {
		t.Fatal(err)
	}
	if got := c2.ActualModel(); got != onnxModel {
		t.Errorf("ActualModel after embed = %q, want %q", got, onnxModel)
	}

	// Ollama URLs do not probe /health.
	c3 := New(srv.URL, DefaultModel)
	if got := c3.ProbeModel(context.Background()); got != "" {
		t.Errorf("ProbeModel on ollama URL = %q, want empty", got)
	}
}
