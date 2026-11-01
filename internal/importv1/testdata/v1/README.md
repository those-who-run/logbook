# v1's data directory, from one recording

A v1 data directory as v1.0.0's store writes it (`index.sqlite`,
`sessions/<id>/lap-NN.parquet`, `sessions/<id>/summary.json`), made by v1.0.0
replaying `real-nuc-suzuka-pit-stop-2026-10-15.jsonl.gz` (salsa's
`testdata/v1/`) into a store of its own, with v1's environment
(`uv sync`):

```sh
those-who-run replay --store <dir> --clock 2026-10-15T18:00:00Z \
    real-nuc-suzuka-pit-stop-2026-10-15.jsonl.gz
```

One session, `20261015-180014`: a 6-lap race at Suzuka East with a pit stop
on lap 3, as v1 stored it. `engineer.lock` is left out. The tests copy it
before they change it.
