// Package openapischema places standalone JSON schemas in an OpenAPI document.
package openapischema

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Place preserves inline schemas without definitions. Schemas with local
// definitions are stored as uniquely named components, and every local ref is
// rebased to its component. Distinct routes cannot accidentally share T0 names.
func Place(raw json.RawMessage, name string, components map[string]any) (any, error) {
	var shape map[string]any
	if err := json.Unmarshal(raw, &shape); err != nil {
		return nil, fmt.Errorf("openapi schema %s: %w", name, err)
	}
	if shape == nil {
		return nil, fmt.Errorf("openapi schema %s: object required", name)
	}
	if _, ok := shape["$defs"]; !ok {
		return raw, nil
	}
	if _, exists := components[name]; exists {
		return nil, fmt.Errorf("openapi: conflicting schema %s", name)
	}
	pointer := func(s string) string { return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1") }
	base := "#/components/schemas/" + pointer(name)
	defs, ok := shape["$defs"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("openapi: invalid definitions in %s", name)
	}
	delete(shape, "$defs")
	var rebase func(any) error
	rebase = func(value any) error {
		switch value := value.(type) {
		case map[string]any:
			if ref, ok := value["$ref"].(string); ok {
				switch {
				case ref == "#":
					value["$ref"] = base
				case strings.HasPrefix(ref, "#/$defs/"):
					path := strings.SplitN(strings.TrimPrefix(ref, "#/$defs/"), "/", 2)
					key := strings.ReplaceAll(strings.ReplaceAll(path[0], "~1", "/"), "~0", "~")
					if _, exists := defs[key]; !exists {
						return fmt.Errorf("openapi: unresolved schema ref %q", ref)
					}
					replacement := "#/components/schemas/" + pointer(name+"_"+key)
					if len(path) > 1 {
						replacement += "/" + path[1]
					}
					value["$ref"] = replacement
				case strings.HasPrefix(ref, "#/"):
					value["$ref"] = base + ref[1:]
				default:
					return fmt.Errorf("openapi: nonlocal schema ref %q", ref)
				}
			}
			for _, child := range value {
				if err := rebase(child); err != nil {
					return err
				}
			}
		case []any:
			for _, child := range value {
				if err := rebase(child); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := rebase(shape); err != nil {
		return nil, err
	}
	for key, definition := range defs {
		component := name + "_" + key
		if _, exists := components[component]; exists {
			return nil, fmt.Errorf("openapi: conflicting schema %s", component)
		}
		if err := rebase(definition); err != nil {
			return nil, err
		}
		components[component] = definition
	}
	components[name] = shape
	return map[string]any{"$ref": base}, nil
}
