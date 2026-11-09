// Package docsui supplies a self-contained, same-origin API explorer.
package docsui

import (
	_ "embed"
	"encoding/json"
	"html"
	"strings"
)

//go:embed page.html
var page string

// Page returns an offline HTML explorer. Only a user-triggered Send action calls
// an endpoint. Credentials stay in the page's memory and are never persisted.
// specPath is checked for same-origin access again by the browser.
func Page(title, specPath string) []byte {
	encoded, _ := json.Marshal(specPath)
	out := strings.ReplaceAll(page, "__TREST_TITLE__", html.EscapeString(title))
	out = strings.ReplaceAll(out, "__TREST_SPEC__", string(encoded))
	return []byte(out)
}
