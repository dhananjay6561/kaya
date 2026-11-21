// Package contracts embeds the JSON Schemas and sample payloads shared by every Kaya service.
package contracts

import "embed"

// Schemas holds schemas/v1/*.schema.json.
//
//go:embed schemas
var Schemas embed.FS

// Samples holds samples/v1/<schema>/{valid,invalid}/*.json, which every side's tests check.
//
//go:embed samples
var Samples embed.FS
