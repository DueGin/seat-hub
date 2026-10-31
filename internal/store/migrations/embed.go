// Package migrations embeds the ordered SQL schema files.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
