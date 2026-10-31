package gateway

// Boundary argument validation, mirroring the TS gateway's SDK layer: the
// @modelcontextprotocol/server bundle validates tools/call arguments against
// the served inputSchema with AJV (allErrors, no defaults applied) BEFORE the
// handler runs, and a failure reaches the model as an isError result:
//
//	"Input validation error: Invalid arguments for tool <name>: <ajv errorsText>"
//
// errorsText entries are `data<instancePath> <message>` joined with ", ".
// This implements the draft-07 keyword subset the 20 pinned schemas use:
// type, enum, minLength, minimum, maximum, required, properties, items,
// additionalProperties:false, and local $ref (#/properties/...).

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

// validateBoundary returns AJV errorsText entries; empty means valid.
func validateBoundary(schema json.RawMessage, args map[string]any) []string {
	var root map[string]any
	if err := json.Unmarshal(schema, &root); err != nil {
		return nil // boot-validated; unreachable
	}
	var errs []string
	validateValue(root, root, args, "", &errs)
	return errs
}

func validateValue(root, schema map[string]any, data any, path string, errs *[]string) {
	if ref, ok := schema["$ref"].(string); ok && strings.HasPrefix(ref, "#/properties/") {
		// Local pointer the pinned schemas use (end -> #/properties/start).
		cur := any(root)
		for _, seg := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
			m, ok := cur.(map[string]any)
			if !ok {
				return
			}
			cur = m[seg]
		}
		if target, ok := cur.(map[string]any); ok {
			validateValue(root, target, data, path, errs)
		}
		return
	}

	if typ, ok := schema["type"].(string); ok && !typeMatches(typ, data) {
		*errs = append(*errs, fmt.Sprintf("data%s must be %s", path, typ))
		return // AJV stops at a failed type: deeper keywords do not apply.
	}
	// Type unions (the writeSpreadsheet values cell: ["string","number",
	// "boolean","null"]): AJV's message joins the allowed types with commas.
	if typs, ok := schema["type"].([]any); ok {
		matched := false
		names := make([]string, 0, len(typs))
		for _, t := range typs {
			name, _ := t.(string)
			names = append(names, name)
			if typeMatches(name, data) {
				matched = true
			}
		}
		if !matched {
			*errs = append(*errs, fmt.Sprintf("data%s must be %s", path, strings.Join(names, ",")))
			return
		}
	}
	if enum, ok := schema["enum"].([]any); ok {
		found := false
		for _, e := range enum {
			if jsonEqual(e, data) {
				found = true
				break
			}
		}
		if !found {
			*errs = append(*errs, fmt.Sprintf("data%s must be equal to one of the allowed values", path))
		}
	}
	if s, ok := data.(string); ok {
		if ml, ok := schema["minLength"].(float64); ok && float64(utf8.RuneCountInString(s)) < ml {
			*errs = append(*errs, fmt.Sprintf("data%s must NOT have fewer than %d characters", path, int(ml)))
		}
		if ml, ok := schema["maxLength"].(float64); ok && float64(utf8.RuneCountInString(s)) > ml {
			*errs = append(*errs, fmt.Sprintf("data%s must NOT have more than %d characters", path, int(ml)))
		}
	}
	if n, ok := data.(float64); ok {
		if mn, ok := schema["minimum"].(float64); ok && n < mn {
			*errs = append(*errs, fmt.Sprintf("data%s must be >= %s", path, ajvNumber(mn)))
		}
		if mx, ok := schema["maximum"].(float64); ok && n > mx {
			*errs = append(*errs, fmt.Sprintf("data%s must be <= %s", path, ajvNumber(mx)))
		}
	}
	if obj, ok := data.(map[string]any); ok {
		if req, ok := schema["required"].([]any); ok {
			for _, k := range req {
				key, _ := k.(string)
				if _, present := obj[key]; !present {
					*errs = append(*errs, fmt.Sprintf("data%s must have required property '%s'", path, key))
				}
			}
		}
		props, _ := schema["properties"].(map[string]any)
		// AJV draft-07 vocabulary order: additionalProperties (applicator)
		// runs BEFORE properties (observed: extra-key errors precede property
		// type errors). required (validation vocabulary) precedes both.
		if ap, exists := schema["additionalProperties"]; exists {
			if apFalse, ok := ap.(bool); ok && !apFalse {
				for _, key := range orderedKeys(obj) {
					if _, known := props[key]; !known {
						*errs = append(*errs, fmt.Sprintf("data%s must NOT have additional properties", path))
					}
				}
			}
		}
		if props != nil {
			// AJV emits property checks in the DATA's key order.
			for _, key := range orderedKeys(obj) {
				sub, ok := props[key].(map[string]any)
				if !ok {
					continue
				}
				validateValue(root, sub, obj[key], path+"/"+escapePointer(key), errs)
			}
		}
	}
	if arr, ok := data.([]any); ok {
		if items, ok := schema["items"].(map[string]any); ok {
			for i, el := range arr {
				validateValue(root, items, el, fmt.Sprintf("%s/%d", path, i), errs)
			}
		}
	}
}

func typeMatches(typ string, data any) bool {
	switch typ {
	case "string":
		_, ok := data.(string)
		return ok
	case "integer":
		f, ok := data.(float64)
		return ok && f == float64(int64(f))
	case "number":
		_, ok := data.(float64)
		return ok
	case "boolean":
		_, ok := data.(bool)
		return ok
	case "array":
		_, ok := data.([]any)
		return ok
	case "object":
		_, ok := data.(map[string]any)
		return ok
	case "null":
		return data == nil
	}
	return true
}

func jsonEqual(a, b any) bool {
	aj, _ := json.Marshal(a)
	bj, _ := json.Marshal(b)
	return string(aj) == string(bj)
}

func ajvNumber(f float64) string {
	if f == float64(int64(f)) {
		return fmt.Sprintf("%d", int64(f))
	}
	return fmt.Sprintf("%g", f)
}

// orderedKeys: Go maps randomize iteration; the DATA's key order comes from a
// re-parse, so callers pass data decoded with order preserved. For boundary
// validation the args arrive as map[string]any from encoding/json — order is
// lost, so this falls back to sorted order. AJV error ORDER within a
// multi-error message is the only place this is observable; single-error
// messages (the common case) are exact either way.
func orderedKeys(obj map[string]any) []string {
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func escapePointer(key string) string {
	return strings.ReplaceAll(strings.ReplaceAll(key, "~", "~0"), "/", "~1")
}
