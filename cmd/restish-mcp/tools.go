package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/pb33f/libopenapi"
	v3high "github.com/pb33f/libopenapi/datamodel/high/v3"

	"github.com/rest-sh/restish/v2/internal/spec"
	"github.com/rest-sh/restish/v2/plugin"
)

type APISpec struct {
	Name        string
	ContentType string
	Raw         []byte
	Document    libopenapi.Document
	Operations  []plugin.APIOperation
}

type HTTPExecutor func(*HTTPRequest) (*HTTPResponse, error)
type SpecFetcher func(name string) (*APISpec, error)

type Tool struct {
	APIName           string
	Name              string
	Description       string
	Method            string
	Path              string
	InputSchema       map[string]any
	Params            []Param
	BodyContentType   string
	BodySchemaDialect string
	BodyRequired      bool
}

type Param struct {
	Name             string
	In               string
	Required         bool
	Description      string
	Type             string
	ItemType         string
	Style            string
	Explode          *bool
	AllowReserved    bool
	ContentMediaType string
	Schema           map[string]any
	SchemaDialect    string
}

type Options struct {
	ReadOnly        bool
	AllowWriteTools bool
	MaxResultBytes  int
	RequestTimeout  int
}

func LoadTools(fetchSpec SpecFetcher, apiNames []string, opts Options) ([]*Tool, error) {
	multiAPI := len(apiNames) > 1
	var tools []*Tool
	for _, apiName := range apiNames {
		s, err := fetchSpec(apiName)
		if err != nil {
			return nil, err
		}
		apiTools, err := toolsFromSpec(apiName, multiAPI, s, opts)
		if err != nil {
			return nil, err
		}
		tools = append(tools, apiTools...)
	}
	disambiguateToolNames(tools)
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
	return tools, nil
}

func toolsFromSpec(apiName string, multiAPI bool, s *APISpec, _ Options) ([]*Tool, error) {
	if len(s.Operations) > 0 {
		return toolsFromOperations(apiName, multiAPI, s.Operations), nil
	}
	model, err := s.Document.BuildV3Model()
	if err != nil || model == nil || model.Model.Paths == nil {
		return nil, fmt.Errorf("building OpenAPI model for %q: %w", apiName, err)
	}

	var tools []*Tool
	for path, pathItem := range model.Model.Paths.PathItems.FromOldest() {
		for _, item := range spec.PathItemMethods(pathItem) {
			if item.Op == nil {
				continue
			}
			tool, err := buildTool(apiName, multiAPI, path, item.Method, pathItem.Parameters, item.Op)
			if err != nil {
				return nil, err
			}
			tools = append(tools, tool)
		}
	}
	disambiguateToolNames(tools)
	return tools, nil
}

func toolsFromOperations(apiName string, multiAPI bool, ops []plugin.APIOperation) []*Tool {
	var tools []*Tool
	for _, op := range ops {
		tools = append(tools, buildToolFromOperation(apiName, multiAPI, op))
	}
	disambiguateToolNames(tools)
	return tools
}

func mcpWriteMethod(method string) bool {
	switch strings.ToUpper(method) {
	case "POST", "PUT", "PATCH", "DELETE":
		return true
	default:
		return false
	}
}

func buildToolFromOperation(apiName string, multiAPI bool, op plugin.APIOperation) *Tool {
	name := operationToolName(op.ID, op.Method, op.Path)
	if multiAPI {
		name = apiName + "__" + name
	}
	description := strings.TrimSpace(op.Summary)
	if description == "" {
		description = strings.TrimSpace(op.Description)
	}
	if description == "" {
		description = op.Method + " " + op.Path
	}

	properties := map[string]any{}
	var required []string
	params := make([]Param, 0, len(op.Parameters))
	for _, p := range op.Parameters {
		var paramSchema map[string]any
		if len(p.Schema) > 0 {
			paramSchema = schemaMap(p.Schema)
		}
		params = append(params, Param{
			Name:             p.Name,
			In:               p.In,
			Required:         p.Required,
			Description:      p.Description,
			Type:             p.Type,
			ItemType:         p.ItemType,
			Style:            p.Style,
			Explode:          p.Explode,
			AllowReserved:    p.AllowReserved,
			ContentMediaType: p.ContentMediaType,
			Schema:           paramSchema,
			SchemaDialect:    p.SchemaDialect,
		})
		prop := schemaFromOperationParam(p)
		properties[p.Name] = prop
		if p.Required {
			required = append(required, p.Name)
		}
	}

	if op.HasBody {
		properties["body"] = operationBodySchema(op.RequestSchema)
		if op.BodyRequired {
			required = append(required, "body")
		}
	}

	inputSchema := map[string]any{
		"type":       "object",
		"properties": properties,
	}
	if len(required) > 0 {
		inputSchema["required"] = required
	}
	return &Tool{
		APIName:           apiName,
		Name:              name,
		Description:       description,
		Method:            op.Method,
		Path:              op.Path,
		InputSchema:       inputSchema,
		Params:            params,
		BodyContentType:   op.RequestMediaType,
		BodySchemaDialect: op.RequestSchemaDialect,
		BodyRequired:      op.BodyRequired,
	}
}

func operationBodySchema(schema map[string]any) map[string]any {
	if len(schema) == 0 {
		return map[string]any{"type": "object"}
	}
	return schemaMap(schema)
}

func schemaFromOperationParam(p plugin.APIParam) map[string]any {
	if len(p.Schema) > 0 {
		prop := schemaMap(p.Schema)
		if p.Description != "" {
			prop["description"] = p.Description
		}
		if len(p.Enum) > 0 {
			enum := make([]any, len(p.Enum))
			for i, value := range p.Enum {
				enum[i] = value
			}
			prop["enum"] = enum
		}
		return prop
	}
	typ := p.Type
	if typ == "" {
		typ = "string"
	}
	prop := map[string]any{"type": typ}
	if typ == "array" && p.ItemType != "" {
		prop["items"] = map[string]any{"type": p.ItemType}
	}
	if p.Description != "" {
		prop["description"] = p.Description
	}
	if len(p.Enum) > 0 {
		enum := make([]any, len(p.Enum))
		for i, value := range p.Enum {
			enum[i] = value
		}
		prop["enum"] = enum
	}
	return prop
}

func buildTool(apiName string, multiAPI bool, path, method string, pathParams []*v3high.Parameter, op *v3high.Operation) (*Tool, error) {
	name := operationToolName(op.OperationId, method, path)
	if multiAPI {
		name = apiName + "__" + name
	}
	description := strings.TrimSpace(op.Summary)
	if description == "" {
		description = strings.TrimSpace(op.Description)
	}
	if description == "" {
		description = method + " " + path
	}

	properties := map[string]any{}
	var required []string
	var params []Param
	for _, p := range spec.MergeParameters(pathParams, op.Parameters) {
		prop := schemaMap(nil)
		contentMediaType := parameterContentMediaType(p)
		if contentMediaType != "" {
			prop = parameterContentSchemaMap(p, contentMediaType)
		} else if p.Schema != nil && p.Schema.Schema() != nil {
			prop = schemaMap(p.Schema.Schema())
		}
		typ, itemType := schemaTypeInfo(prop)
		params = append(params, Param{
			Name:             p.Name,
			In:               p.In,
			Required:         p.Required != nil && *p.Required,
			Description:      p.Description,
			Type:             typ,
			ItemType:         itemType,
			Style:            p.Style,
			Explode:          p.Explode,
			AllowReserved:    p.AllowReserved,
			ContentMediaType: contentMediaType,
			Schema:           schemaMap(prop),
		})
		if p.Description != "" {
			prop["description"] = p.Description
		}
		properties[p.Name] = prop
		if p.Required != nil && *p.Required {
			required = append(required, p.Name)
		}
	}

	bodyContentType, bodyRequired := "", false
	if op.RequestBody != nil {
		bodyContentType, properties["body"] = requestBodyProperty(op.RequestBody)
		bodyRequired = requestBodyRequired(op.RequestBody)
		if bodyRequired {
			required = append(required, "body")
		}
	}

	inputSchema := map[string]any{
		"type":       "object",
		"properties": properties,
	}
	if len(required) > 0 {
		inputSchema["required"] = required
	}
	return &Tool{
		APIName:         apiName,
		Name:            name,
		Description:     description,
		Method:          method,
		Path:            path,
		InputSchema:     inputSchema,
		Params:          params,
		BodyContentType: bodyContentType,
		BodyRequired:    bodyRequired,
	}, nil
}

func operationToolName(operationID, method, path string) string {
	if name := strings.TrimSpace(operationID); name != "" {
		return name
	}
	var slug strings.Builder
	lastUnderscore := false
	for _, r := range strings.Trim(path, "/") {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			slug.WriteRune(unicode.ToLower(r))
			lastUnderscore = false
			continue
		}
		if !lastUnderscore {
			slug.WriteByte('_')
			lastUnderscore = true
		}
	}
	base := strings.Trim(slug.String(), "_")
	if base == "" {
		base = "root"
	}
	if len(base) > 40 {
		base = strings.TrimRight(base[:40], "_")
	}
	digest := sha256.Sum256([]byte(strings.ToUpper(method) + "\n" + path))
	return strings.ToLower(method) + "_" + base + "_" + fmt.Sprintf("%x", digest[:4])
}

func disambiguateToolNames(tools []*Tool) {
	counts := make(map[string]int, len(tools))
	for _, tool := range tools {
		counts[tool.Name]++
	}
	used := make(map[string]struct{}, len(tools))
	for _, tool := range tools {
		if counts[tool.Name] == 1 {
			used[tool.Name] = struct{}{}
		}
	}
	for _, tool := range tools {
		if counts[tool.Name] < 2 {
			continue
		}
		base := tool.Name
		digest := sha256.Sum256([]byte(tool.APIName + "\n" + strings.ToUpper(tool.Method) + "\n" + tool.Path))
		suffix := fmt.Sprintf("%x", digest[:4])
		candidate := base + "__" + suffix
		for counter := 2; ; counter++ {
			if _, exists := used[candidate]; !exists {
				break
			}
			candidate = base + "__" + suffix + fmt.Sprintf("_%d", counter)
		}
		tool.Name = candidate
		used[candidate] = struct{}{}
	}
}

func requestBodyProperty(body *v3high.RequestBody) (string, map[string]any) {
	if body == nil || body.Content == nil {
		return "", map[string]any{"type": "object"}
	}
	for _, contentType := range []string{"application/json", "application/merge-patch+json"} {
		if media := body.Content.GetOrZero(contentType); media != nil {
			return contentType, bodySchema(media, body.Description)
		}
	}
	for contentType, media := range body.Content.FromOldest() {
		if media != nil {
			return contentType, bodySchema(media, body.Description)
		}
	}
	return "", map[string]any{"type": "object"}
}

func requestBodyRequired(body *v3high.RequestBody) bool {
	return body != nil && body.Required != nil && *body.Required
}

func bodySchema(media *v3high.MediaType, description string) map[string]any {
	prop := map[string]any{"type": "object"}
	if media != nil && media.Schema != nil && media.Schema.Schema() != nil {
		prop = schemaMap(media.Schema.Schema())
	}
	if description != "" {
		prop["description"] = description
	}
	return prop
}

func parameterContentMediaType(p *v3high.Parameter) string {
	if p == nil || p.Content == nil {
		return ""
	}
	var names []string
	for name := range p.Content.FromOldest() {
		names = append(names, name)
	}
	if len(names) == 0 {
		return ""
	}
	for _, name := range names {
		mt := strings.ToLower(strings.TrimSpace(strings.Split(name, ";")[0]))
		if mt == "application/json" || strings.HasSuffix(mt, "+json") {
			return name
		}
	}
	sort.Strings(names)
	return names[0]
}

func parameterContentSchemaMap(p *v3high.Parameter, contentType string) map[string]any {
	if p == nil || p.Content == nil || contentType == "" {
		return schemaMap(nil)
	}
	media := p.Content.GetOrZero(contentType)
	if media == nil || media.Schema == nil || media.Schema.Schema() == nil {
		return schemaMap(nil)
	}
	return schemaMap(media.Schema.Schema())
}

func schemaMap(v any) map[string]any {
	if v == nil {
		return map[string]any{"type": "string"}
	}
	data, err := json.Marshal(v)
	if err != nil || len(data) == 0 {
		return map[string]any{"type": "string"}
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil || len(out) == 0 {
		return map[string]any{"type": "string"}
	}
	return out
}

func schemaTypeInfo(prop map[string]any) (string, string) {
	typ, _ := prop["type"].(string)
	itemType := ""
	if items, ok := prop["items"].(map[string]any); ok {
		itemType, _ = items["type"].(string)
	}
	return typ, itemType
}

func indexTools(tools []*Tool) map[string]*Tool {
	out := make(map[string]*Tool, len(tools))
	for _, tool := range tools {
		out[tool.Name] = tool
	}
	return out
}
