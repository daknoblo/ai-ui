package main

import (
	"errors"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/daknoblo/ai-ui/internal/documentai"
)

func documentAnalysisFromEnv(lookup func(string) string) (*documentai.Client, error) {
	endpoint := strings.TrimSpace(lookup("AZURE_DOCUMENT_ENDPOINT"))
	key := lookup("AZURE_DOCUMENT_API_KEY")
	if endpoint == "" {
		if key != "" {
			return nil, errors.New("AZURE_DOCUMENT_API_KEY requires AZURE_DOCUMENT_ENDPOINT")
		}
		return nil, nil
	}
	var credential azcore.TokenCredential
	if key == "" {
		tenant, client, secret := lookup("AZURE_TENANT_ID"), lookup("AZURE_CLIENT_ID"), lookup("AZURE_CLIENT_SECRET")
		if tenant == "" || client == "" || secret == "" {
			return nil, errors.New("AZURE_DOCUMENT_ENDPOINT requires AZURE_DOCUMENT_API_KEY or AZURE_TENANT_ID, AZURE_CLIENT_ID and AZURE_CLIENT_SECRET")
		}
		var err error
		credential, err = azidentity.NewClientSecretCredential(tenant, client, secret, nil)
		if err != nil {
			return nil, errors.New("document analysis Azure identity configuration is invalid")
		}
	}
	return documentai.New(endpoint, key, credential)
}
