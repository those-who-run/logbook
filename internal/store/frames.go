package store

import (
	"cmp"
	"database/sql/driver"
	"math"
	"slices"
	"strings"

	"github.com/duckdb/duckdb-go/v2"
	telemetryv1 "github.com/those-who-run/contracts/gen/go/twr/telemetry/v1"
)

// FrameSchemaVersion is the version of a lap file's columns, recorded in each
// file's key-value metadata as frame_schema_version. A change to frameColumns
// raises it.
const FrameSchemaVersion = 1

// frameColumns are a lap file's columns: `seq`, then v1's frame columns by name and
// type, in v1's order (v1's store.py, FRAME_COLUMNS).
var frameColumns = []struct{ name, typ string }{
	{"seq", "BIGINT"},
	{"t", "DOUBLE"},
	{"packet_id", "BIGINT"},
	{"lap", "INTEGER"},
	{"phase", "VARCHAR"},
	{"pos_x", "DOUBLE"},
	{"pos_y", "DOUBLE"},
	{"pos_z", "DOUBLE"},
	{"vel_x", "DOUBLE"},
	{"vel_y", "DOUBLE"},
	{"vel_z", "DOUBLE"},
	{"speed_kph", "DOUBLE"},
	{"engine_rpm", "DOUBLE"},
	{"gear", "INTEGER"},
	{"suggested_gear", "INTEGER"},
	{"throttle", "INTEGER"},
	{"brake", "INTEGER"},
	{"fuel_level", "DOUBLE"},
	{"tire_fl_temp", "DOUBLE"},
	{"tire_fr_temp", "DOUBLE"},
	{"tire_rl_temp", "DOUBLE"},
	{"tire_rr_temp", "DOUBLE"},
	{"tire_fl_radius", "DOUBLE"},
	{"tire_fr_radius", "DOUBLE"},
	{"tire_rl_radius", "DOUBLE"},
	{"tire_rr_radius", "DOUBLE"},
	{"sus_fl", "DOUBLE"},
	{"sus_fr", "DOUBLE"},
	{"sus_rl", "DOUBLE"},
	{"sus_rr", "DOUBLE"},
	{"wheel_fl_rps", "DOUBLE"},
	{"wheel_fr_rps", "DOUBLE"},
	{"wheel_rl_rps", "DOUBLE"},
	{"wheel_rr_rps", "DOUBLE"},
	{"water_temp", "DOUBLE"},
	{"oil_temp", "DOUBLE"},
	{"oil_pressure", "DOUBLE"},
	{"boost_pressure", "DOUBLE"},
	{"body_height", "DOUBLE"},
	{"position", "INTEGER"},
	{"time_of_day_ms", "BIGINT"},
	{"car_code", "INTEGER"},
	{"track_id", "INTEGER"},
	{"rot_x", "DOUBLE"},
	{"rot_y", "DOUBLE"},
	{"rot_z", "DOUBLE"},
	{"orientation", "DOUBLE"},
	{"ang_vel_x", "DOUBLE"},
	{"ang_vel_y", "DOUBLE"},
	{"ang_vel_z", "DOUBLE"},
}

// createFramesTable is the scratch table a lap's frames are appended to before
// they are copied to Parquet.
var createFramesTable = func() string {
	cols := make([]string, len(frameColumns))
	for i, c := range frameColumns {
		cols[i] = c.name + " " + c.typ
	}
	return "CREATE TABLE frames (" + strings.Join(cols, ", ") + ")"
}()

// frameRow is f's values in frameColumns' order, as the appender takes them.
func frameRow(f *telemetryv1.Frame) []driver.Value {
	return []driver.Value{
		int64(f.GetSeq()),
		f.GetT(),
		int64(f.GetPacketId()),
		f.GetLap(),
		phaseName(f.GetPhase()),
		f.GetPosX(), f.GetPosY(), f.GetPosZ(),
		f.GetVelX(), f.GetVelY(), f.GetVelZ(),
		f.GetSpeedKph(),
		f.GetEngineRpm(),
		f.GetGear(),
		f.GetSuggestedGear(),
		f.GetThrottle(),
		f.GetBrake(),
		f.GetFuelLevel(),
		f.GetTireFlTemp(), f.GetTireFrTemp(), f.GetTireRlTemp(), f.GetTireRrTemp(),
		f.GetTireFlRadius(), f.GetTireFrRadius(), f.GetTireRlRadius(), f.GetTireRrRadius(),
		f.GetSusFl(), f.GetSusFr(), f.GetSusRl(), f.GetSusRr(),
		f.GetWheelFlRps(), f.GetWheelFrRps(), f.GetWheelRlRps(), f.GetWheelRrRps(),
		f.GetWaterTemp(),
		f.GetOilTemp(),
		f.GetOilPressure(),
		f.GetBoostPressure(),
		f.GetBodyHeight(),
		optional(f.Position),
		int64(f.GetTimeOfDayMs()),
		f.GetCarCode(),
		optional(f.TrackId),
		f.GetRotX(), f.GetRotY(), f.GetRotZ(),
		f.GetOrientation(),
		f.GetAngVelX(), f.GetAngVelY(), f.GetAngVelZ(),
	}
}

// optional is an optional field's value, or NULL when it is absent.
func optional(v *int32) driver.Value {
	if v == nil {
		return nil
	}
	return *v
}

// phaseName is a phase as v1 wrote it in the phase column: `in_race`, `finished`.
func phaseName(p telemetryv1.Phase) string {
	return enumName(p.String(), "PHASE_")
}

// enumName is a protobuf enum value's name without its prefix, in lower case, as
// v1 spelt the same values.
func enumName(name, prefix string) string {
	return strings.ToLower(strings.TrimPrefix(name, prefix))
}

// racingFrames are the frames that go in a lap's file, in seq order, each seq once:
// those in [first, last] whose phase is in race, and the finished frame that closes
// a race (one that follows an in-race frame), as v1's racing_frame kept them.
// Frames outside the range still say whether the frame after them follows a race.
func racingFrames(frames []*telemetryv1.Frame, first, last uint32) []*telemetryv1.Frame {
	sorted := slices.Clone(frames)
	slices.SortStableFunc(sorted, func(a, b *telemetryv1.Frame) int { return cmp.Compare(a.GetSeq(), b.GetSeq()) })
	sorted = slices.CompactFunc(sorted, func(a, b *telemetryv1.Frame) bool { return a.GetSeq() == b.GetSeq() })
	var out []*telemetryv1.Frame
	prevInRace := false
	for _, f := range sorted {
		phase := f.GetPhase()
		racing := phase == telemetryv1.Phase_PHASE_IN_RACE ||
			(phase == telemetryv1.Phase_PHASE_FINISHED && prevInRace)
		if racing && f.GetSeq() >= first && f.GetSeq() <= last {
			out = append(out, f)
		}
		prevInRace = phase == telemetryv1.Phase_PHASE_IN_RACE
	}
	return out
}

// lapFacts are what a lap row records from its frames alone, so a reader never
// opens the file for them (v1 #115).
type lapFacts struct {
	topSpeedKph, minSpeedKph *float64
	// pathM is the distance along the frames' positions in the horizontal plane
	// (x and z; y is up), and maxJumpM the largest single step of it, in metres.
	pathM, maxJumpM float64
}

func factsOf(frames []*telemetryv1.Frame) lapFacts {
	var facts lapFacts
	for i, f := range frames {
		speed := f.GetSpeedKph()
		if facts.topSpeedKph == nil || speed > *facts.topSpeedKph {
			facts.topSpeedKph = &speed
		}
		if facts.minSpeedKph == nil || speed < *facts.minSpeedKph {
			facts.minSpeedKph = &speed
		}
		if i == 0 {
			continue
		}
		prev := frames[i-1]
		step := math.Hypot(f.GetPosX()-prev.GetPosX(), f.GetPosZ()-prev.GetPosZ())
		facts.pathM += step
		facts.maxJumpM = max(facts.maxJumpM, step)
	}
	return facts
}

// appendFrames appends frames to the scratch table through conn, the driver's own
// connection, with DuckDB's appender: row-by-row inserts take minutes for a long
// lap (v1, 2026-10-14).
func appendFrames(conn driver.Conn, frames []*telemetryv1.Frame) (err error) {
	a, err := duckdb.NewAppenderFromConn(conn, "", "frames")
	if err != nil {
		return err
	}
	defer func() {
		if cerr := a.Close(); err == nil {
			err = cerr
		}
	}()
	for _, f := range frames {
		if err := a.AppendRow(frameRow(f)...); err != nil {
			return err
		}
	}
	return nil
}
