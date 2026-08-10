key: websearch
title: Health checks for distroless containers
model: gpt-5.5
---
@user
What is the current recommendation for Docker health checks in distroless
images behind Traefik?

@assistant
Distroless images ship neither a shell nor `curl`/`wget`, so the classic
`CMD curl -f http://localhost:8080/healthz` cannot work. Two approaches are in
use today:

- **Self-probing binary** *(recommended)* — the application understands a flag
  such as `-healthcheck`, calls its own endpoint and exits with `0` or `1`. The
  Dockerfile then uses the exec form:
  `HEALTHCHECK CMD ["/app/ai-ui", "-healthcheck"]`.
- **External probe** — Traefik or the orchestrator queries `/healthz` over the
  network. This does not notice a container that is up but wedged.

The health endpoint itself should stay local: no database round-trip, no call
to an upstream API, so a slow dependency cannot restart a healthy container.

**Sources**

- [Distroless FAQ — no shell in the runtime image](https://github.com/GoogleContainerTools/distroless)
- [Docker docs — HEALTHCHECK instruction](https://docs.docker.com/reference/dockerfile/)
- [Traefik — health check reference](https://doc.traefik.io/traefik/)
