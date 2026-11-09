package webinput

import (
	"encoding/json"
	"errors"
	"regexp"
)

var integerText = regexp.MustCompile(`^-?(0|[1-9][0-9]*)$`)
var numberText = regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?$`)

// parameterJSON converts HTTP text into the declared wire representation before
// invoking application codecs. Validation therefore sees the original value,
// including values that a custom decoder might otherwise normalize or discard.
func parameterJSON(values []string, kind, itemKind string) ([]byte, error) {
	scalar := func(text, typ string) (json.RawMessage, error) {
		switch typ {
		case "string":
			return json.Marshal(text)
		case "integer":
			if integerText.MatchString(text) {
				return json.RawMessage(text), nil
			}
		case "number":
			if numberText.MatchString(text) {
				return json.RawMessage(text), nil
			}
		case "boolean":
			if text == "true" || text == "false" {
				return json.RawMessage(text), nil
			}
		}
		return nil, errors.New("invalid scalar parameter")
	}
	if kind == "array" {
		items := make([]json.RawMessage, len(values))
		for i, value := range values {
			item, err := scalar(value, itemKind)
			if err != nil {
				return nil, err
			}
			items[i] = item
		}
		return json.Marshal(items)
	}
	if len(values) != 1 {
		return nil, errors.New("scalar field must appear once")
	}
	return scalar(values[0], kind)
}

func decodeJSON(data []byte, target any) (err error) {
	defer func() {
		if recover() != nil {
			err = errors.New("custom decoder panicked")
		}
	}()
	return json.Unmarshal(data, target)
}

func encodeJSON(value any) (data []byte, err error) {
	defer func() {
		if recover() != nil {
			data = nil
			err = errors.New("custom encoder panicked")
		}
	}()
	return json.Marshal(value)
}
