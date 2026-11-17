package main

import "testing"

func TestDocumentAnalysisEnvironment(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		env                    map[string]string
		wantEnabled, wantError bool
	}{
		{"disabled", nil, false, false},
		{"key without endpoint", map[string]string{"AZURE_DOCUMENT_API_KEY": "secret"}, false, true},
		{"missing auth", map[string]string{"AZURE_DOCUMENT_ENDPOINT": "https://docs.cognitiveservices.azure.com"}, false, true},
		{"key", map[string]string{"AZURE_DOCUMENT_ENDPOINT": "https://docs.cognitiveservices.azure.com", "AZURE_DOCUMENT_API_KEY": "secret"}, true, false},
		{"partial identity", map[string]string{"AZURE_DOCUMENT_ENDPOINT": "https://docs.cognitiveservices.azure.com", "AZURE_TENANT_ID": "tenant"}, false, true},
		{"invalid origin", map[string]string{"AZURE_DOCUMENT_ENDPOINT": "https://example.com", "AZURE_DOCUMENT_API_KEY": "secret"}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, err := documentAnalysisFromEnv(func(key string) string { return tc.env[key] })
			if (err != nil) != tc.wantError || (client != nil) != tc.wantEnabled {
				t.Fatalf("enabled=%v err=%v", client != nil, err)
			}
		})
	}
}
