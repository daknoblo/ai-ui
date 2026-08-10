key: websearch
title: Health-Checks für Distroless-Container
model: gpt-5.5
---
@user
Was ist aktuell die Empfehlung für Docker-Health-Checks in Distroless-Images
hinter Traefik?

@assistant
Distroless-Images enthalten weder eine Shell noch `curl`/`wget` – das klassische
`CMD curl -f http://localhost:8080/healthz` kann also nicht funktionieren.
Gebräuchlich sind heute zwei Wege:

- **Selbstprüfendes Binary** *(empfohlen)* — die Anwendung versteht einen
  Schalter wie `-healthcheck`, ruft den eigenen Endpunkt auf und endet mit `0`
  oder `1`. Das Dockerfile nutzt dann die Exec-Form:
  `HEALTHCHECK CMD ["/app/ai-ui", "-healthcheck"]`.
- **Externe Prüfung** — Traefik oder der Orchestrator fragt `/healthz` über das
  Netzwerk ab. Ein laufender, aber blockierter Container fällt so nicht auf.

Der Health-Endpunkt selbst sollte lokal bleiben: keine Datenbankabfrage, kein
Aufruf einer fremden API — sonst startet eine langsame Abhängigkeit einen
gesunden Container neu.

**Quellen**

- [Distroless FAQ — keine Shell im Runtime-Image](https://github.com/GoogleContainerTools/distroless)
- [Docker-Doku — HEALTHCHECK-Anweisung](https://docs.docker.com/reference/dockerfile/)
- [Traefik — Referenz zu Health-Checks](https://doc.traefik.io/traefik/)
