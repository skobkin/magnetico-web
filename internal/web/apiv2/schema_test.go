package apiv2

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/skobkin/magnetico-web/codec"
	"github.com/skobkin/magnetico-web/internal/web/apidocs"
)

// schema compiles one component schema of the embedded OpenAPI v2 document.
// Nothing is fetched: the only resource is the document itself.
func schema(t *testing.T, name string) *jsonschema.Schema {
	t.Helper()
	j, err := yaml.YAMLToJSON(apidocs.V2())
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(j))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	const url = "https://docs.invalid/magnetico-v2.json"
	if err := c.AddResource(url, doc); err != nil {
		t.Fatal(err)
	}
	s, err := c.Compile(url + "#/components/schemas/" + name)
	if err != nil {
		t.Fatalf("compiling %s: %v", name, err)
	}
	return s
}

func validate(t *testing.T, s *jsonschema.Schema, body []byte) {
	t.Helper()
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("not JSON: %v\n%s", err, body)
	}
	if err := s.Validate(inst); err != nil {
		t.Fatalf("response does not match the schema: %#v\n%.600s", err, body)
	}
}

func rejects(t *testing.T, s *jsonschema.Schema, body string) {
	t.Helper()
	inst, err := jsonschema.UnmarshalJSON(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if s.Validate(inst) == nil {
		t.Fatalf("the schema accepted an invalid document: %s", body)
	}
}

// Real responses of every JSON endpoint validate against the documented
// schemas, including the exactly-one-of name/name_base64 rule.
func TestResponsesMatchTheDocumentedSchemas(t *testing.T) {
	tr := staticTrackers{"udp://tracker.one:1337/announce"}
	e := newEnv(t, func(o *envOpts) { o.trackers = tr; o.view.MaxTreeFiles = 150 })
	odd := []codec.File{
		{Path: "100%/x", Size: 1}, {Path: "bad\xff\xfe/x", Size: 8}, {Path: "/lead/x", Size: 6}, {Path: "mid//dle/x", Size: 7},
		{Path: "dup/same", Size: 10}, {Path: "dup/same", Size: 10}, {Path: "тест/файл", Size: 12}, {Path: "end/", Size: 13},
		{Path: "ctl\x01‮/x", Size: 9}, {Path: "q\"uo\\te</>& ", Size: 14}, {Path: "top-level-file", Size: 20},
	}
	var many []codec.File
	for i := 0; i < 400; i++ {
		many = append(many, codec.File{Path: fmt.Sprintf("d%d/f%03d", i%5, i), Size: 1})
	}
	oddID := e.seed("odd names", 0, 1_700_000_000, odd)
	manyID := e.seed("many", 0, 1_700_000_001, many)
	single := e.seed("single", 99, 1_700_000_002, nil)

	torrentPage, details, treeS, dirS, problemS := schema(t, "TorrentPage"), schema(t, "TorrentDetails"), schema(t, "FileTree"), schema(t, "FileDirPage"), schema(t, "Problem")

	validate(t, torrentPage, e.api("/api/v2/torrents?limit=2").Body)
	validate(t, torrentPage, e.api("/api/v2/torrents?q=nomatchhere").Body)
	for _, id := range []int32{oddID, manyID, single} {
		validate(t, details, e.api(fmt.Sprintf("/api/v2/torrents/%d", id)).Body)
	}
	validate(t, treeS, e.api(fmt.Sprintf("/api/v2/torrents/%d/files/tree", oddID)).Body)

	// Every directory page of both torrents, with several page sizes.
	for _, id := range []int32{oddID, manyID} {
		for _, limit := range []int{1, 3, 1000} {
			var walk func(dir string)
			walk = func(dir string) {
				offset := 0
				for {
					r := e.api(fmt.Sprintf("/api/v2/torrents/%d/files?dir=%s&offset=%d&limit=%d", id, dir, offset, limit))
					validate(t, dirS, r.Body)
					var pg struct {
						Entries []struct {
							Type string `json:"type"`
							Path string `json:"path"`
						} `json:"entries"`
						NextOffset *int `json:"next_offset"`
					}
					if err := json.Unmarshal(r.Body, &pg); err != nil {
						t.Fatal(err)
					}
					for _, en := range pg.Entries {
						if en.Type == "dir" {
							walk(en.Path)
						}
					}
					if pg.NextOffset == nil {
						return
					}
					offset = *pg.NextOffset
				}
			}
			walk("")
		}
	}

	// Problem documents of the main failure classes.
	for name, body := range map[string][]byte{
		"401": e.do("GET", "/api/v2/torrents", nil).Body,
		"404": e.api("/api/v2/torrents/424242").Body,
		"400": e.api("/api/v2/torrents?limit=0").Body,
		"422": e.api(fmt.Sprintf("/api/v2/torrents/%d/files/tree", manyID)).Body,
	} {
		validate(t, problemS, body)
		_ = name
	}

	// The schemas really constrain: they reject the typical mistakes.
	rejects(t, details, `{"id":1,"info_hash":"abc","name":"x","total_size":1,"file_count":1,"discovered_at":"2023-01-01T00:00:00Z","magnet_uri":"m","full_tree_available":true}`)
	rejects(t, details, `{"id":1,"info_hash":"`+strings.Repeat("a", 40)+`","name":"x","total_size":1,"file_count":1,"discovered_at":"yesterday","magnet_uri":"m","full_tree_available":true}`)
	rejects(t, details, `{"id":1,"info_hash":"`+strings.Repeat("a", 40)+`","name":"x","total_size":1,"file_count":1,"discovered_at":"2023-01-01T00:00:00Z","magnet_uri":"m"}`)
	rejects(t, schema(t, "FileEntry"), `{"type":"file","name":null,"size":1,"file_index":0,"duplicate":false}`)                     // null name needs name_base64
	rejects(t, schema(t, "FileEntry"), `{"type":"file","name":"a","name_base64":"YQ==","size":1,"file_index":0,"duplicate":false}`) // both forms at once
	rejects(t, schema(t, "FileEntry"), `{"type":"file","size":1,"file_index":0,"duplicate":false}`)                                 // neither
	validate(t, schema(t, "FileEntry"), []byte(`{"type":"file","name":null,"name_base64":"YmFk//4=","size":1,"file_index":0,"duplicate":false}`))
	validate(t, schema(t, "FileEntry"), []byte(`{"type":"file","name":"","size":1,"file_index":0,"duplicate":false}`)) // empty component
	rejects(t, problemS, `{"type":"urn:magnetico:problem:made-up","title":"x","status":400}`)
	rejects(t, dirS, `{"version":2,"path":"","ancestors":[],"dir":{"file_count":0,"size":0,"dir_count":0,"entry_count":0},"offset":0,"limit":160,"next_offset":null,"entries":[]}`)
	rejects(t, dirS, `{"version":1,"path":"","ancestors":[],"dir":{"file_count":0,"size":0,"dir_count":0,"entry_count":0},"offset":0,"limit":160,"next_offset":null}`)
}
