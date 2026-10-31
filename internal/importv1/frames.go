package importv1

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
)

// v1's lap files keep most of a frame, but not all of it: store.py's
// FRAME_COLUMNS leave out GT7's lap total, its last and best lap times, the fuel
// tank's capacity, the field's size and the cars-on-track flag, and the frames
// v1 did not store (the menus, the pause menu, the grid before the start). ecu's
// race state reads those fields, so each frame is made whole again before it goes
// through salsa's v1 mapping:
//
//   - total_laps, total_cars, fuel_capacity and cars_on_track are the session's
//     own in v1's rows: they stay the same through a session in GT7;
//   - last_lap_ms is the time v1 stored for the last lap before the frame's lap,
//     and best_lap_ms the best of those so far, -1 before there is one, as GT7
//     sends them: GT7 sets both as the lap counter goes up, which is when v1
//     stored the lap's time (store.py's _flush_lap reads last_lap_ms then).
//
// Everything else is the frame's own. The frames then go through salsa and ecu
// as a recording's would, so nothing of ecu's is reimplemented here (R2).

// sessionFacts are the fields the lap files lack, from v1's rows.
type sessionFacts struct {
	totalLaps    int32
	totalCars    int32
	fuelCapacity float64
	carsOnTrack  bool
	// lapTimes is each lap's time v1 stored, by lap number, for the complete laps.
	lapTimes map[int32]int32
}

func factsOf(sum *v1SessionSummary) sessionFacts {
	f := sessionFacts{totalLaps: sum.TotalLaps, totalCars: -1, lapTimes: map[int32]int32{}}
	if sum.TotalCars != nil && *sum.TotalCars > 0 {
		f.totalCars = *sum.TotalCars
	}
	if sum.FuelCapacity != nil {
		f.fuelCapacity = *sum.FuelCapacity
	}
	// v1's session type is "race" exactly when GT7's cars-on-track flag was set
	// on the first frame (store.py's session_type); the kind's record says it too.
	f.carsOnTrack = sum.Type == "race"
	if sum.Kind != nil && sum.Kind.CarsOnTrack != nil {
		f.carsOnTrack = *sum.Kind.CarsOnTrack
	}
	for _, l := range sum.Laps {
		if l.Complete && l.LapTimeMs != nil && *l.LapTimeMs > 0 {
			f.lapTimes[l.Lap] = *l.LapTimeMs
		}
	}
	return f
}

// gameTimes are GT7's last and best lap times as it would have sent them on a
// frame of lap: the time of the last lap before it that v1 timed, and the best of
// those, -1 for none.
func (f sessionFacts) gameTimes(lap int32) (last, best int32) {
	last, best = -1, -1
	lastLap := int32(-1)
	for n, ms := range f.lapTimes {
		if n >= lap {
			continue
		}
		if n > lastLap {
			lastLap, last = n, ms
		}
		if best < 0 || ms < best {
			best = ms
		}
	}
	return last, best
}

// recordingOf is v1's lap frames as a v1 recording, one RaceState.to_dict line
// each (format version 2), made whole with facts, for salsa.NewRecording.
func recordingOf(frames []v1Frame, facts sessionFacts) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for i, f := range frames {
		lap := int32(num(f["lap"]))
		last, best := facts.gameTimes(lap)
		position := num(f["position"])
		if _, ok := f["position"]; !ok {
			position = -1 // v1 stored GT7's -1 or 0 as NULL (RaceState.position)
		}
		line := map[string]any{
			"v":                 2,
			"t":                 num(f["t"]),
			"packet_id":         int64(num(f["packet_id"])),
			"phase":             f["phase"],
			"current_lap":       lap,
			"total_laps":        facts.totalLaps,
			"best_lap_ms":       best,
			"last_lap_ms":       last,
			"fuel_level":        num(f["fuel_level"]),
			"fuel_capacity":     facts.fuelCapacity,
			"speed_kph":         num(f["speed_kph"]),
			"engine_rpm":        num(f["engine_rpm"]),
			"tire_temp":         wheels(f, "tire_%s_temp"),
			"water_temp":        num(f["water_temp"]),
			"oil_temp":          num(f["oil_temp"]),
			"track_id":          nil,
			"race_start_pos":    int64(position),
			"total_cars":        facts.totalCars,
			"cars_on_track":     facts.carsOnTrack,
			"pos_x":             num(f["pos_x"]),
			"pos_y":             num(f["pos_y"]),
			"pos_z":             num(f["pos_z"]),
			"vel_x":             num(f["vel_x"]),
			"vel_y":             num(f["vel_y"]),
			"vel_z":             num(f["vel_z"]),
			"rot_x":             num(f["rot_x"]),
			"rot_y":             num(f["rot_y"]),
			"rot_z":             num(f["rot_z"]),
			"orientation":       num(f["orientation"]),
			"ang_vel_x":         num(f["ang_vel_x"]),
			"ang_vel_y":         num(f["ang_vel_y"]),
			"ang_vel_z":         num(f["ang_vel_z"]),
			"throttle":          int64(num(f["throttle"])),
			"brake":             int64(num(f["brake"])),
			"current_gear":      int64(num(f["gear"])),
			"suggested_gear":    int64(num(f["suggested_gear"])),
			"tire_radius":       wheels(f, "tire_%s_radius"),
			"suspension_height": wheels(f, "sus_%s"),
			"wheel_rps":         wheels(f, "wheel_%s_rps"),
			"boost_pressure":    num(f["boost_pressure"]),
			"oil_pressure":      num(f["oil_pressure"]),
			"body_height":       num(f["body_height"]),
			"car_code":          int64(num(f["car_code"])),
			"time_of_day_ms":    int64(num(f["time_of_day_ms"])),
		}
		if _, ok := line["phase"].(string); !ok {
			return nil, fmt.Errorf("frame %d: no phase", i+1)
		}
		if err := enc.Encode(line); err != nil {
			return nil, fmt.Errorf("frame %d: %w", i+1, err)
		}
	}
	return buf.Bytes(), nil
}

// wheels is the four wheels' values of a column pattern, fl, fr, rl, rr.
func wheels(f v1Frame, pattern string) [4]float64 {
	var w [4]float64
	for i, wheel := range []string{"fl", "fr", "rl", "rr"} {
		w[i] = num(f[fmt.Sprintf(pattern, wheel)])
	}
	return w
}

// num is a column's value as a number: 0 for NULL or a column the file lacks,
// and for a NaN or an infinity, which JSON cannot carry.
func num(v any) float64 {
	var f float64
	switch v := v.(type) {
	case float64:
		f = v
	case float32:
		f = float64(v)
	case int64:
		f = float64(v)
	case int32:
		f = float64(v)
	case int16:
		f = float64(v)
	case int8:
		f = float64(v)
	case int:
		f = float64(v)
	case uint8:
		f = float64(v)
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0
	}
	return f
}
