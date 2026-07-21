// Package migrations embeds the goose SQL migration files so binaries can run
// them without the source tree being present.
package migrations

import "embed"

// FS holds the Postgres and ClickHouse migrations under their database paths.
//
//go:embed postgres/*.sql clickhouse/*.sql
var FS embed.FS
