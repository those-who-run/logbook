# Changelog

## [0.1.7](https://github.com/those-who-run/logbook/compare/v0.1.6...v0.1.7) (2026-10-31)


### Features

* **logbook:** schema 5, keep ecu's summary, the lap pit roles and card.posted, with a backfill ([#27](https://github.com/those-who-run/logbook/issues/27)) ([8dc6bba](https://github.com/those-who-run/logbook/commit/8dc6bba4a80478c82eec979328b2987f9e7ba56f))
* **logbook:** schema 6, an open session's facts as its events say them ([#32](https://github.com/those-who-run/logbook/issues/32)) ([e0c8cf3](https://github.com/those-who-run/logbook/commit/e0c8cf31fe53ddb767d3b514f8f98cba02a42718))
* **logbook:** serve sessions, laps, same-track sessions and lap traces ([#30](https://github.com/those-who-run/logbook/issues/30)) ([64d1572](https://github.com/those-who-run/logbook/commit/64d1572e93706832b2f9a8faf9eec9c2c57eacc1))

## [0.1.6](https://github.com/those-who-run/logbook/compare/v0.1.5...v0.1.6) (2026-10-31)


### Bug Fixes

* **store:** contracts v0.2.7, and a ranked session's start fuel of 0 ([#28](https://github.com/those-who-run/logbook/issues/28)) ([97f8b53](https://github.com/those-who-run/logbook/commit/97f8b53256d01f953b3d8d0c65007ab4c22d383f))

## [0.1.5](https://github.com/those-who-run/logbook/compare/v0.1.4...v0.1.5) (2026-10-30)


### Features

* **logbook:** follow a session ecu continues after a restart ([#25](https://github.com/those-who-run/logbook/issues/25)) ([3afe1a8](https://github.com/those-who-run/logbook/commit/3afe1a8c03e918a3efe28feb99cf3262f82cf04c))

## [0.1.4](https://github.com/those-who-run/logbook/compare/v0.1.3...v0.1.4) (2026-10-28)


### Features

* **logbook:** store the session fields ecu sends, on contracts v0.1.5 ([#20](https://github.com/those-who-run/logbook/issues/20)) ([4589033](https://github.com/those-who-run/logbook/commit/4589033ef10247e576b6f4d63f36a6d3b02491a4))

## [0.1.3](https://github.com/those-who-run/logbook/compare/v0.1.2...v0.1.3) (2026-10-27)


### Bug Fixes

* **logbook:** review fixes for the subscriber ([#15](https://github.com/those-who-run/logbook/issues/15)) ([1dfcfa4](https://github.com/those-who-run/logbook/commit/1dfcfa4ab221535e32d68cecacee3dfcb74193b7))

## [0.1.2](https://github.com/those-who-run/logbook/compare/v0.1.1...v0.1.2) (2026-10-27)


### Features

* **logbook:** subscribe to frames and events, write laps, ack after commit ([#9](https://github.com/those-who-run/logbook/issues/9)) ([6a002f5](https://github.com/those-who-run/logbook/commit/6a002f5bdcd34970f78f2e4542ea5d01a9d48ac7))


### Bug Fixes

* **service:** the shutdown tests call NewServer ([#12](https://github.com/those-who-run/logbook/issues/12)) ([a784b74](https://github.com/those-who-run/logbook/commit/a784b7444502bd2eb5634edc9ce459f4bd28262e))

## [0.1.1](https://github.com/those-who-run/logbook/compare/v0.1.0...v0.1.1) (2026-10-26)


### Bug Fixes

* **service:** check the error the server returns after shutdown ([#7](https://github.com/those-who-run/logbook/issues/7)) ([222b2dd](https://github.com/those-who-run/logbook/commit/222b2dd90650f171a23aef6ed81d999ebb38b191))

## 0.1.0 (2026-10-26)


### Features

* write each lap's Parquet file and its index rows ([#4](https://github.com/those-who-run/logbook/issues/4)) ([d85968b](https://github.com/those-who-run/logbook/commit/d85968bab365cbac38f1489a176c52c5e7a7097e))
