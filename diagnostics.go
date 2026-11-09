package trest

import (
	"fmt"

	"trest.local/trest/operation"
)

// ConstructionError identifies a route declaration that failed to compile.
// Errors returned by New may contain several of these through errors.Join.
// Unwrap preserves detailed schema/operation diagnostics for errors.As.
type ConstructionError struct {
	Component string
	Method    string
	Path      string
	Name      string
	Source    operation.SourceLocation
	Cause     error
}

func (e *ConstructionError) Error() string {
	label := e.Method + " " + e.Path
	if e.Component != "" {
		label = e.Component + " " + e.Path
	}
	if e.Name != "" {
		label += " (" + e.Name + ")"
	}
	if e.Source.File != "" {
		label += fmt.Sprintf(" at %s:%d", e.Source.File, e.Source.Line)
	}
	return fmt.Sprintf("trest: %s: %v", label, e.Cause)
}

// Unwrap exposes the underlying construction diagnostic.
func (e *ConstructionError) Unwrap() error { return e.Cause }
