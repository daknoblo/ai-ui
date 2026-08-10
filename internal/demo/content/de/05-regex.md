key: regex
title: Regex für ISO-8601-Daten
model: o4-mini
---
@user
Bitte einen regulären Ausdruck für ein ISO-8601-Datum mit optionaler Uhrzeit.

@assistant
```
^\d{4}-\d{2}-\d{2}(?:T\d{2}:\d{2}(?::\d{2})?(?:Z|[+-]\d{2}:\d{2})?)?$
```

Er akzeptiert sowohl `2026-08-10` als auch `2026-08-10T14:05:00+02:00`. Beachte:
geprüft wird die *Form*, nicht der Kalender — `2026-02-31` kommt damit durch.
Für eine echte Prüfung parsen: in Go erledigt `time.Parse(time.RFC3339, s)`
beides auf einmal.
