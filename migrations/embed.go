// Package migrations empacota os arquivos .sql de migração do esquema do
// PostgreSQL via //go:embed, para que possam ser aplicados:
//
//  1. Pelo binário (RunMigrations em internal/adapters/postgres), lendo do FS
//     embutido — útil para deploy K8s sem arquivos soltos no filesystem.
//  2. Pelo CLI `migrate -path migrations -database $DATABASE_URL up`, lendo
//     diretamente do diretório (útil em desenvolvimento local).
//
// Os arquivos .sql são a fonte de verdade do schema. Este pacote apenas os
// expõe como embed.FS.
package migrations

import "embed"

// FS contém todos os arquivos .sql de migração. Os nomes seguem a convenção
// {YYYYMMDDHHMM}_{slug}.{up|down}.sql (ver migrations/README.md).
//
//go:embed *.sql
var FS embed.FS
