package contract

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func body(schema string) []byte {
	return []byte(`{"deadline":"2030-01-01T00:00:00Z","input_refs":[{"version":1,"digest":"sha256:` + strings.Repeat("a", 64) + `"}],"output_schema":` + schema + `,"acceptance_policy":{"verifier":"report/1","checks":["schema"],"on_fail":"report"}}`)
}

func TestContractRejectsInvalidAdmission(t *testing.T) {
	for _, schema := range []string{`null`, `42`, `[]`, `"object"`, `{"type":"bogus"}`, `{"required":"x"}`, `{"$ref":"#/$defs/missing"}`, `{"$schema":"http://json-schema.org/draft-07/schema#"}`, `{"$defs":{"x":{"$schema":"http://json-schema.org/draft-07/schema#"}},"$ref":"#/$defs/x"}`} {
		t.Run(schema, func(t *testing.T) {
			if _, err := Parse(body(schema)); err == nil {
				t.Fatal("invalid schema admitted")
			}
		})
	}
	for _, policy := range []string{`null`, `{}`, `{"verifier":" ","checks":["schema"],"on_fail":"report"}`, `{"verifier":"v","checks":["anything"],"on_fail":"report"}`, `{"verifier":"v","checks":["schema","schema"],"on_fail":"report"}`, `{"verifier":"v","checks":["schema"],"on_fail":"ignore"}`} {
		var b map[string]json.RawMessage
		json.Unmarshal(body(`{}`), &b)
		b["acceptance_policy"] = json.RawMessage(policy)
		raw, _ := json.Marshal(b)
		if _, err := Parse(raw); err == nil {
			t.Fatalf("invalid policy admitted: %s", policy)
		}
	}
}

func TestContractCompiledSchema(t *testing.T) {
	b, err := Parse(body(`{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","$defs":{"item":{"type":"integer","minimum":1}},"properties":{"n":{"$ref":"#/$defs/item"},"at":{"type":"string","format":"date-time"}},"required":["n","at"],"unevaluatedProperties":false}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		raw   string
		valid bool
	}{
		{`{"n":1,"at":"2026-10-27T00:00:00Z"}`, true},
		{`{"n":0,"at":"2026-10-27T00:00:00Z"}`, false},
		{`{"n":1,"at":"tomorrow"}`, false},
		{`{"n":1,"at":"2026-10-27T00:00:00Z","extra":true}`, false},
	} {
		var value any
		json.Unmarshal([]byte(test.raw), &value)
		if err := b.Schema.Validate(value); (err == nil) != test.valid {
			t.Fatalf("%s: %v", test.raw, err)
		}
	}
	b, err = Parse(body(`false`))
	if err != nil || b.Schema.Validate(nil) == nil {
		t.Fatal("false schema semantics", err)
	}
	b, err = Parse(body(`true`))
	if err != nil || b.Schema.Validate(nil) != nil {
		t.Fatal("true schema semantics", err)
	}
	if _, err = Parse(body(`{"const":{"$schema":"data, not a dialect"}}`)); err != nil {
		t.Fatal(err)
	}
}

func TestContractNeverLoadsExternalResources(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.Write([]byte(`{"type":"object"}`)) }))
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "schema.json")
	if err := os.WriteFile(path, []byte(`{"type":"object"}`), 0600); err != nil {
		t.Fatal(err)
	}
	fileURL := "file:///" + strings.TrimPrefix(filepath.ToSlash(path), "/")
	for _, ref := range []string{srv.URL + "/schema", fileURL, "relative-schema.json"} {
		schema, _ := json.Marshal(map[string]string{"$ref": ref})
		if _, err := Parse(body(string(schema))); err == nil {
			t.Fatal("external resource loaded", ref)
		}
	}
	if requests.Load() != 0 {
		t.Fatal("contract triggered network request")
	}
}

func TestContractCanonicalAndComplexityBounds(t *testing.T) {
	for _, raw := range [][]byte{
		[]byte(`{"deadline":1,"deadline":2}`),
		body(`{"const":9007199254740992}`),
		body(`{"const":` + strings.Repeat("[", 65) + `0` + strings.Repeat("]", 65) + `}`),
		body(`{"const":[` + strings.Repeat(`0,`, 32768) + `0]}`),
		body(`{"description":"` + strings.Repeat("x", MaxBytes) + `"}`),
	} {
		if _, err := Parse(raw); err == nil {
			t.Fatal("invalid or excessive JSON admitted")
		}
	}
}

func TestContractReferencedDialectAndRecursion(t *testing.T) {
	// definitions is not a schema-position keyword in 2020-12. A referenced
	// resource there still must not switch to a dialect with different semantics.
	for _, ref := range []string{`"$ref":"#/definitions/legacy"`, `"properties":{"x":{"$ref":"#/definitions/legacy"}}`, `"items":{"$ref":"#/definitions/legacy"}`, `"additionalProperties":{"$ref":"#/definitions/legacy"}`} {
		schema := `{"definitions":{"legacy":{"$id":"https://anet.invalid/legacy","$schema":"http://json-schema.org/draft-07/schema#","type":"integer"}},` + ref + `}`
		if _, err := Parse(body(schema)); err == nil {
			t.Fatal("referenced draft-07 admitted", ref)
		}
	}
	b, err := Parse(body(`{"$defs":{"node":{"type":"object","properties":{"next":{"$ref":"#/$defs/node"}}}},"$ref":"#/$defs/node"}`))
	if err != nil {
		t.Fatal("valid recursion rejected", err)
	}
	if err = b.Schema.Validate(map[string]any{"next": map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	if err = b.Schema.Validate(map[string]any{"next": 42}); err == nil {
		t.Fatal("recursive constraint ignored")
	}
}
