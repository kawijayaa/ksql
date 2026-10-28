# evaluate plugins

Besides `bag_unpack` (see bag-unpack.md), `evaluate` supports:

- `narrow()`: one `(Row, Column, Value)` row per input cell, with the
  zero-based input row number and the cell as text.
- `pivot(Column [, Aggregation] [, GroupColumns...]) [: OutputSchema]`: one
  column per distinct value of `Column`, aggregating its rows with the
  conditional form of `count()` (the default), `sum`, `min`, `max`, `avg`,
  `dcount`, `take_any`, `stdev`, `variance`, `make_list`, `make_set` or
  `make_bag`, grouped by the given columns or every other column. Values are
  discovered with the application's `ValuesResolver` (at most 256), or come
  from the output schema's columns after the group columns.
- `ipv4_lookup(LookupTable, SourceKey, LookupKey [, ExtraKeys...] [, return_unmatched])`
  and `ipv6_lookup(...)`: a lookup join matching addresses to the lookup
  table's addresses or prefixes through the application's `ipv4_is_in_range`
  / `ipv6_is_in_range`, plus equal extra keys; `return_unmatched = true`
  keeps unmatched rows with null lookup columns.

- `geo_polygon_lookup(LookupTable, PolygonKey, Longitude, Latitude [, radius] [, return_unmatched] [, lookup_area_radius] [, return_lookup_key])`:
  a lookup join on the application's `geo_point_in_polygon`, or on
  `geo_distance_point_to_polygon` within `radius` meters.
- `geo_line_lookup(LookupTable, LineKey, Longitude, Latitude, Radius [, ...])`:
  a lookup join on `geo_distance_point_to_line` within `Radius` meters.
  For both, options may be positional or named; `lookup_area_radius` is a
  performance hint and is ignored, and the key column is dropped unless
  `return_lookup_key = true`.

Other plugins report `KQLL0391`.
