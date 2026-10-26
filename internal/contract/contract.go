// Package contract validates the admission-time contract profile. It compiles
// output schemas; it does not issue verification receipts or evaluate evidence.
package contract

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/agentnet/agentnet/internal/authority"
	"github.com/agentnet/agentnet/internal/canonical"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

const MaxBytes = 1 << 20
const dialect = "https://json-schema.org/draft/2020-12/schema"

var digest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

type Body struct {
	Deadline  time.Time `json:"deadline"`
	TaskType  string    `json:"task_type"`
	InputRefs []struct {
		Version int    `json:"version"`
		Digest  string `json:"digest"`
	} `json:"input_refs"`
	OutputSchema json.RawMessage `json:"output_schema"`
	Acceptance   struct {
		Verifier string   `json:"verifier"`
		Checks   []string `json:"checks"`
		OnFail   string   `json:"on_fail"`
	} `json:"acceptance_policy"`
	Delegation struct {
		Allow         bool     `json:"allow"`
		MaxDepth      int      `json:"max_depth"`
		MaxConcurrent int      `json:"max_concurrent_subtasks"`
		Scope         []string `json:"scope"`
	} `json:"delegation_policy"`
	Schema *jsonschema.Schema `json:"-"`
}

// Parse is deterministic and performs no network or filesystem reads. Dates are
// structurally checked here; transaction-time expiry checks belong to TCS/ICS.
func Parse(raw []byte) (Body, error) {
	var b Body
	if len(raw) == 0 || len(raw) > MaxBytes {
		return b, fmt.Errorf("contract size outside supported bounds")
	}
	// Bound nesting before JCS or schema compilation recursively processes JSON.
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	depth, tokens := 0, 0
	for {
		t, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return b, err
		}
		tokens++
		if delim, ok := t.(json.Delim); ok {
			if delim == '{' || delim == '[' {
				depth++
			} else {
				depth--
			}
		}
		if depth > 64 || tokens > 32768 {
			return b, fmt.Errorf("contract complexity limit")
		}
	}
	canonicalJSON, err := canonical.Bytes(raw)
	if err != nil {
		return b, err
	}
	if len(canonicalJSON) == 0 || canonicalJSON[0] != '{' || json.Unmarshal(canonicalJSON, &b) != nil {
		return b, fmt.Errorf("contract must be a typed object")
	}
	if b.Deadline.IsZero() || len(b.InputRefs) == 0 {
		return b, fmt.Errorf("deadline and input versions required")
	}
	for _, ref := range b.InputRefs {
		if ref.Version < 1 || !digest.MatchString(ref.Digest) {
			return b, fmt.Errorf("invalid input reference")
		}
	}
	a := b.Acceptance
	if strings.TrimSpace(a.Verifier) == "" || len(a.Checks) == 0 || (a.OnFail != "report" && a.OnFail != "retry_once" && a.OnFail != "human") {
		return b, fmt.Errorf("explicit acceptance policy required")
	}
	seen := map[string]bool{}
	for _, check := range a.Checks {
		switch check {
		case "schema", "source_binding", "sample_range", "units", "code_reproducible", "stat_consistency":
		default:
			return b, fmt.Errorf("unsupported acceptance check")
		}
		if seen[check] {
			return b, fmt.Errorf("duplicate acceptance check")
		}
		seen[check] = true
	}
	p := b.Delegation
	if p.MaxDepth < 0 || p.MaxDepth >= authority.MaxChainLength || p.MaxConcurrent < 0 || (p.Allow && (p.MaxDepth == 0 || len(p.Scope) == 0)) {
		return b, fmt.Errorf("invalid delegation policy")
	}
	seen = map[string]bool{}
	for _, scope := range p.Scope {
		if strings.TrimSpace(scope) == "" || seen[scope] {
			return b, fmt.Errorf("invalid delegation scope")
		}
		seen[scope] = true
	}
	if b.Delegation.MaxConcurrent == 0 {
		b.Delegation.MaxConcurrent = 16
	}
	var schema any
	if json.Unmarshal(b.OutputSchema, &schema) != nil {
		return b, fmt.Errorf("output schema required")
	}
	// Object and boolean schemas are both valid 2020-12 schemas. In particular,
	// false is an unsatisfiable schema, never a missing or successful check.
	if err = checkDialect(schema); err != nil {
		return b, err
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.UseLoader(offlineLoader{})
	c.AssertFormat()
	const location = "https://anet.invalid/contract/output"
	if err = c.AddResource(location, schema); err != nil {
		return b, err
	}
	b.Schema, err = c.Compile(location)
	if err == nil {
		err = compiledDialect(b.Schema, map[*jsonschema.Schema]bool{})
	}
	return b, err
}

// A $ref can target a schema outside standard schema positions (for example
// #/definitions/legacy). The compiler discovers those resources lazily. Check
// the actual compiled graph as well as syntax, with a visited set for recursion.
func compiledDialect(s *jsonschema.Schema, seen map[*jsonschema.Schema]bool) error {
	if s == nil || seen[s] {
		return nil
	}
	seen[s] = true
	if s.DraftVersion != 2020 {
		return fmt.Errorf("referenced schema changes dialect")
	}
	children := []*jsonschema.Schema{s.Ref, s.RecursiveRef, s.Not, s.If, s.Then, s.Else,
		s.PropertyNames, s.UnevaluatedProperties, s.Contains, s.Items2020,
		s.UnevaluatedItems, s.ContentSchema}
	if s.DynamicRef != nil {
		children = append(children, s.DynamicRef.Ref)
	}
	for _, list := range [][]*jsonschema.Schema{s.AllOf, s.AnyOf, s.OneOf, s.PrefixItems} {
		children = append(children, list...)
	}
	for _, m := range []map[string]*jsonschema.Schema{s.Properties, s.DependentSchemas} {
		for _, child := range m {
			children = append(children, child)
		}
	}
	for _, child := range s.PatternProperties {
		children = append(children, child)
	}
	add := func(v any) {
		switch value := v.(type) {
		case *jsonschema.Schema:
			children = append(children, value)
		case []*jsonschema.Schema:
			children = append(children, value...)
		}
	}
	add(s.AdditionalProperties)
	add(s.Items)
	add(s.AdditionalItems)
	for _, child := range s.Dependencies {
		add(child)
	}
	for _, child := range children {
		if err := compiledDialect(child, seen); err != nil {
			return err
		}
	}
	return nil
}

type offlineLoader struct{}

func (offlineLoader) Load(string) (any, error) {
	return nil, fmt.Errorf("external schema resources are not admitted; embed them in $defs")
}

// Visit schema positions only: an enum/const/example may legitimately contain a
// property named $schema as data. Embedded resources cannot change dialect.
func checkDialect(value any) error {
	if _, ok := value.(bool); ok {
		return nil
	}
	m, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("schema must be an object or boolean")
	}
	if v, exists := m["$schema"]; exists && v != dialect {
		return fmt.Errorf("only JSON Schema 2020-12 is supported")
	}
	for _, name := range []string{"$defs", "properties", "patternProperties", "dependentSchemas"} {
		if children, ok := m[name].(map[string]any); ok {
			for _, child := range children {
				if err := checkDialect(child); err != nil {
					return err
				}
			}
		}
	}
	for _, name := range []string{"allOf", "anyOf", "oneOf", "prefixItems"} {
		if children, ok := m[name].([]any); ok {
			for _, child := range children {
				if err := checkDialect(child); err != nil {
					return err
				}
			}
		}
	}
	for _, name := range []string{"not", "if", "then", "else", "items", "contains", "additionalProperties", "propertyNames", "unevaluatedProperties", "unevaluatedItems", "contentSchema"} {
		if child, exists := m[name]; exists {
			if err := checkDialect(child); err != nil {
				return err
			}
		}
	}
	return nil
}
