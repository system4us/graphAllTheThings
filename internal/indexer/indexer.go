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

// Reindex embeds every graph node (except the source root) into vs using emb
// with the given model. When full is false and the store already holds vectors
// from the same model, unchanged nodes reuse their cached vector.
func Reindex(ctx context.Context, g *graph.Graph, vs store.VectorStore, emb *embed.Client, model string, full bool) (Result, error) {
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
	}

	if len(points) == 0 {
		return Result{}, fmt.Errorf("nothing to index")
	}
	if err := vs.Upsert(ctx, points); err != nil {
		return Result{}, err
	}
	return Result{Total: len(points), Embedded: len(todoIDs), Reused: reused}, nil
}
