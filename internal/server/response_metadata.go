package server

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"strings"
)

// Stored snapshots contain a rendered model label. Extract text only; never
// trust persisted HTML as template.HTML when reopening a conversation.
func responseMetadata(snapshot string) (model, usage string, err error) {
	if snapshot == "" {
		return "", "", nil
	}
	var events map[string]string
	if err := json.Unmarshal([]byte(snapshot), &events); err != nil {
		return "", "", fmt.Errorf("decode response metadata: %w", err)
	}
	usage = events["usage"]
	if strings.TrimSpace(events["model"]) == "" {
		return "", usage, nil
	}
	var label struct {
		XMLName xml.Name `xml:"span"`
		Text    string   `xml:",chardata"`
	}
	if err := xml.Unmarshal([]byte(events["model"]), &label); err != nil {
		return "", usage, fmt.Errorf("decode response model label: %w", err)
	}
	return strings.TrimSpace(label.Text), usage, nil
}
