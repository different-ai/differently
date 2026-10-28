// Package apispec embeds the OpenAPI description of the Differently API.
package apispec

import _ "embed"

// OpenAPI is the OpenAPI 3.1 document served at /openapi.yaml.
//
//go:embed openapi.yaml
var OpenAPI []byte
