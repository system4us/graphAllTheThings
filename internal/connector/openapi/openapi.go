// Package openapi extracts metadata from an OpenAPI 3.x or Swagger 2.0
// document (such as the spec FastAPI serves at /openapi.json, or the swagger.json
// swaggo generates for Go services) into a semantic graph: component schemas
// and their properties, HTTP endpoints, and the $ref relationships between them
// — the API-side analogue of tables, columns and foreign keys.
//
// The source may be a local file (JSON or YAML) or an http(s):// URL, which is
// fetched with a GET (point it straight at a running FastAPI's
// http://host:port/openapi.json).
package openapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"graphallthethings/internal/graph"
)

type Connector struct {
	Source string // file path or http(s):// URL
}

func New(source string) *Connector { return &Connector{Source: source} }

func (c *Connector) Name() string { return "openapi" }

func (c *Connector) Extract(ctx context.Context) (*graph.Graph, error) {
	data, err := c.read(ctx)
	if err != nil {
		return nil, err
	}
	// YAML is a superset of JSON, so this one path parses both. Decode into a
	// generic tree, then re-encode as JSON so the typed structs' json tags apply
	// uniformly regardless of the input syntax.
	var raw any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse spec: %w", err)
	}
	jb, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("normalize spec: %w", err)
	}
	var sp spec
	if err := json.Unmarshal(jb, &sp); err != nil {
		return nil, fmt.Errorf("decode spec: %w", err)
	}
	if sp.OpenAPI == "" && sp.Swagger == "" {
		return nil, fmt.Errorf("not an OpenAPI/Swagger document (no openapi/swagger version field)")
	}

	title := sp.Info.Title
	if title == "" {
		title = "api"
	}
	g := graph.New("openapi:" + title)
	apiID := "api:" + title
	apiAttrs := map[string]string{}
	if sp.Info.Version != "" {
		apiAttrs["version"] = sp.Info.Version
	}
	if v := sp.OpenAPI; v != "" {
		apiAttrs["openapi"] = v
	} else if sp.Swagger != "" {
		apiAttrs["swagger"] = sp.Swagger
	}
	if d := strings.TrimSpace(sp.Info.Description); d != "" {
		apiAttrs["comment"] = firstLine(d)
	}
	if base := sp.baseURL(); base != "" {
		apiAttrs["base_url"] = base
	}
	g.AddNode(&graph.Node{ID: apiID, Type: graph.NodeAPI, Name: title, Attrs: apiAttrs})

	schemas := sp.schemas()
	b := &builder{
		g:          g,
		apiID:      apiID,
		schemas:    schemas,
		display:    displayNames(schemas),
		globalSec:  sp.Security,
		secSchemes: sp.securitySchemes(),
	}
	// Schemas whose only content is an enum are value domains, not entities: we
	// inline their allowed values onto referencing properties and don't draw a
	// $ref relationship to them (just as a SQL enum column is not a foreign key).
	b.enumSchemas = map[string]string{}
	for key, s := range schemas {
		if vals := s.enumValues(); vals != "" && len(s.Properties) == 0 {
			b.enumSchemas[b.name(key)] = vals
		}
	}

	b.buildSchemas()
	b.buildEndpoints(sp.Paths)
	return g, nil
}

func (c *Connector) read(ctx context.Context) ([]byte, error) {
	if strings.HasPrefix(c.Source, "http://") || strings.HasPrefix(c.Source, "https://") {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Source, nil)
		if err != nil {
			return nil, err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("fetch %s: %w", c.Source, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("fetch %s: HTTP %d", c.Source, resp.StatusCode)
		}
		return io.ReadAll(resp.Body)
	}
	return os.ReadFile(c.Source)
}

// builder holds the state threaded through graph construction: the schema set,
// the mapping from raw schema keys to display names, and the enum value domains.
type builder struct {
	g           *graph.Graph
	apiID       string
	schemas     map[string]*schema
	display     map[string]string // raw key ("pkg.Type") -> display name
	enumSchemas map[string]string // display name -> comma-joined values
	globalSec   []secReq          // global security requirement
	secSchemes  map[string]*secScheme
}

// name maps a raw schema key to its display name, falling back to the key.
func (b *builder) name(key string) string {
	if d, ok := b.display[key]; ok {
		return d
	}
	return key
}

func schemaID(name string) string { return "schema:" + name }

// displayNames shortens verbose schema keys — swaggo emits Go-package-qualified
// names like "backend_internal_api_idp.AgentSyncPayload" — to their final
// dotted segment, but only when that segment is unique across all schemas;
// colliding names keep their full key so ids stay unambiguous.
func displayNames(schemas map[string]*schema) map[string]string {
	shortCount := map[string]int{}
	for key := range schemas {
		shortCount[lastSegment(key)]++
	}
	out := make(map[string]string, len(schemas))
	for key := range schemas {
		if s := lastSegment(key); shortCount[s] == 1 {
			out[key] = s
		} else {
			out[key] = key
		}
	}
	return out
}

func lastSegment(name string) string {
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		return name[i+1:]
	}
	return name
}

func (b *builder) buildSchemas() {
	for _, key := range sortedKeys(b.schemas) {
		s := b.schemas[key]
		name := b.name(key)
		sid := schemaID(name)
		attrs := map[string]string{}
		if d := strings.TrimSpace(s.Description); d != "" {
			attrs["comment"] = firstLine(d)
		}
		if vals, ok := b.enumSchemas[name]; ok {
			attrs["enum_values"] = vals
			if t := s.typeStr(); t != "" {
				attrs["data_type"] = t
			}
		}
		b.g.AddNode(&graph.Node{ID: sid, Type: graph.NodeSchema, Name: name, Attrs: attrs})
		b.g.AddEdge(b.apiID, sid, graph.EdgeHasSchema, nil)

		required := map[string]bool{}
		for _, r := range s.Required {
			required[r] = true
		}
		for _, pname := range sortedKeys(s.Properties) {
			b.buildProperty(name, pname, s.Properties[pname], required[pname])
		}
	}
}

func (b *builder) buildProperty(schemaName, pname string, p *schema, required bool) {
	pid := "property:" + schemaName + "." + pname
	a := map[string]string{"data_type": b.typeLabel(p)}
	if required {
		a["not_null"] = "true"
	}
	if d := strings.TrimSpace(p.Description); d != "" {
		a["comment"] = firstLine(d)
	}

	// Referenced schema, either directly ($ref / Optional wrapper) or as the
	// element type of an array. Enum schemas are inlined as allowed values
	// rather than modeled as a relationship.
	target, viaArray := p.bodyRef()
	targetName := b.name(target)
	if vals := p.enumValues(); vals != "" {
		a["enum_values"] = vals
	} else if evals, ok := b.enumSchemas[targetName]; ok {
		a["enum_values"] = evals
		targetName = "" // value domain, not a relationship
	}

	b.g.AddNode(&graph.Node{ID: pid, Type: graph.NodeProperty, Name: schemaName + "." + pname, Attrs: a})
	b.g.AddEdge(schemaID(schemaName), pid, graph.EdgeHasProperty, nil)

	if targetName != "" && b.schemas[target] != nil {
		b.g.AddEdge(pid, schemaID(targetName), graph.EdgeRefersTo, nil)
		refAttrs := map[string]string{"from_property": pname}
		if viaArray {
			refAttrs["cardinality"] = "array"
		}
		b.g.AddEdge(schemaID(schemaName), schemaID(targetName), graph.EdgeReferences, refAttrs)
	}
}

// typeLabel renders a property's compact type, mapping a $ref to its display
// name so verbose package-qualified keys don't leak into the type column.
func (b *builder) typeLabel(p *schema) string {
	if ref := p.schemaRef(); ref != "" {
		return b.name(ref)
	}
	if p.typeStr() == "array" && p.Items != nil {
		if ref := p.Items.schemaRef(); ref != "" {
			return "array<" + b.name(ref) + ">"
		}
	}
	return p.typeName()
}

func (b *builder) buildEndpoints(paths map[string]*pathItem) {
	for _, path := range sortedKeys(paths) {
		item := paths[path]
		for _, m := range methodOrder {
			op := item.op(m)
			if op == nil {
				continue
			}
			eid := "endpoint:" + m + " " + path
			attrs := map[string]string{"method": m, "path": path}
			if op.OperationID != "" {
				attrs["operation_id"] = op.OperationID
			}
			if s := strings.TrimSpace(op.Summary); s != "" {
				attrs["comment"] = firstLine(s)
			} else if d := strings.TrimSpace(op.Description); d != "" {
				attrs["comment"] = firstLine(d)
			}
			if len(op.Tags) > 0 {
				attrs["tags"] = strings.Join(op.Tags, ", ")
			}
			if auth := op.authLabels(b.globalSec, b.secSchemes); len(auth) > 0 {
				attrs["auth"] = strings.Join(auth, ", ")
			}
			// Path-level parameters apply to every operation; operation-level ones
			// add to them. Body parameters (2.0) are modeled as ACCEPTS below, not
			// listed here.
			byIn := map[string][]string{}
			for _, pr := range append(append([]param{}, item.Parameters...), op.Parameters...) {
				if pr.In == "body" {
					continue
				}
				desc := pr.Name
				if t := pr.typeName(); t != "" {
					desc += " " + t
				}
				if pr.Required {
					desc += "!"
				}
				byIn[pr.In] = append(byIn[pr.In], desc)
			}
			for in, key := range map[string]string{"path": "path_params", "query": "query_params", "header": "header_params"} {
				if len(byIn[in]) > 0 {
					attrs[key] = strings.Join(byIn[in], ", ")
				}
			}

			b.g.AddNode(&graph.Node{ID: eid, Type: graph.NodeEndpoint, Name: m + " " + path, Attrs: attrs})
			b.g.AddEdge(b.apiID, eid, graph.EdgeHasEndpoint, nil)

			// Request body → ACCEPTS.
			if key, arr := op.requestRef(); key != "" {
				if name := b.name(key); b.enumSchemas[name] == "" && b.schemas[key] != nil {
					b.g.AddEdge(eid, schemaID(name), graph.EdgeAccepts, arrAttr(arr))
				}
			}
			// Responses → RESPONDS_WITH (success statuses only; error responses
			// reuse a shared error model that would just add noise).
			for _, status := range sortedKeys(op.Responses) {
				if !isSuccess(status) {
					continue
				}
				key, arr := op.Responses[status].bodyRef()
				name := b.name(key)
				if key == "" || b.enumSchemas[name] != "" || b.schemas[key] == nil {
					continue
				}
				a := arrAttr(arr)
				if a == nil {
					a = map[string]string{}
				}
				a["status"] = status
				b.g.AddEdge(eid, schemaID(name), graph.EdgeRespondsWith, a)
			}
		}
	}
}

func arrAttr(arr bool) map[string]string {
	if arr {
		return map[string]string{"cardinality": "array"}
	}
	return nil
}

// jsonContentRef returns the schema referenced by a request/response content
// map, preferring application/json, and whether it is wrapped in an array.
func jsonContentRef(content map[string]*mediaType) (string, bool) {
	if content == nil {
		return "", false
	}
	mt := content["application/json"]
	if mt == nil {
		for _, k := range sortedKeys(content) {
			mt = content[k]
			break
		}
	}
	if mt == nil {
		return "", false
	}
	return mt.Schema.bodyRef()
}

func isSuccess(status string) bool {
	return status == "default" || strings.HasPrefix(status, "2")
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
