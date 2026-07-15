// Package indexer embeds graph nodes into a vector store. It is the shared
// engine behind the CLI's `gatt index` and the MCP server's refresh tool, so
// both index incrementally the same way: nodes whose embedding text is
// unchanged since the last index reuse their cached vector, and only what
// actually moved is re-embedded.
package indexer

import (
	"context"
	"fmt"

	"graphallthethings/internal/embed"
	"graphallthethings/internal/graph"
	"graphallthethings/internal/store"
	"graphallthethings/internal/store/local"
)

// Result reports what an index run did.
type Result struct {
	Total    int // points written to the store (embedded + reused)
	Embedded int // nodes actually sent to the embedder
	Reused   int // nodes served from the incremental cache
}

// ReindexNodes embeds only the given node ids and merges them into the store.
// This is the post-refresh path: a handful of changed nodes in milliseconds,
// instead of walking the whole graph. Ids no longer in the graph are skipped.
func ReindexNodes(ctx context.Context, g *graph.Graph, vs store.VectorStore, emb *embed.Client, model string, ids []string) (int, error) {
	if ls, ok := vs.(*local.Store); ok {
		ls.Model = model
	}
	var liveIDs, texts []string
	for _, id := range ids {
		n := g.Nodes[id]
		if n == nil || n.Type == graph.NodeDatabase || n.Type == graph.NodeAPI {
			continue
		}
		liveIDs = append(liveIDs, id)
		texts = append(texts, g.NodeText(id))
	}
	if len(liveIDs) == 0 {
		return 0, nil
	}
	var points []store.Point
	const batch = 64
	for i := 0; i < len(liveIDs); i += batch {
		end := min(i+batch, len(liveIDs))
		vecs, err := emb.Embed(ctx, texts[i:end])
		if err != nil {
			return 0, err
		}
		for j, v := range vecs {
			n := g.Nodes[liveIDs[i+j]]
			points = append(points, store.Point{
				NodeID: liveIDs[i+j], Type: n.Type, Name: n.Name, Text: texts[i+j], Vector: v,
			})
		}
	}
	if err := vs.Upsert(ctx, points); err != nil {
		return 0, err
	}
	return len(points), nil
}

// Reindex embeds every graph node (except the source root) into vs using emb
// with the given model. When full is false and the store already holds vectors
// from the same model, unchanged nodes reuse their cached vector. progress, if
// non-nil, is called after each embedded batch with the running count of
// embedded nodes and the total to embed (excludes reused/cached nodes) — a
// large graph can take many sequential HTTP round-trips to the embedder, so
// the CLI uses this to print a progress indicator.
func Reindex(ctx context.Context, g *graph.Graph, vs store.VectorStore, emb *embed.Client, model string, full bool, progress func(done, total int)) (Result, error) {
	// Reuse vectors for nodes whose embedding text is unchanged. Only the local
	// store exposes a cache, and only vectors from the same model are
	// comparable. Set the store's model last: reading the cache loads the file
	// and would otherwise overwrite it with the stored value.
	cache := map[string]local.Cached{}
	if ls, ok := vs.(*local.Store); ok {
		if !full && ls.StoredModel() == model {
			cache = ls.Indexed()
		}
		ls.Model = model
	}

	var points []store.Point
	var todoIDs, todoTexts []string
	reused := 0
	for id, n := range g.Nodes {
		if n.Type == graph.NodeDatabase || n.Type == graph.NodeAPI {
			continue
		}
		text := g.NodeText(id)
		if c, ok := cache[id]; ok && c.Hash == local.TextHash(text) {
			points = append(points, store.Point{NodeID: id, Type: n.Type, Name: n.Name, Text: text, Vector: c.Vector})
			reused++
			continue
		}
		todoIDs = append(todoIDs, id)
		todoTexts = append(todoTexts, text)
	}

	const batch = 64
	for i := 0; i < len(todoIDs); i += batch {
		end := min(i+batch, len(todoIDs))
		vecs, err := emb.Embed(ctx, todoTexts[i:end])
		if err != nil {
			return Result{}, err
		}
		for j, v := range vecs {
			n := g.Nodes[todoIDs[i+j]]
			points = append(points, store.Point{
				NodeID: todoIDs[i+j], Type: n.Type, Name: n.Name, Text: todoTexts[i+j], Vector: v,
			})
		}
		if progress != nil {
			progress(end, len(todoIDs))
		}
	}

	if len(points) == 0 {
		return Result{}, fmt.Errorf("nothing to index")
	}
	if err := vs.Upsert(ctx, points); err != nil {
		return Result{}, err
	}
	return Result{Total: len(points), Embedded: len(todoIDs), Reused: reused}, nil
}
