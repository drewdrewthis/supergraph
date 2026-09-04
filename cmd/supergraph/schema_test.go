package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRunSchema_PrintsFieldsForKnownType(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"__type":{
			"name":"Issue",
			"kind":"OBJECT",
			"description":"An issue.",
			"fields":[
				{"name":"id","type":{"name":null,"kind":"NON_NULL","ofType":{"name":"ID","kind":"SCALAR"}}},
				{"name":"title","type":{"name":"String","kind":"SCALAR","ofType":null}},
				{"name":"labels","type":{"name":null,"kind":"LIST","ofType":{"name":"Label","kind":"OBJECT"}}}
			]
		}}}`))
	}))
	defer srv.Close()

	if err := runSchema(srv.URL, "Issue"); err != nil {
		t.Fatalf("runSchema() error: %v", err)
	}
	vars, _ := gotBody["variables"].(map[string]any)
	if vars["name"] != "Issue" {
		t.Errorf("posted variables = %v, want name=Issue", vars)
	}
}

func TestRunSchema_ErrorsClearlyWhenTypeUnknown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"__type":null}}`))
	}))
	defer srv.Close()

	err := runSchema(srv.URL, "NoSuchType")
	if err == nil {
		t.Fatal("expected error for unknown type")
	}
}

func TestFormatFieldType_RendersNonNullListAndPlain(t *testing.T) {
	cases := []struct {
		name string
		in   introspectFieldType
		want string
	}{
		{"plain", introspectFieldType{Name: "String", Kind: "SCALAR"}, "String"},
		{"non-null", introspectFieldType{Kind: "NON_NULL", OfType: &introspectFieldType0{Name: "ID", Kind: "SCALAR"}}, "ID!"},
		{"list", introspectFieldType{Kind: "LIST", OfType: &introspectFieldType0{Name: "Label", Kind: "OBJECT"}}, "[Label]"},
	}
	for _, c := range cases {
		if got := formatFieldType(c.in); got != c.want {
			t.Errorf("%s: formatFieldType() = %q, want %q", c.name, got, c.want)
		}
	}
}
