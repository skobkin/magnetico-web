// Package apidocs holds the embedded OpenAPI documents and serves the public
// API documentation (docs/specs/web-v1.md §9). The documents describe only
// implemented endpoints; tests keep them consistent with the router.
package apidocs

import _ "embed"

//go:embed openapi/v2.yaml
var v2 []byte

// V2 returns the OpenAPI document of /api/v2.
func V2() []byte { return append([]byte(nil), v2...) }
