// Package inspect exports the actual frozen application model without executing
// handlers or reading application credentials. Metadata itself may be sensitive;
// applications decide who can access an exported model.
package inspect

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"

	"trest.local/trest/contract"
	"trest.local/trest/httpadapter"
	"trest.local/trest/operation"
)

const SchemaVersion = "trest.inspect.v1"

// Model is a versioned, deterministic snapshot. It excludes executable bindings,
// credentials, request bodies, and connection configuration by construction.
type Model struct {
	SchemaVersion string                `json:"schema_version"`
	Operations    []operation.Metadata  `json:"operations"`
	HTTP          []httpadapter.Binding `json:"http"`
}

// Build reads the frozen registry and validates its explicit HTTP bindings. A
// source location identifies registration code, not inferred dependencies.
func Build(registry *operation.Registry, bindings []httpadapter.Binding) (Model, error) {
	if registry == nil || !registry.IsFrozen() {
		return Model{}, &operation.Diagnostic{Code: "registry_not_frozen", Path: "registry", Message: "freeze the registry before inspection"}
	}
	if _, err := contract.Export(registry, bindings, contract.Info{Title: "Inspection validation", Version: "1"}); err != nil {
		return Model{}, fmt.Errorf("inspect: invalid bindings: %w", err)
	}
	ops := registry.Operations()
	for i := range ops {
		// Even explicitly configured source paths must not expose a developer's
		// home directory. Source function names remain useful within Go modules.
		ops[i].Source.File = filepath.Base(ops[i].Source.File)
		if ops[i].Source.File == "." {
			ops[i].Source.File = ""
		}
	}
	copied := append([]httpadapter.Binding{}, bindings...)
	sort.Slice(copied, func(i, j int) bool {
		if copied[i].Path != copied[j].Path {
			return copied[i].Path < copied[j].Path
		}
		return copied[i].Method < copied[j].Method
	})
	return Model{SchemaVersion: SchemaVersion, Operations: ops, HTTP: copied}, nil
}

// JSON encodes a versioned model without timestamps or process-specific values.
func JSON(model Model) ([]byte, error) {
	if model.SchemaVersion != SchemaVersion {
		return nil, errors.New("inspect: unsupported schema version")
	}
	return json.MarshalIndent(model, "", "  ")
}
