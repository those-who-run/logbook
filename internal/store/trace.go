package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"

	storev1 "github.com/those-who-run/contracts/gen/go/twr/store/v1"
)

// lapTrace is v1's lap_trace (v1's pitwall/data.py, v1.0.0), ported as it is:
// speed, throttle, brake, gear, RPM and position for the lap in file against
// time, thinned to at most maxPoints frames, plus both frames either side of
// each position jump (paddock#123).
//
// Thinning keeps peaks (v1's #183 R2): the frames are cut into stretches, and each
// stretch keeps its slowest and its fastest frame, so a braking point's minimum
// and a straight's top speed survive. The first and last frames are always kept,
// and so are the two frames either side of a jump (jumpMetres, v1's #349 R2), so
// a map can break its line there. Under four points, it takes every n-th frame
// instead. One departure from v1: a lap file that is missing or holds no frames
// is not found, never an empty trace (#123 R6).
func (s *Store) lapTrace(ctx context.Context, tx *sql.Tx, file string, maxPoints int) (*storev1.LapTrace, error) {
	path := filepath.Join(s.dir, filepath.FromSlash(file))
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: lap file %s", ErrNotFound, file)
	}
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM read_parquet(?)`, path).Scan(&n); err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, fmt.Errorf("%w: lap file %s holds no frames", ErrNotFound, file)
	}
	points := max(1, maxPoints)
	var size int
	var keep string
	switch {
	case n <= points:
		size, keep = 1, "true"
	case points < 4:
		size = ceilDiv(n, points)
		keep = "rn % " + strconv.Itoa(size) + " = 0"
	default:
		size = ceilDiv(n, (points-2)/2) // two frames a stretch, plus the ends
		keep = "lo = 1 OR hi = 1 OR rn = 0 OR rn = n - 1 OR jump"
	}
	sz := strconv.Itoa(size)
	rows, err := tx.QueryContext(ctx,
		"WITH f AS (SELECT *, row_number() OVER (ORDER BY t) - 1 AS rn, count(*) OVER () AS n,"+ //nolint:gosec // only integers and fixed conditions are joined in, as v1 does
			" least(coalesce(t - lag(t) OVER (ORDER BY t), 0), ?) AS dt,"+
			" coalesce(sqrt((pos_x - lag(pos_x) OVER (ORDER BY t)) ** 2"+
			" + (pos_z - lag(pos_z) OVER (ORDER BY t)) ** 2) > greatest(?, 2 / 3.6"+
			" * greatest(speed_kph, lag(speed_kph) OVER (ORDER BY t))"+
			" * (t - lag(t) OVER (ORDER BY t))), false) AS jumped FROM read_parquet(?)),"+
			" g AS (SELECT *, sum(dt) OVER (ORDER BY t ROWS UNBOUNDED PRECEDING) AS el,"+
			" jumped OR coalesce(lead(jumped) OVER (ORDER BY t), false) AS jump,"+
			" row_number() OVER (PARTITION BY rn // "+sz+" ORDER BY speed_kph, rn) AS lo,"+
			" row_number() OVER (PARTITION BY rn // "+sz+" ORDER BY speed_kph DESC, rn) AS hi"+
			" FROM f) SELECT el, speed_kph, throttle * 100.0 / 255, brake * 100.0 / 255, gear,"+
			" engine_rpm, pos_x, pos_z FROM g WHERE "+keep+" ORDER BY t",
		maxStepSecs, jumpMetres, path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	tr := &storev1.LapTrace{}
	for rows.Next() {
		var t, speed, throttle, brake, rpm, x, z float64
		var gear int32
		if err := rows.Scan(&t, &speed, &throttle, &brake, &gear, &rpm, &x, &z); err != nil {
			return nil, err
		}
		tr.T = append(tr.T, round(t, 3))
		tr.SpeedKph = append(tr.SpeedKph, speed)
		tr.ThrottlePct = append(tr.ThrottlePct, round(throttle, 1))
		tr.BrakePct = append(tr.BrakePct, round(brake, 1))
		tr.Gear = append(tr.Gear, gear)
		tr.Rpm = append(tr.Rpm, rpm)
		// A tenth of a metre: a frame at 60 Hz moves up to 1.3 m, and the map
		// needs no finer, while each extra digit costs every point a byte.
		tr.X = append(tr.X, round(x, 1))
		tr.Z = append(tr.Z, round(z, 1))
	}
	return tr, rows.Err()
}

func ceilDiv(a, b int) int { return (a + b - 1) / b }

// round rounds v to digits decimals as Python's round does: to the nearest
// decimal of the binary value itself, ties to even. strconv rounds the exact
// value, so the trace equals v1's to the bit.
func round(v float64, digits int) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return v
	}
	r, _ := strconv.ParseFloat(strconv.FormatFloat(v, 'f', digits, 64), 64)
	return r
}
