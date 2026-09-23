// Package streamerdb keeps per-streamer facts that must outlive the daemon but
// have no place in streamers.toml, which belongs to the user.
package streamerdb

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/vodlabs-io/recorder/internal/streamer"
	"github.com/vodlabs-io/recorder/internal/vlconfig"
	"github.com/vodlabs-io/recorder/internal/vllog"
	_ "modernc.org/sqlite"
)

// DB is the streamers.db database.
type DB struct {
	db  *sql.DB
	log *vllog.FileLogger
}

// Open opens streamers.db next to processing.db (own file, same SQLite
// settings), creating it on first use.
func Open(ctx context.Context, log *vllog.FileLogger, processingDBPath string) (*DB, error) {
	var dbPath string
	if processingDBPath != "" {
		dbPath = filepath.Join(filepath.Dir(processingDBPath), "streamers.db")
	} else {
		dbPath = vlconfig.DefaultFilePath("streamers.db")
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		return nil, fmt.Errorf("failed to create database directory: %w", err)
	}

	database, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}
	log.Debug("Database opened", "path", dbPath)

	// Limit to 1 connection to avoid "database is locked" errors in SQLite
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)

	for _, query := range []string{
		"PRAGMA journal_mode=WAL;",
		"PRAGMA synchronous=NORMAL;",
		"PRAGMA busy_timeout=5000;",
		"PRAGMA temp_store=memory;",
	} {
		if _, err = database.ExecContext(ctx, query); err != nil {
			database.Close()
			return nil, fmt.Errorf("failed to set database settings: %w", err)
		}
	}

	// Rows are never deleted when a streamer leaves the subscription: it can
	// drop out for a cycle without the user removing it, and its time must
	// survive that.
	schema := `
	CREATE TABLE IF NOT EXISTS streamer_added (
		platform TEXT NOT NULL,
		username TEXT NOT NULL,
		added_at INTEGER NOT NULL,
		PRIMARY KEY (platform, username)
	);
	`
	if _, err := database.ExecContext(ctx, schema); err != nil {
		database.Close()
		return nil, fmt.Errorf("failed to create schema: %w", err)
	}
	return &DB{db: database, log: log}, nil
}

func (d *DB) Close() error {
	return d.db.Close()
}

// Track implements vlstate.AddedTracker. A key first seen here (e.g. a streamer
// added by editing streamers.toml) is stamped with now.
func (d *DB) Track(ctx context.Context, keys []streamer.StreamerKey, now time.Time) map[streamer.StreamerKey]int64 {
	out, err := d.track(ctx, keys, now)
	if err != nil {
		d.log.Warn("Failed to track streamer added times", "error", err)
		return nil
	}
	return out
}

func (d *DB) track(ctx context.Context, keys []streamer.StreamerKey, now time.Time) (map[streamer.StreamerKey]int64, error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	insert, err := tx.PrepareContext(ctx,
		`INSERT OR IGNORE INTO streamer_added (platform, username, added_at) VALUES (?, ?, ?)`)
	if err != nil {
		return nil, err
	}
	defer insert.Close()
	for _, k := range keys {
		if _, err := insert.ExecContext(ctx, k.Platform, k.Username, now.Unix()); err != nil {
			return nil, err
		}
	}

	rows, err := tx.QueryContext(ctx, `SELECT platform, username, added_at FROM streamer_added`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	want := make(map[streamer.StreamerKey]bool, len(keys))
	for _, k := range keys {
		want[k] = true
	}
	out := make(map[streamer.StreamerKey]int64, len(keys))
	for rows.Next() {
		var k streamer.StreamerKey
		var at int64
		if err := rows.Scan(&k.Platform, &k.Username, &at); err != nil {
			return nil, err
		}
		if want[k] {
			out[k] = at
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, tx.Commit()
}

// MarkAdded records at as key's added time, replacing any earlier one: adding a
// streamer again after removing it starts its time over.
func (d *DB) MarkAdded(ctx context.Context, key streamer.StreamerKey, at time.Time) {
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO streamer_added (platform, username, added_at) VALUES (?, ?, ?)
		 ON CONFLICT (platform, username) DO UPDATE SET added_at = excluded.added_at`,
		key.Platform, key.Username, at.Unix())
	if err != nil {
		d.log.Warn("Failed to record streamer added time", "streamer", key, "error", err)
	}
}
