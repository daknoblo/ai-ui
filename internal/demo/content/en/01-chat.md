key: chat
title: How retrieval-augmented generation works
model: gpt-5.5
---
@user
Explain how retrieval-augmented generation works and when it is the better
choice compared to fine-tuning a model.

@assistant
Retrieval-augmented generation (RAG) keeps the model unchanged and instead
feeds it the *right* excerpts of your own documents at request time.

**The pipeline**

1. **Chunking** – every uploaded document is split into overlapping passages of
   roughly 800 characters.
2. **Embedding** – each chunk is turned into a vector by the embedding
   deployment and stored next to the chat in SQLite.
3. **Retrieval** – the question is embedded as well; the chunks with the highest
   cosine similarity win.
4. **Augmentation** – the top matches are prepended to the prompt as context.
5. **Generation** – the chat model answers using that context only.

The scoring step is small enough to keep in the application:

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

**RAG or fine-tuning?**

| Question                          | RAG                    | Fine-tuning          |
| --------------------------------- | ---------------------- | -------------------- |
| Content changes daily             | ✅ re-index only       | ❌ retrain           |
| Answers must cite a source        | ✅ chunk is known      | ❌ opaque            |
| Style or output format matters    | ➖ prompt only         | ✅ learned           |
| Cost of a change                  | seconds                | hours                |

In short: use RAG for **knowledge**, fine-tuning for **behaviour**. For a
self-hosted assistant like this one, RAG covers almost everything — the
`AZURE_EMBEDDING_DEPLOYMENT` is all that is needed.
