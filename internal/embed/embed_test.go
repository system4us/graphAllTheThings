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
