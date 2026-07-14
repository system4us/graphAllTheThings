package openapi

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// methodOrder is the fixed set of HTTP methods an OpenAPI path item may carry,
// iterated in a stable order.
var methodOrder = []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "HEAD", "TRACE"}

// spec is the subset of an OpenAPI 3.x / Swagger 2.0 document gatt models.
type spec struct {
	OpenAPI     string               `json:"openapi"`
	Swagger     string               `json:"swagger"` // "2.0" marker
	Info        info                 `json:"info"`
	Paths       map[string]*pathItem `json:"paths"`
	Components  components           `json:"components"`
	Definitions map[string]*schema   `json:"definitions"` // Swagger 2.0 schemas

	// Base URL: 3.x uses servers; 2.0 uses host + basePath + schemes.
	Servers  []server `json:"servers"`
	Host     string   `json:"host"`
	BasePath string   `json:"basePath"`
	Schemes  []string `json:"schemes"`

	// Auth: the global requirement applied to operations that don't override it,
	// and the named scheme catalog (3.x securitySchemes, 2.0 securityDefinitions).
	Security            []secReq              `json:"security"`
	SecurityDefinitions map[string]*secScheme `json:"securityDefinitions"`
}

// secReq is one security requirement object: scheme name -> scopes. A list of
// them means "any of these alternatives satisfies auth".
type secReq map[string][]string

type server struct {
	URL       string               `json:"url"`
	Variables map[string]serverVar `json:"variables"`
}

type serverVar struct {
	Default string `json:"default"`
}

type secScheme struct {
	Type   string `json:"type"`   // apiKey | http | oauth2 | basic (2.0) | openIdConnect
	Name   string `json:"name"`   // apiKey: the header/query parameter name
	In     string `json:"in"`     // apiKey: header | query | cookie
	Scheme string `json:"scheme"` // http: bearer | basic
}

// label renders a compact, curl-oriented description of the scheme, e.g.
// "Bearer", "Basic", "apiKey header X-API-Key", "OAuth2".
func (s *secScheme) label() string {
	switch s.Type {
	case "apiKey":
		return strings.TrimSpace("apiKey " + s.In + " " + s.Name)
	case "http":
		switch s.Scheme {
		case "bearer":
			return "Bearer"
		case "basic":
			return "Basic"
		default:
			return "http " + s.Scheme
		}
	case "basic":
		return "Basic"
	case "oauth2":
		return "OAuth2"
	case "openIdConnect":
		return "OpenIDConnect"
	default:
		return s.Type
	}
}

// schemas returns the component schemas, from either the 3.x location
// (components.schemas) or the 2.0 location (definitions).
func (s *spec) schemas() map[string]*schema {
	if len(s.Components.Schemas) > 0 {
		return s.Components.Schemas
	}
	return s.Definitions
}

// securitySchemes returns the named auth scheme catalog, from either the 3.x
// location (components.securitySchemes) or the 2.0 one (securityDefinitions).
func (s *spec) securitySchemes() map[string]*secScheme {
	if len(s.Components.SecuritySchemes) > 0 {
		return s.Components.SecuritySchemes
	}
	return s.SecurityDefinitions
}

// baseURL resolves the server base URL a curl needs: the first 3.x server (with
// its variable defaults substituted), or the 2.0 scheme://host+basePath.
// Empty when the spec pins no host.
func (s *spec) baseURL() string {
	if len(s.Servers) > 0 {
		url := s.Servers[0].URL
		for name, v := range s.Servers[0].Variables {
			url = strings.ReplaceAll(url, "{"+name+"}", v.Default)
		}
		return url
	}
	if s.Host == "" {
		return ""
	}
	scheme := "http"
	for _, sc := range s.Schemes {
		if sc == "https" {
			scheme = "https"
			break
		}
	}
	return scheme + "://" + s.Host + s.BasePath
}

type info struct {
	Title       string `json:"title"`
	Version     string `json:"version"`
	Description string `json:"description"`
}

type components struct {
	Schemas         map[string]*schema    `json:"schemas"`
	SecuritySchemes map[string]*secScheme `json:"securitySchemes"`
}

type pathItem struct {
	Parameters []param    `json:"parameters"`
	Get        *operation `json:"get"`
	Post       *operation `json:"post"`
	Put        *operation `json:"put"`
	Patch      *operation `json:"patch"`
	Delete     *operation `json:"delete"`
	Options    *operation `json:"options"`
	Head       *operation `json:"head"`
	Trace      *operation `json:"trace"`
}

// op returns the operation for an uppercased HTTP method, or nil.
func (p *pathItem) op(method string) *operation {
	switch method {
	case "GET":
		return p.Get
	case "POST":
		return p.Post
	case "PUT":
		return p.Put
	case "PATCH":
		return p.Patch
	case "DELETE":
		return p.Delete
	case "OPTIONS":
		return p.Options
	case "HEAD":
		return p.Head
	case "TRACE":
		return p.Trace
	}
	return nil
}

type operation struct {
	OperationID string               `json:"operationId"`
	Summary     string               `json:"summary"`
	Description string               `json:"description"`
	Tags        []string             `json:"tags"`
	Parameters  []param              `json:"parameters"`
	RequestBody *requestBody         `json:"requestBody"`
	Responses   map[string]*response `json:"responses"`
	Security    []secReq             `json:"security"` // nil = inherit global; [] = explicitly public
}

// authLabels resolves the auth an operation requires — its own security if set,
// otherwise the global one — into compact per-scheme labels for a curl. Returns
// nil when no auth applies (public), so the caller can tell "public" from
// "unknown scheme".
func (o *operation) authLabels(global []secReq, catalog map[string]*secScheme) []string {
	reqs := o.Security
	if reqs == nil {
		reqs = global
	}
	var labels []string
	seen := map[string]bool{}
	for _, req := range reqs {
		for name := range req {
			if seen[name] {
				continue
			}
			seen[name] = true
			if sc := catalog[name]; sc != nil {
				labels = append(labels, sc.label())
			} else {
				labels = append(labels, name)
			}
		}
	}
	sort.Strings(labels)
	return labels
}

type param struct {
	Name        string  `json:"name"`
	In          string  `json:"in"` // path, query, header, cookie, body (2.0)
	Required    bool    `json:"required"`
	Description string  `json:"description"`
	Schema      *schema `json:"schema"` // 3.x, or the body param's schema in 2.0
	// Swagger 2.0 carries a non-body parameter's type inline on the parameter
	// rather than under a schema object.
	Type   json.RawMessage `json:"type"`
	Format string          `json:"format"`
	Enum   []any           `json:"enum"`
	Items  *schema         `json:"items"`
}

// typeName renders the parameter's compact type, from its schema (3.x) or its
// inline fields (2.0 non-body).
func (p *param) typeName() string {
	if p.Schema != nil {
		return p.Schema.typeName()
	}
	return (&schema{Type: p.Type, Format: p.Format, Items: p.Items}).typeName()
}

type requestBody struct {
	Required bool                  `json:"required"`
	Content  map[string]*mediaType `json:"content"`
}

// requestRef returns the schema an operation accepts as its request body, from
// either the 3.x requestBody or a 2.0 "in: body" parameter, and whether it is
// an array.
func (o *operation) requestRef() (string, bool) {
	if o.RequestBody != nil {
		if n, arr := jsonContentRef(o.RequestBody.Content); n != "" {
			return n, arr
		}
	}
	for _, p := range o.Parameters {
		if p.In == "body" && p.Schema != nil {
			return p.Schema.bodyRef()
		}
	}
	return "", false
}

type response struct {
	Description string                `json:"description"`
	Content     map[string]*mediaType `json:"content"`
	Schema      *schema               `json:"schema"` // Swagger 2.0
}

// bodyRef returns the schema a response produces, from either the 3.x content
// map or the 2.0 direct schema, and whether it is an array.
func (r *response) bodyRef() (string, bool) {
	if r == nil {
		return "", false
	}
	if r.Schema != nil {
		return r.Schema.bodyRef()
	}
	return jsonContentRef(r.Content)
}

type mediaType struct {
	Schema *schema `json:"schema"`
}

// schema is a JSON Schema object as used by OpenAPI. It also appears wrapped in
// allOf/anyOf/oneOf, which FastAPI emits for Optional[...] and nullable refs.
type schema struct {
	Ref         string             `json:"$ref"`
	Type        json.RawMessage    `json:"type"` // string, or []string in OpenAPI 3.1
	Format      string             `json:"format"`
	Enum        []any              `json:"enum"`
	Properties  map[string]*schema `json:"properties"`
	Required    []string           `json:"required"`
	Items       *schema            `json:"items"`
	AllOf       []*schema          `json:"allOf"`
	AnyOf       []*schema          `json:"anyOf"`
	OneOf       []*schema          `json:"oneOf"`
	Description string             `json:"description"`
	Title       string             `json:"title"`
}

// refName extracts "User" from "#/components/schemas/User".
func refName(ref string) string {
	if i := strings.LastIndex(ref, "/"); i >= 0 {
		return ref[i+1:]
	}
	return ref
}

// schemaRef returns the schema name this schema points to via $ref, unwrapping
// the single-ref allOf/anyOf/oneOf forms FastAPI emits for Optional and
// nullable references. Empty when it isn't a reference.
func (s *schema) schemaRef() string {
	if s == nil {
		return ""
	}
	if s.Ref != "" {
		return refName(s.Ref)
	}
	for _, group := range [][]*schema{s.AllOf, s.AnyOf, s.OneOf} {
		for _, sub := range group {
			if r := sub.schemaRef(); r != "" {
				return r
			}
		}
	}
	return ""
}

// bodyRef returns the schema a value refers to and whether it is an array of
// that schema. Used for both properties and request/response bodies.
func (s *schema) bodyRef() (string, bool) {
	if s == nil {
		return "", false
	}
	if r := s.schemaRef(); r != "" {
		return r, false
	}
	if s.typeStr() == "array" && s.Items != nil {
		if r := s.Items.schemaRef(); r != "" {
			return r, true
		}
	}
	return "", false
}

// typeStr resolves the base JSON type, tolerating the 3.1 form where type is a
// list like ["string","null"]; the "null" member is dropped.
func (s *schema) typeStr() string {
	if s == nil || len(s.Type) == 0 {
		return ""
	}
	var one string
	if json.Unmarshal(s.Type, &one) == nil {
		return one
	}
	var many []string
	if json.Unmarshal(s.Type, &many) == nil {
		for _, t := range many {
			if t != "null" {
				return t
			}
		}
	}
	return ""
}

// typeName renders a compact human type: a schema name for refs,
// "array<Elem>" for arrays, "type(format)" for formatted scalars, or a union
// like "string|integer".
func (s *schema) typeName() string {
	if s == nil {
		return ""
	}
	if r := s.schemaRef(); r != "" {
		return r
	}
	t := s.typeStr()
	if t == "array" {
		if s.Items != nil {
			if el := s.Items.typeName(); el != "" {
				return "array<" + el + ">"
			}
		}
		return "array"
	}
	if t == "" {
		var parts []string
		for _, group := range [][]*schema{s.AnyOf, s.OneOf} {
			for _, sub := range group {
				// Drop the "null" member of nullable unions (FastAPI's
				// Optional[...] on 3.1): "string|null" reads as just "string".
				if ts := sub.typeName(); ts != "" && ts != "null" {
					parts = append(parts, ts)
				}
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "|")
		}
	}
	if t != "" && s.Format != "" {
		return t + "(" + s.Format + ")"
	}
	return t
}

// enumValues returns the enum members as a comma-joined string, or empty.
func (s *schema) enumValues() string {
	if s == nil || len(s.Enum) == 0 {
		return ""
	}
	vals := make([]string, 0, len(s.Enum))
	for _, v := range s.Enum {
		vals = append(vals, fmt.Sprint(v))
	}
	return strings.Join(vals, ", ")
}
