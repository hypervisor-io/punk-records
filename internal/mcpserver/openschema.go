package mcpserver

import (
	"encoding/json"

	"github.com/google/jsonschema-go/jsonschema"
)

// openOutputSchema infers the output schema for T the same way the SDK
// would, then removes every "additionalProperties": false it inferred.
// Clients cache a tool's output schema when they connect and validate
// every result against that cache, so a closed object schema turns any
// field the server adds later into a hard error for every session that
// connected before the upgrade (the seq field on messages broke running
// OpenCode sessions this way). An open schema still documents every
// field; it just tolerates ones the client has not seen yet.
func openOutputSchema[T any]() *jsonschema.Schema {
	s, err := jsonschema.For[T](nil)
	if err != nil {
		panic(err)
	}
	openAdditionalProperties(s)
	return s
}

// openAdditionalProperties clears inferred closed-object markers
// recursively. Only the exact "false" schema the inferrer emits is
// removed; an explicit additionalProperties schema is kept.
func openAdditionalProperties(s *jsonschema.Schema) {
	if s == nil {
		return
	}
	if ap := s.AdditionalProperties; ap != nil {
		// The inferrer's closed marker marshals as the JSON literal false.
		if raw, err := json.Marshal(ap); err == nil && string(raw) == "false" {
			s.AdditionalProperties = nil
		}
	}
	for _, p := range s.Properties {
		openAdditionalProperties(p)
	}
	openAdditionalProperties(s.Items)
	for _, d := range s.Defs {
		openAdditionalProperties(d)
	}
	for _, a := range s.AnyOf {
		openAdditionalProperties(a)
	}
	for _, a := range s.OneOf {
		openAdditionalProperties(a)
	}
	for _, a := range s.AllOf {
		openAdditionalProperties(a)
	}
}
