key: regex
title: Regex for ISO 8601 dates
model: o4-mini
---
@user
A regex that matches an ISO 8601 date with an optional time, please.

@assistant
```
^\d{4}-\d{2}-\d{2}(?:T\d{2}:\d{2}(?::\d{2})?(?:Z|[+-]\d{2}:\d{2})?)?$
```

It accepts `2026-08-10` as well as `2026-08-10T14:05:00+02:00`. Note that it
validates the *shape*, not the calendar: `2026-02-31` still passes. For real
validation, parse it — in Go `time.Parse(time.RFC3339, s)` does both at once.
