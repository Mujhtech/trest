package schema

import (
	"reflect"
	"sort"
	"strconv"
	"strings"
)

type structField struct {
	field    reflect.StructField
	name     string
	depth    int
	tagged   bool
	optional bool
}

// structFields follows encoding/json's promotion rules. Fields at the shallowest
// depth win, then explicitly named fields win; remaining ambiguity is ignored.
// A nil embedded pointer omits all its promoted fields, making them optional.
func structFields(t reflect.Type, path string) ([]structField, error) {
	var candidates []structField
	var collect func(reflect.Type, int, bool, map[reflect.Type]bool) error
	collect = func(current reflect.Type, depth int, optionalParent bool, active map[reflect.Type]bool) error {
		if active[current] {
			return nil
		}
		if depth > 128 {
			return unsupported(path, "maximum embedded type nesting exceeded")
		}
		active[current] = true
		defer delete(active, current)
		for i := 0; i < current.NumField(); i++ {
			f := current.Field(i)
			base := f.Type
			if base.Kind() == reflect.Pointer {
				base = base.Elem()
			}
			if f.PkgPath != "" && (!f.Anonymous || base.Kind() != reflect.Struct) {
				continue
			}
			tag := f.Tag.Get("json")
			if tag == "-" {
				continue
			}
			if f.Anonymous && f.PkgPath != "" && f.Type.Kind() == reflect.Pointer {
				return unsupported(joinPath(path, f.Name), "unexported embedded pointers cannot be initialized by encoding/json")
			}
			parts := strings.Split(tag, ",")
			name := parts[0]
			tagged := name != ""
			if name == "" {
				name = f.Name
			}
			if !validName(name) {
				return unsupported(joinPath(path, f.Name), "valid json field name required")
			}
			optional := optionalParent || f.Type.Kind() == reflect.Pointer
			for _, option := range parts[1:] {
				if option != "omitempty" {
					return unsupported(joinPath(path, name), "unsupported json tag option "+strconv.Quote(option))
				}
				optional = true
			}
			if f.Anonymous && base.Kind() == reflect.Struct && !tagged {
				for _, constraint := range []string{"required", "min", "max", "minLength", "maxLength", "minItems", "maxItems", "pattern", "enum", "default", "description"} {
					if _, exists := f.Tag.Lookup(constraint); exists {
						return unsupported(joinPath(path, name), "constraints on flattened embedded fields must be declared on their promoted fields")
					}
				}
				if err := collect(base, depth+1, optionalParent || f.Type.Kind() == reflect.Pointer, active); err != nil {
					return err
				}
				continue
			}
			if required, ok := f.Tag.Lookup("required"); ok {
				if required != "true" && required != "false" {
					return unsupported(joinPath(path, name), "required must be true or false")
				}
				optional = required == "false"
			}
			candidates = append(candidates, structField{field: f, name: name, depth: depth, tagged: tagged, optional: optional})
		}
		return nil
	}
	if err := collect(t, 0, false, make(map[reflect.Type]bool)); err != nil {
		return nil, err
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if a.name != b.name {
			return a.name < b.name
		}
		if a.depth != b.depth {
			return a.depth < b.depth
		}
		return a.tagged && !b.tagged
	})
	fields := make([]structField, 0, len(candidates))
	for start := 0; start < len(candidates); {
		end := start + 1
		for end < len(candidates) && candidates[end].name == candidates[start].name {
			end++
		}
		first := candidates[start]
		ambiguous := start+1 < end && first.depth == candidates[start+1].depth && first.tagged == candidates[start+1].tagged
		if ambiguous && first.depth == 0 {
			return nil, unsupported(joinPath(path, first.name), "duplicate json field name")
		}
		if !ambiguous {
			fields = append(fields, first)
		}
		start = end
	}
	return fields, nil
}
