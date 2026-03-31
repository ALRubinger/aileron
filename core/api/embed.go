// Package spec embeds the OpenAPI specification for use by the server and docs.
package spec

import _ "embed"

//go:embed openapi.yaml
var OpenAPISpec []byte
