// Package migrations embeds the SQL files so the binary can migrate a database
// without shipping a separate directory or requiring a migration tool.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
