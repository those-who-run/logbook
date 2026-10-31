"""Writes v1's lap traces for logbook's lap files: the expected values of
internal/store's trace test (paddock#123 AC1).

It runs v1 1.0.0's own lap_trace (those_who_run.pitwall.data) on the lap files
logbook writes from testdata/replays/suzuka-pit-stop-2026-10-15.jsonl.gz, with
v1's index lookup pointed at those files, at each of POINTS, and writes one JSON
file per lap: {"<points>": {"t": [...], ...}}. Run it with v1's environment:

    <v1>/.venv/bin/python -I testdata/traces/v1_lap_trace.py DATA_DIR OUT_DIR

where DATA_DIR is a logbook data directory holding that replay's laps (the test
writes one the same way) and OUT_DIR is testdata/traces.
"""

import json
import sys
from pathlib import Path

from those_who_run.pitwall import data

POINTS = (1500, 40, 10, 3)


def main(data_dir: Path, out_dir: Path) -> None:
    for path in sorted(data_dir.glob("sessions/*/*/lap-*.parquet")):
        lap = int(path.stem.removeprefix("lap-"))
        data.lap_file = lambda _d, _s, _l, p=path: (p.name, 0)
        data._lap_file = lambda _d, _s, _f, p=path: p
        out = {}
        for points in POINTS:
            tr = data.lap_trace(data_dir, path.parent.name, lap, points)
            out[str(points)] = {
                k: list(getattr(tr, k))
                for k in ("t", "speed_kph", "throttle_pct", "brake_pct", "gear", "rpm", "x", "z")
            }
        (out_dir / f"suzuka-pit-stop-lap-{lap:03d}.json").write_text(json.dumps(out) + "\n")


if __name__ == "__main__":
    main(Path(sys.argv[1]), Path(sys.argv[2]))
