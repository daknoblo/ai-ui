// Package rag implements chunking, ingestion and retrieval (brute-force cosine).
package rag

import "strings"

// ChunkText splits a text into overlapping sections. maxRunes is the target size
// per chunk, overlap the size of the overlapping region. Paragraph boundaries
// are preferred as split points.
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

	// Split into paragraphs and reassemble them into chunks.
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

		// Hard-split very long paragraphs.
		if pRunes > maxRunes {
			flush()
			chunks = append(chunks, splitLong(p, maxRunes, overlap)...)
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

// splitLong divides an over-long paragraph into overlapping pieces.
func splitLong(s string, maxRunes, overlap int) []string {
	runes := []rune(s)
	var out []string
	step := maxRunes - overlap
	if step <= 0 {
		step = maxRunes
	}
	for start := 0; start < len(runes); start += step {
		end := min(start+maxRunes, len(runes))
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
