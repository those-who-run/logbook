package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// migrations is the index's schema, as one ordered list: migrations[i] takes the
// index from version i to version i+1. A schema change is a new entry at the end;
// an entry that has been released is never edited. The columns v1's index has keep
// v1's names.
var migrations = []string{
	// 1: sessions and laps, keyed by source so live sessions, replays and parity
	// runs share the index (paddock#37). Readers show `live` unless asked.
	`
CREATE TABLE sessions (
    src VARCHAR NOT NULL,
    id VARCHAR NOT NULL,
    started_at TIMESTAMP NOT NULL,
    ended_at TIMESTAMP,
    type VARCHAR NOT NULL,
    car_code INTEGER NOT NULL,
    total_laps INTEGER,
    joined_part_way BOOLEAN NOT NULL,
    first_seq BIGINT NOT NULL,
    laps_completed INTEGER NOT NULL DEFAULT 0,
    best_lap_ms INTEGER,
    fuel_at_start DOUBLE,
    fuel_used DOUBLE,
    final_position INTEGER,
    end_reason VARCHAR,
    PRIMARY KEY (src, id)
);
CREATE TABLE laps (
    src VARCHAR NOT NULL,
    session_id VARCHAR NOT NULL,
    lap INTEGER NOT NULL,
    lap_time_ms INTEGER,
    complete BOOLEAN NOT NULL,
    fuel_start DOUBLE NOT NULL,
    fuel_end DOUBLE NOT NULL,
    fuel_used DOUBLE NOT NULL,
    position INTEGER,
    first_seq BIGINT NOT NULL,
    last_seq BIGINT NOT NULL,
    frames INTEGER NOT NULL,
    top_speed_kph DOUBLE,
    min_speed_kph DOUBLE,
    path_m DOUBLE NOT NULL,
    max_jump_m DOUBLE NOT NULL,
    file VARCHAR NOT NULL,
    recorded_at TIMESTAMP NOT NULL,
    PRIMARY KEY (src, session_id, lap)
);
`,
	// 2: frames staged until their lap is written (paddock#38). A fetched batch of
	// frames is written here and acked, so a lap in progress survives a restart
	// without holding thousands of frames unacked on the bus. A frame is its
	// protobuf encoding; run is the ecu run that published it, from its ce-id, and
	// a session's run is the run of its first staged frame, so a frame of another
	// run (ecu restarted part way) never joins it.
	`
ALTER TABLE sessions ADD COLUMN run VARCHAR;
CREATE TABLE frames_pending (
    src VARCHAR NOT NULL,
    session_id VARCHAR NOT NULL,
    seq BIGINT NOT NULL,
    run VARCHAR NOT NULL,
    frame BLOB NOT NULL,
    PRIMARY KEY (src, session_id, seq)
);
`,
}

// SchemaVersion is the index's schema version this code writes.
var SchemaVersion = len(migrations)

// migrate brings db's schema up to SchemaVersion, each step in a transaction of its
// own, and refuses a database newer than the code.
func migrate(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("store: schema version: %w", err)
	}
	have, err := schemaVersion(ctx, db)
	if err != nil {
		return err
	}
	if have > SchemaVersion {
		return fmt.Errorf("store: %s is at schema version %d, newer than this logbook's %d: run a newer logbook",
			indexFile, have, SchemaVersion)
	}
	for v := have; v < SchemaVersion; v++ {
		if err := step(ctx, db, v); err != nil {
			return fmt.Errorf("store: migrate to schema version %d: %w", v+1, err)
		}
	}
	return nil
}

// schemaVersion is the version the index records, 0 for a new one.
func schemaVersion(ctx context.Context, db *sql.DB) (int, error) {
	var v int
	err := db.QueryRowContext(ctx, `SELECT version FROM schema_version`).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("store: schema version: %w", err)
	}
	return v, nil
}

// step runs migrations[from] and records version from+1, together or not at all.
func step(ctx context.Context, db *sql.DB, from int) (err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { err = rollback(tx, err) }()
	if _, err = tx.ExecContext(ctx, migrations[from]); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM schema_version`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO schema_version VALUES (?)`, from+1); err != nil {
		return err
	}
	return tx.Commit()
}

// rollback rolls tx back when err says the work failed, and adds what the rollback
// itself says. After a commit, failed or not, there is nothing to roll back.
func rollback(tx *sql.Tx, err error) error {
	if err == nil {
		return nil
	}
	if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
		return errors.Join(err, rbErr)
	}
	return err
}
