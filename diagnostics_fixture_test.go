package trest_test

import "trest.local/trest"

// Keep this declaration in another file to verify locations survive aggregation.
func invalidDeclaredElsewhere() trest.Component {
	return trest.Post("/other", trest.Const("hello")).Status(99)
}
