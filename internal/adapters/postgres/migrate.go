// file: internal/adapters/postgres/migrate.go
package postgres

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

type migration struct {
	version int
	name    string
	sql     string
	sum     string
}

func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return nil, err
	}
	var out []migration
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		// Filenames are NNNN_name.sql — the numeric prefix is the version.
		parts := strings.SplitN(e.Name(), "_", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("migration %q: expected NNNN_name.sql", e.Name())
		}
		v, err := strconv.Atoi(parts[0])
		if err != nil {
			return nil, fmt.Errorf("migration %q: bad version prefix: %w", e.Name(), err)
		}
		b, err := migrationsFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(b)
		out = append(out, migration{
			version: v, name: e.Name(), sql: string(b),
			sum: hex.EncodeToString(sum[:]),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

// advisoryLockKey is an arbitrary constant. Every API replica calls Migrate on boot; the
// session-level advisory lock makes exactly one of them apply migrations while the others
// block and then find nothing to do. Without it, a rolling deploy of three replicas races
// three concurrent CREATE TABLE statements.
const advisoryLockKey int64 = 0x4152454e41 // "ARENA"

// Migrate applies every pending migration inside its own transaction.
//
// Checksums are recorded and verified: if a migration file is edited after it has been
// applied somewhere, this fails loudly instead of leaving two environments with silently
// different schemas. That is the difference between "reproducible" and "worked on my
// laptop".
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, advisoryLockKey); err != nil {
		return fmt.Errorf("advisory lock: %w", err)
	}
	defer conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, advisoryLockKey)

	if _, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    int PRIMARY KEY,
			name       text NOT NULL,
			checksum   text NOT NULL,
			applied_at timestamptz NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	applied := map[int]string{}
	rows, err := conn.Query(ctx, `SELECT version, checksum FROM schema_migrations`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var v int
		var sum string
		if err := rows.Scan(&v, &sum); err != nil {
			rows.Close()
			return err
		}
		applied[v] = sum
	}
	rows.Close()

	migs, err := loadMigrations()
	if err != nil {
		return err
	}

	for _, m := range migs {
		if sum, ok := applied[m.version]; ok {
			if sum != m.sum {
				return fmt.Errorf(
					"migration %s was modified after being applied (checksum %s != %s); "+
						"write a new migration instead of editing an applied one",
					m.name, m.sum, sum)
			}
			continue
		}
		err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, m.sql); err != nil {
				return fmt.Errorf("apply %s: %w", m.name, err)
			}
			_, err := tx.Exec(ctx,
				`INSERT INTO schema_migrations (version, name, checksum) VALUES ($1,$2,$3)`,
				m.version, m.name, m.sum)
			return err
		})
		if err != nil {
			return err
		}
		fmt.Printf("migrated: %s\n", m.name)
	}
	return nil
}
