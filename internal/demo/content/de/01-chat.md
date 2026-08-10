key: chat
title: Wie Retrieval-Augmented Generation funktioniert
model: gpt-5.5
---
@user
Erkläre, wie Retrieval-Augmented Generation funktioniert und wann es die
bessere Wahl gegenüber einem Fine-Tuning ist.

@assistant
Retrieval-Augmented Generation (RAG) lässt das Modell unverändert und reicht
ihm stattdessen zur Laufzeit genau die *passenden* Ausschnitte aus den eigenen
Dokumenten.

**Die Pipeline**

1. **Chunking** – jedes hochgeladene Dokument wird in überlappende Abschnitte
   von rund 800 Zeichen zerlegt.
2. **Embedding** – jeder Chunk wird vom Embedding-Deployment in einen Vektor
   übersetzt und neben dem Chat in SQLite abgelegt.
3. **Retrieval** – die Frage wird ebenfalls eingebettet; es gewinnen die Chunks
   mit der höchsten Kosinus-Ähnlichkeit.
4. **Augmentation** – die besten Treffer werden dem Prompt als Kontext
   vorangestellt.
5. **Generation** – das Chat-Modell antwortet ausschließlich auf dieser Basis.

Die Bewertung ist klein genug, um in der Anwendung zu bleiben:

```go
func cosine(a, b []float32) float32 {
    var dot, na, nb float32
    for i := range a {
        dot += a[i] * b[i]
        na += a[i] * a[i]
        nb += b[i] * b[i]
    }
    return dot / (sqrt(na) * sqrt(nb))
}
```

**RAG oder Fine-Tuning?**

| Frage                              | RAG                    | Fine-Tuning          |
| ---------------------------------- | ---------------------- | -------------------- |
| Inhalte ändern sich täglich        | ✅ nur neu indexieren  | ❌ neu trainieren    |
| Antworten brauchen eine Quelle     | ✅ Chunk ist bekannt   | ❌ intransparent     |
| Stil oder Ausgabeformat zählt      | ➖ nur über den Prompt | ✅ gelernt           |
| Kosten einer Änderung              | Sekunden               | Stunden              |

Kurz: RAG für **Wissen**, Fine-Tuning für **Verhalten**. Für einen
selbst gehosteten Assistenten wie diesen deckt RAG praktisch alles ab – nötig
ist nur das `AZURE_EMBEDDING_DEPLOYMENT`.
