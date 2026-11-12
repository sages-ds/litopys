// Package migrations embeds the numbered SQL migrations into the litopys binary.
// Files live here, inside the core module, because //go:embed cannot reach above its module.
package migrations

import "embed"

// FS holds every NNNN_name.sql file of this directory.
//
//go:embed *.sql
var FS embed.FS
