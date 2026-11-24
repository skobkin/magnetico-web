// Package oastest checks real HTTP responses against the embedded OpenAPI
// documents. It is test support and lives outside internal/web on purpose: only
// _test.go files import it, so neither it nor its JSON Schema dependency is
// linked into the shipped binary (TestBinaryDoesNotLinkTestSupport checks the
// binary's dependency list, and the web import-boundary test forbids the JSON
// Schema library in every web package).
//
// A response is accepted only if the operation documents it: the status code
// is declared (or `default`), the media type is one of the declared content
// types, every header the response requires is present, the Retry-After of a
// 429/503 is a non-negative integer, and a JSON body validates against the
// declared schema. Nothing is fetched: the only resource is the document.
package oastest

import (
	"bytes"
	"fmt"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

const docURL = "https://docs.invalid/openapi.json"

// Spec is one loaded OpenAPI document.
type Spec struct {
	doc      map[string]any
	compiler *jsonschema.Compiler
	covered  map[string]bool
}

// Load parses an OpenAPI document given as YAML.
func Load(t *testing.T, yamlDoc []byte) *Spec {
	t.Helper()
	j, err := yaml.YAMLToJSON(yamlDoc)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := jsonschema.UnmarshalJSON(bytes.NewReader(j))
	if err != nil {
		t.Fatal(err)
	}
	doc, ok := parsed.(map[string]any)
	if !ok {
		t.Fatal("the OpenAPI document is not an object")
	}
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	if err := c.AddResource(docURL, parsed); err != nil {
		t.Fatal(err)
	}
	return &Spec{doc: doc, compiler: c, covered: map[string]bool{}}
}

// Operations lists every documented operation as "METHOD /path".
func (s *Spec) Operations() []string {
	var out []string
	paths, _ := s.doc["paths"].(map[string]any)
	for p, item := range paths {
		for m := range item.(map[string]any) {
			switch m {
			case "get", "put", "post", "delete", "patch", "head", "options", "trace":
				out = append(out, strings.ToUpper(m)+" "+p)
			}
		}
	}
	return out
}

// Covered reports whether Check has accepted a response of the operation.
func (s *Spec) Covered(method, template string) bool {
	return s.covered[strings.ToUpper(method)+" "+template]
}

// lookup follows a JSON pointer ("/a/b~1c") in the document.
func (s *Spec) lookup(ptr string) (any, bool) {
	var node any = s.doc
	if ptr == "" {
		return node, true
	}
	for _, seg := range strings.Split(strings.TrimPrefix(ptr, "/"), "/") {
		seg = strings.NewReplacer("~1", "/", "~0", "~").Replace(seg)
		m, ok := node.(map[string]any)
		if !ok {
			return nil, false
		}
		if node, ok = m[seg]; !ok {
			return nil, false
		}
	}
	return node, true
}

func escape(seg string) string {
	return strings.NewReplacer("~", "~0", "/", "~1").Replace(seg)
}

// resolve follows internal $ref chains and returns the node and the pointer it
// finally lives at.
func (s *Spec) resolve(t *testing.T, node any, ptr string) (map[string]any, string) {
	t.Helper()
	for i := 0; i < 10; i++ {
		m, ok := node.(map[string]any)
		if !ok {
			t.Fatalf("OpenAPI node at %s is not an object", ptr)
		}
		ref, isRef := m["$ref"].(string)
		if !isRef {
			return m, ptr
		}
		if !strings.HasPrefix(ref, "#/") {
			t.Fatalf("external $ref %q at %s", ref, ptr)
		}
		ptr = strings.TrimPrefix(ref, "#")
		if node, ok = s.lookup(ptr); !ok {
			t.Fatalf("dangling $ref %q", ref)
		}
	}
	t.Fatalf("$ref chain too long at %s", ptr)
	return nil, ""
}

// Check validates one real response of the operation method template (the path
// as written in the document, relative to its server URL, for example
// "/torrents/{id}").
func (s *Spec) Check(t *testing.T, method, template string, status int, header http.Header, body []byte) {
	t.Helper()
	opPtr := "/paths/" + escape(template) + "/" + strings.ToLower(method)
	op, ok := s.lookup(opPtr)
	if !ok {
		t.Fatalf("%s %s is not a documented operation", method, template)
	}
	responses, _ := op.(map[string]any)["responses"].(map[string]any)
	key := strconv.Itoa(status)
	respNode, ok := responses[key]
	respPtr := opPtr + "/responses/" + key
	if !ok {
		if respNode, ok = responses["default"]; !ok {
			t.Fatalf("%s %s: status %d is not documented (declared: %v)", method, template, status, keys(responses))
		}
		respPtr = opPtr + "/responses/default"
	}
	resp, respPtr := s.resolve(t, respNode, respPtr)

	// Headers the response declares: present ones must be well formed.
	if hs, ok := resp["headers"].(map[string]any); ok {
		for name, spec := range hs {
			h, _ := spec.(map[string]any)
			v := header.Get(name)
			if v == "" {
				if req, _ := h["required"].(bool); req {
					t.Errorf("%s %s %d: required header %s is missing", method, template, status, name)
				}
				continue
			}
			if sch, _ := h["schema"].(map[string]any); sch != nil && sch["type"] == "integer" {
				if n, err := strconv.Atoi(v); err != nil || n < 0 {
					t.Errorf("%s %s %d: header %s = %q, want a non-negative integer", method, template, status, name, v)
				}
			}
		}
	}
	if status == 429 || status == 503 {
		if v := header.Get("Retry-After"); v == "" {
			t.Errorf("%s %s %d: Retry-After is missing", method, template, status)
		}
	}

	content, _ := resp["content"].(map[string]any)
	if len(content) == 0 {
		if len(bytes.TrimSpace(body)) != 0 {
			t.Errorf("%s %s %d: the response declares no content but has a body", method, template, status)
		}
		s.covered[strings.ToUpper(method)+" "+template] = true
		return
	}
	mt, _, err := mime.ParseMediaType(header.Get("Content-Type"))
	if err != nil {
		t.Fatalf("%s %s %d: Content-Type %q: %v", method, template, status, header.Get("Content-Type"), err)
	}
	media, ok := content[mt]
	if !ok {
		t.Fatalf("%s %s %d: media type %q is not documented (declared: %v)", method, template, status, mt, keys(content))
	}
	if strings.HasSuffix(mt, "json") {
		mm, mptr := s.resolve(t, media, respPtr+"/content/"+escape(mt))
		if _, has := mm["schema"]; !has {
			t.Fatalf("%s %s %d: %s documents no schema", method, template, status, mt)
		}
		schemaPtr := mptr + "/schema"
		sch, err := s.compiler.Compile(docURL + "#" + strings.NewReplacer("{", "%7B", "}", "%7D").Replace(schemaPtr))
		if err != nil {
			t.Fatalf("compiling the schema of %s %s %d: %v", method, template, status, err)
		}
		inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(body))
		if err != nil {
			t.Fatalf("%s %s %d: the body is not JSON: %v\n%.300s", method, template, status, err, body)
		}
		if err := sch.Validate(inst); err != nil {
			t.Fatalf("%s %s %d does not match the documented schema: %#v\n%.600s", method, template, status, err, body)
		}
	}
	s.covered[strings.ToUpper(method)+" "+template] = true
}

func keys(m map[string]any) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// String is for diagnostics.
func (s *Spec) String() string { return fmt.Sprintf("OpenAPI %v", s.doc["openapi"]) }
