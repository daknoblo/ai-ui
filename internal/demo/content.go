package demo

import (
	"embed"
	"fmt"
	"path"
	"strconv"
	"strings"
)

// content holds the seeded conversations per language. Keeping them as Markdown
// files instead of Go string literals allows fenced code blocks and inline code
// in the demo answers. The "all:" prefix keeps the _reply.md files, which the
// embed pattern would otherwise skip.
//
//go:embed all:content
var content embed.FS

// imagePlaceholder in a message is replaced with the database ID of the image
// that was generated for that conversation.
const imagePlaceholder = "%IMAGE%"

// conversation is one demo chat including its attachments.
type conversation struct {
	Key      string // stable identifier used by the screenshot tooling
	Title    string
	Model    string
	Mode     string // storage.ChatModeChat or storage.ChatModeImage
	Docs     []attachment
	Image    string // prompt of the generated image; empty means none
	Upload   string // file name of an attached source image; empty means none
	Messages []exchange
}

// attachment is an uploaded demo document.
type attachment struct {
	Name   string
	MIME   string
	Chunks int
}

// exchange is a single message of a demo conversation.
type exchange struct {
	Role    string // "user" or "assistant"
	Content string
}

// conversations parses the demo conversations of a language in file name order.
// Files whose name starts with an underscore hold backend replies and are
// skipped here.
func conversations(lang string) ([]conversation, error) {
	dir := path.Join("content", lang)
	entries, err := content.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("demo content for %q: %w", lang, err)
	}
	out := make([]conversation, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), "_") {
			continue
		}
		raw, err := content.ReadFile(path.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		conv, err := parseConversation(e.Name(), string(raw))
		if err != nil {
			return nil, err
		}
		out = append(out, conv)
	}
	return out, nil
}

// reply returns the answer the stub backend streams for a live demo request.
func reply(lang string) string {
	raw, err := content.ReadFile(path.Join("content", lang, "_reply.md"))
	if err != nil {
		return "The demo backend has no canned answer for this language."
	}
	return strings.TrimSpace(string(raw))
}

// parseConversation reads the "key: value" header, the "---" separator and the
// message blocks introduced by "@user" and "@assistant".
func parseConversation(name, src string) (conversation, error) {
	var conv conversation
	head, body, ok := strings.Cut(src, "\n---\n")
	if !ok {
		return conv, fmt.Errorf("%s: missing --- separator", name)
	}

	for _, line := range strings.Split(head, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			return conv, fmt.Errorf("%s: invalid header line %q", name, line)
		}
		value = strings.TrimSpace(value)
		switch strings.TrimSpace(key) {
		case "key":
			conv.Key = value
		case "title":
			conv.Title = value
		case "model":
			conv.Model = value
		case "mode":
			conv.Mode = value
		case "image":
			conv.Image = value
		case "upload":
			conv.Upload = value
		case "docs":
			docs, err := parseDocs(value)
			if err != nil {
				return conv, fmt.Errorf("%s: %w", name, err)
			}
			conv.Docs = docs
		default:
			return conv, fmt.Errorf("%s: unknown header key %q", name, key)
		}
	}

	for _, line := range strings.Split(body, "\n") {
		switch strings.TrimSpace(line) {
		case "@user":
			conv.Messages = append(conv.Messages, exchange{Role: "user"})
			continue
		case "@assistant":
			conv.Messages = append(conv.Messages, exchange{Role: "assistant"})
			continue
		}
		if len(conv.Messages) == 0 {
			continue // text before the first role marker
		}
		last := &conv.Messages[len(conv.Messages)-1]
		last.Content += line + "\n"
	}
	for i := range conv.Messages {
		conv.Messages[i].Content = strings.TrimSpace(conv.Messages[i].Content)
	}

	if conv.Key == "" || conv.Title == "" || len(conv.Messages) == 0 {
		return conv, fmt.Errorf("%s: key, title and at least one message are required", name)
	}
	return conv, nil
}

// parseDocs reads a document list of the form "name.pdf:18, notes.md:6".
func parseDocs(value string) ([]attachment, error) {
	var out []attachment
	for _, entry := range strings.Split(value, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		name, count, ok := strings.Cut(entry, ":")
		if !ok {
			return nil, fmt.Errorf("invalid document %q, expected name:chunks", entry)
		}
		chunks, err := strconv.Atoi(strings.TrimSpace(count))
		if err != nil {
			return nil, fmt.Errorf("invalid chunk count in %q: %w", entry, err)
		}
		name = strings.TrimSpace(name)
		out = append(out, attachment{Name: name, MIME: mimeForName(name), Chunks: chunks})
	}
	return out, nil
}

// mimeForName maps the demo file names to the content types the uploader would
// have stored.
func mimeForName(name string) string {
	switch {
	case strings.HasSuffix(name, ".pdf"):
		return "application/pdf"
	case strings.HasSuffix(name, ".docx"):
		return "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	case strings.HasSuffix(name, ".md"), strings.HasSuffix(name, ".markdown"):
		return "text/markdown"
	default:
		return "text/plain"
	}
}
