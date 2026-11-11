package trest

import (
	"encoding/json"
	"reflect"

	"github.com/mujhtech/trest/internal/schema"
)

// SchemaProvider explicitly describes the wire format of a custom JSON or text
// codec. Implement it on a transport value or local wrapper around a domain type.
// TrestSchema must be pure, deterministic, and valid on a fresh zero value.
// Matching value-encoder and pointer-decoder methods are required. Construction
// checks supported schema keywords; requests and results validate against it.
// Metadata is limited to 64 KiB. Unsupported keywords fail instead of being ignored.
type SchemaProvider interface{ TrestSchema() json.RawMessage }

// Schema returns the same deterministic JSON schema used by routes and clients.
// It invokes no handler and performs no registration or network access.
func Schema[T any]() (json.RawMessage, error) { return schema.Generate(reflect.TypeFor[T]()) }
