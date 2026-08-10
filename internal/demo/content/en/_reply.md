**Three steps for a safe self-hosted setup**

1. **Keep it off the open internet.** Put the container behind a reverse proxy
   with authentication, or expose it inside a VPN only — the app has no user
   accounts of its own.
2. **Pass secrets as environment variables.** `AZURE_API_KEY` and friends are
   read from the environment and never written to `config.json`, so a leaked
   data volume does not leak the key.
3. **Own the data path.** Mount a named volume at `/appdata`, back it up, and
   keep the health check on `/healthz` so a wedged container is restarted.

Everything else — CSP headers, parameterized SQL, sanitized Markdown, a
non-root distroless image — is already in place.
