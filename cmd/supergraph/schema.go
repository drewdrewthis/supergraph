package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// introspectTypeQuery is the fixed introspection query `schema <Type>` sends;
// it stays shallow (one level of ofType) because that is what a wrapped
// scalar/list/non-null field needs to render (LIST/NON_NULL of a NAMED type).
const introspectTypeQuery = `query($name: String!) {
  __type(name: $name) {
    name
    kind
    description
    fields {
      name
      type { name kind ofType { name kind } }
    }
  }
}`

type introspectFieldType struct {
	Name   string                `json:"name"`
	Kind   string                `json:"kind"`
	OfType *introspectFieldType0 `json:"ofType"`
}

// introspectFieldType0 is the one-level-deeper shape the query above allows;
// it deliberately does not recurse further than the fixed query fetches.
type introspectFieldType0 struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

type introspectField struct {
	Name string              `json:"name"`
	Type introspectFieldType `json:"type"`
}

type introspectType struct {
	Name        string            `json:"name"`
	Kind        string            `json:"kind"`
	Description string            `json:"description"`
	Fields      []introspectField `json:"fields"`
}

// runSchema posts the fixed introspection query for typeName to endpoint and
// prints a readable SDL-ish listing, erroring clearly when the type is unknown.
func runSchema(endpoint, typeName string) error {
	body, err := json.Marshal(map[string]any{
		"query":     introspectTypeQuery,
		"variables": map[string]any{"name": typeName},
	})
	if err != nil {
		return fmt.Errorf("encode request: %w", err)
	}
	raw, err := postGraphQL(endpoint, body)
	if err != nil {
		return err
	}

	var envelope struct {
		Data struct {
			Type *introspectType `json:"__type"`
		} `json:"data"`
		Errors []json.RawMessage `json:"errors"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("decode response %q: %w", string(raw), err)
	}
	if len(envelope.Errors) > 0 {
		var parts []string
		for _, e := range envelope.Errors {
			parts = append(parts, string(e))
		}
		return fmt.Errorf("graphql errors: %s", strings.Join(parts, ", "))
	}
	if envelope.Data.Type == nil {
		return fmt.Errorf("unknown type: %s", typeName)
	}

	fmt.Println(formatSchemaType(*envelope.Data.Type))
	return nil
}

// formatSchemaType renders an introspected type as an SDL-ish listing.
func formatSchemaType(t introspectType) string {
	var b strings.Builder
	if t.Description != "" {
		fmt.Fprintf(&b, "\"\"\"%s\"\"\"\n", t.Description)
	}
	fmt.Fprintf(&b, "type %s {\n", t.Name)
	for _, f := range t.Fields {
		fmt.Fprintf(&b, "  %s: %s\n", f.Name, formatFieldType(f.Type))
	}
	b.WriteString("}")
	return b.String()
}

// formatFieldType renders a field's type reference, e.g. "String", "[Issue]",
// "Int!" from the (kind, name, ofType) triple the introspection query returns.
func formatFieldType(t introspectFieldType) string {
	switch t.Kind {
	case "NON_NULL":
		return formatOfType(t.OfType) + "!"
	case "LIST":
		return "[" + formatOfType(t.OfType) + "]"
	default:
		return t.Name
	}
}

func formatOfType(t *introspectFieldType0) string {
	if t == nil {
		return "Unknown"
	}
	return t.Name
}
