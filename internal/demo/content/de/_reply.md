**Drei Schritte zu einem sicheren Self-Hosting**

1. **Nicht offen ins Internet stellen.** Der Container gehört hinter einen
   Reverse Proxy mit Authentifizierung oder in ein VPN — die App bringt keine
   eigene Benutzerverwaltung mit.
2. **Secrets als Umgebungsvariablen übergeben.** `AZURE_API_KEY` und die
   übrigen Schlüssel werden nur aus der Umgebung gelesen und nie in
   `config.json` geschrieben; ein kopiertes Datenvolume verrät sie also nicht.
3. **Den Datenpfad im Griff behalten.** Ein Named Volume auf `/appdata` mounten,
   sichern und den Health-Check auf `/healthz` aktiv lassen, damit ein
   hängender Container neu startet.

Alles Weitere — CSP-Header, parametrisiertes SQL, bereinigtes Markdown und ein
Distroless-Image ohne Root — ist bereits eingebaut.
