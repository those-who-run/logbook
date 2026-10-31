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
	// 3: the session fields contracts v0.1.4 added (paddock#276, paddock#289), as
	// the events carry them. The started_ and ended_ columns are the event's word,
	// kept apart from the columns logbook works out from the laps (fuel_at_start,
	// fuel_used, final_position). session_kind keeps v1's name for ecu's final
	// verdict on the session's type; type is its verdict on the first frame. Each is
	// NULL in a row written before, and for an event from before v0.1.4.
	`
ALTER TABLE sessions ADD COLUMN started_fuel_at_start DOUBLE;
ALTER TABLE sessions ADD COLUMN last_seq BIGINT;
ALTER TABLE sessions ADD COLUMN ended_fuel_used DOUBLE;
ALTER TABLE sessions ADD COLUMN ended_final_position INTEGER;
ALTER TABLE sessions ADD COLUMN session_kind VARCHAR;
`,
	// 4: a session continued in a later ecu run after a restart (paddock#76,
	// session.continued). session_runs holds each run that continued a session:
	// its frames from from_seq, in that run's seqs, and the session's laps from
	// from_lap, the lap the gap fell in. sessions.run stays the run of the
	// session's first frame. A seq counts within its run, so a staged frame is
	// keyed by its run as well: the earlier run's frames never mix with the
	// later run's under the same seq. DuckDB cannot change a primary key in
	// place, so frames_pending is made again with its rows. With contracts v0.2
	// the end reasons v0.1.5 did not name, stored as their bare numbers, get
	// their names, as a session ended from then on stores them.
	`
CREATE TABLE session_runs (
    src VARCHAR NOT NULL,
    session_id VARCHAR NOT NULL,
    run VARCHAR NOT NULL,
    from_seq BIGINT NOT NULL,
    from_lap INTEGER NOT NULL,
    continued_at TIMESTAMP NOT NULL,
    PRIMARY KEY (src, session_id, run)
);
CREATE TABLE frames_pending_4 (
    src VARCHAR NOT NULL,
    session_id VARCHAR NOT NULL,
    seq BIGINT NOT NULL,
    run VARCHAR NOT NULL,
    frame BLOB NOT NULL,
    PRIMARY KEY (src, session_id, run, seq)
);
INSERT INTO frames_pending_4 SELECT src, session_id, seq, run, frame FROM frames_pending;
DROP TABLE frames_pending;
ALTER TABLE frames_pending_4 RENAME TO frames_pending;
UPDATE sessions SET end_reason = CASE end_reason
    WHEN '9' THEN 'timed_finish' WHEN '10' THEN 'menu' WHEN '11' THEN 'pause_limit' END
WHERE end_reason IN ('9', '10', '11');
`,
	// 5: what ecu and podium publish that the pit wall reads (Rosberg, the schema 5
	// issue). summary is session.ended's SessionSummary, whole, as protobuf JSON;
	// car_name is the start's, then the summary's; track_id, track_name and
	// track_sure are the summary's track verdict, sure when probable or confirmed as
	// contracts' LiveTrack.sure and v1's sure_track_id read it; posted_at is
	// card.posted's. A lap's pit_role and counts_for_best are its lap.completed's.
	// Each is NULL in a row written before, until `logbook backfill` reads the
	// events back from EVENTS.
	`
ALTER TABLE sessions ADD COLUMN summary JSON;
ALTER TABLE sessions ADD COLUMN car_name VARCHAR;
ALTER TABLE sessions ADD COLUMN track_id INTEGER;
ALTER TABLE sessions ADD COLUMN track_name VARCHAR;
ALTER TABLE sessions ADD COLUMN track_sure BOOLEAN;
ALTER TABLE sessions ADD COLUMN posted_at TIMESTAMP;
ALTER TABLE laps ADD COLUMN pit_role VARCHAR;
ALTER TABLE laps ADD COLUMN counts_for_best BOOLEAN;
`,
	// 6: an open session's facts, what its events say so far (paddock#337), for the
	// open session's summary (paddock#122): joined_on_lap and fuel_capacity from
	// session.started; best_lap from lap.best, with best_lap_ms; paused_ms from
	// session.paused and session.resumed; gaps (JSON, each a SessionGap) from
	// session.continued; pit_stops (JSON, each a PitStop) from pit.exited; kind
	// (JSON, the SessionKind) from session.kind, with session_kind and total_laps;
	// track (JSON, the TrackVerdict) from session.track, or a continue's carried
	// track while there is none, with the track columns of 5 and track_label
	// (contracts' format.TrackLabel), which the track filter keys on. The track,
	// the kind and the best lap are the last event's: track_seq, kind_seq and
	// best_seq keep the EVENTS sequence that set each, so an older event delivered
	// again changes nothing. At the end, the summary's. Each is NULL in a row
	// written before, until `logbook backfill` reads the events back from EVENTS.
	`
ALTER TABLE sessions ADD COLUMN joined_on_lap INTEGER;
ALTER TABLE sessions ADD COLUMN fuel_capacity DOUBLE;
ALTER TABLE sessions ADD COLUMN best_lap INTEGER;
ALTER TABLE sessions ADD COLUMN paused_ms INTEGER;
ALTER TABLE sessions ADD COLUMN gaps JSON;
ALTER TABLE sessions ADD COLUMN pit_stops JSON;
ALTER TABLE sessions ADD COLUMN kind JSON;
ALTER TABLE sessions ADD COLUMN track JSON;
ALTER TABLE sessions ADD COLUMN track_label VARCHAR;
ALTER TABLE sessions ADD COLUMN kind_seq BIGINT;
ALTER TABLE sessions ADD COLUMN track_seq BIGINT;
ALTER TABLE sessions ADD COLUMN best_seq BIGINT;
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
