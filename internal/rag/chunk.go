// Package rag implementiert Chunking, Ingestion und Retrieval (Brute-Force-Cosine).
package rag

import "strings"

// ChunkText teilt einen Text in überlappende Abschnitte.
// maxRunes ist die Zielgröße pro Chunk, overlap der Überlappungsbereich.
// Es wird bevorzugt an Absatz-/Satzgrenzen getrennt.
func ChunkText(text string, maxRunes, overlap int) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	if maxRunes <= 0 {
		maxRunes = 1200
	}
	if overlap < 0 || overlap >= maxRunes {
		overlap = maxRunes / 5
	}

	// In Absätze aufteilen und zu Chunks zusammensetzen.
	paras := splitParagraphs(text)

	var chunks []string
	var cur strings.Builder
	curLen := 0

	flush := func() {
		if curLen > 0 {
			chunks = append(chunks, strings.TrimSpace(cur.String()))
			cur.Reset()
			curLen = 0
		}
	}

	for _, p := range paras {
		pRunes := len([]rune(p))

		// Sehr lange Absätze hart unterteilen.
		if pRunes > maxRunes {
			flush()
			for _, piece := range splitLong(p, maxRunes, overlap) {
				chunks = append(chunks, piece)
			}
			continue
		}

		if curLen+pRunes+1 > maxRunes {
			flush()
		}
		if curLen > 0 {
			cur.WriteString("\n\n")
			curLen++
		}
		cur.WriteString(p)
		curLen += pRunes
	}
	flush()

	return chunks
}

func splitParagraphs(text string) []string {
	raw := strings.Split(text, "\n\n")
	var out []string
	for _, p := range raw {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		out = []string{text}
	}
	return out
}

// splitLong unterteilt einen überlangen Absatz in überlappende Stücke.
func splitLong(s string, maxRunes, overlap int) []string {
	runes := []rune(s)
	var out []string
	step := maxRunes - overlap
	if step <= 0 {
		step = maxRunes
	}
	for start := 0; start < len(runes); start += step {
		end := start + maxRunes
		if end > len(runes) {
			end = len(runes)
		}
		piece := strings.TrimSpace(string(runes[start:end]))
		if piece != "" {
			out = append(out, piece)
		}
		if end == len(runes) {
			break
		}
	}
	return out
}
