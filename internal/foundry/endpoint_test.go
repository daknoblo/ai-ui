package foundry

import (
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
)

const testResourceID = "/subscriptions/11111111-2222-3333-4444-555555555555/resourceGroups/test-rg/providers/Microsoft.CognitiveServices/accounts/test-account"

func TestValidateResourceID(t *testing.T) {
	for _, raw := range []string{testResourceID, strings.ToUpper(testResourceID), testResourceID + "/"} {
		if err := ValidateResourceID(raw); err != nil {
			t.Errorf("valid resource ID rejected: %v", err)
		}
	}
	for _, raw := range []string{
		"", "test-account", "https://management.azure.com" + testResourceID,
		testResourceID + "/deployments/model", testResourceID + "?api-version=other",
		testResourceID + "#fragment", testResourceID + "//",
		strings.Replace(testResourceID, "test-rg", "..", 1),
		strings.Replace(testResourceID, "test-rg", "test%2Frg", 1),
		strings.Replace(testResourceID, "test-rg", "test\\rg", 1),
		strings.Replace(testResourceID, "test-rg", "test\nrg", 1),
		strings.Replace(testResourceID, "11111111-2222-3333-4444-555555555555", "not-a-subscription", 1),
		strings.Replace(testResourceID, "Microsoft.CognitiveServices", "Microsoft.Storage", 1),
		" " + testResourceID,
	} {
		if err := ValidateResourceID(raw); err == nil {
			t.Errorf("invalid resource ID accepted: %q", raw)
		}
	}
}

func TestNewRequiresExplicitIdentity(t *testing.T) {
	identity := Identity{
		ResourceID: testResourceID, TenantID: "11111111-2222-3333-4444-555555555555",
		ClientID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", ClientSecret: "offline-test-secret",
	}
	client, err := New(identity)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := client.credential.(*azidentity.ClientSecretCredential); !ok {
		t.Fatal("New did not create an explicit ClientSecretCredential")
	}
	if client.armEndpoint != "https://management.azure.com" || client.resourceID != testResourceID {
		t.Fatal("incorrect resource-scoped ARM configuration")
	}
	for _, missing := range []string{"resource", "tenant", "client", "secret"} {
		t.Run(missing, func(t *testing.T) {
			value := identity
			switch missing {
			case "resource":
				value.ResourceID = ""
			case "tenant":
				value.TenantID = ""
			case "client":
				value.ClientID = " "
			case "secret":
				value.ClientSecret = ""
			}
			if _, err := New(value); err == nil {
				t.Fatal("incomplete identity accepted")
			}
		})
	}
	identity.TenantID = "invalid/tenant"
	if _, err := New(identity); err == nil || strings.Contains(err.Error(), identity.ClientSecret) {
		t.Fatal("invalid SDK identity was not rejected safely")
	}
}

func TestNormalizeEndpoint(t *testing.T) {
	for _, test := range []struct {
		raw, want string
	}{
		{"https://resource.openai.azure.com/", "https://resource.openai.azure.com/openai/v1"},
		{"https://resource.services.ai.azure.com", "https://resource.services.ai.azure.com/openai/v1"},
		{"https://resource.privatelink.openai.azure.com/", "https://resource.privatelink.openai.azure.com/openai/v1"},
		{"https://RESOURCE.openai.azure.com//openai///v1//", "https://resource.openai.azure.com/openai/v1"},
		{"https://proxy.internal:8443/tenant/openai/v1/", "https://proxy.internal:8443/tenant/openai/v1"},
		{"https://resource.cognitiveservices.azure.com/openai/v1", "https://resource.cognitiveservices.azure.com/openai/v1"},
	} {
		t.Run(test.raw, func(t *testing.T) {
			got, err := NormalizeEndpoint(test.raw)
			if err != nil || got != test.want {
				t.Fatalf("NormalizeEndpoint() = %q, %v; want %q", got, err, test.want)
			}
			if !IsV1Endpoint(got) {
				t.Fatal("normalized endpoint not recognized as v1")
			}
		})
	}
	for _, raw := range []string{
		"", "https://resource.cognitiveservices.azure.com/", "https://proxy.internal/",
		"https://resource.openai.azure.com/openai/deployments",
		"https://resource.openai.azure.com.evil.invalid/",
		"https://resource.openai.azure.com/openai/v1?api-key=secret",
		"https://user:secret@resource.openai.azure.com/openai/v1",
		"https://resource.openai.azure.com/openai/v1#fragment",
		"https://resource.openai.azure.com/path/../openai/v1",
		"https://resource.openai.azure.com/%6fpenai/v1",
		"http://resource.openai.azure.com/openai/v1", "/openai/v1",
	} {
		if _, err := NormalizeEndpoint(raw); err == nil || strings.Contains(err.Error(), "api-key=secret") {
			t.Errorf("endpoint was not rejected safely: %q", raw)
		}
	}
	if !IsV1Endpoint("http://127.0.0.1:8080/openai/v1/") {
		t.Fatal("manual-mode HTTP v1 endpoint not recognized")
	}
	if IsV1Endpoint("https://resource.openai.azure.com/") {
		t.Fatal("a resource root is not an explicit v1 endpoint")
	}
}

func TestSelectEndpointMetadataAndOverrides(t *testing.T) {
	properties := accountProperties{
		Endpoint: "https://canonical.cognitiveservices.azure.com/",
		Endpoints: map[string]string{
			"Azure OpenAI Legacy API - Latest moniker": "https://canonical.openai.azure.com/",
			"Unrelated speech API":                     "https://region.tts.speech.microsoft.com/",
		},
	}
	got, err := selectEndpoint(properties, "")
	if err != nil || got != "https://canonical.openai.azure.com/openai/v1" {
		t.Fatalf("named endpoint selection: %q, %v", got, err)
	}
	for _, override := range []string{
		"https://canonical.openai.azure.com/",
		"https://canonical.services.ai.azure.com/openai/v1/",
		"https://canonical.privatelink.openai.azure.com/openai/v1",
		"https://canonical.cognitiveservices.azure.com/openai/v1",
	} {
		if _, err := selectEndpoint(properties, override); err != nil {
			t.Errorf("same-resource alias rejected: %v", err)
		}
	}
	for _, override := range []string{
		"https://other.openai.azure.com/openai/v1",
		"https://canonical.openai.azure.com.evil.invalid/openai/v1",
		"https://unadvertised.proxy.internal/openai/v1",
		"https://canonical.services.ai.azure.com:8443/openai/v1",
	} {
		if _, err := selectEndpoint(properties, override); err == nil {
			t.Errorf("unverified endpoint accepted: %s", override)
		}
	}
	private := accountProperties{Endpoint: "https://gateway.internal:8443/resource"}
	if endpoint, err := selectEndpoint(private, "https://gateway.internal:8443/resource/openai/v1"); err != nil ||
		endpoint != "https://gateway.internal:8443/resource/openai/v1" {
		t.Fatalf("advertised proxy override rejected: %q, %v", endpoint, err)
	}
	if _, err := selectEndpoint(private, "https://gateway.internal:8443/other-resource/openai/v1"); err == nil {
		t.Fatal("a proxy override changed the resource path")
	}
	advertisedV1 := accountProperties{Endpoint: "https://gateway.internal:8443/resource/openai/v1"}
	if _, err := selectEndpoint(advertisedV1, "https://gateway.internal:8443/other-resource/openai/v1"); err == nil {
		t.Fatal("a proxy override changed the advertised v1 path")
	}
	private.Endpoints = map[string]string{"v1": "https://gateway.internal:8443/advertised/openai/v1"}
	if _, err := selectEndpoint(private, private.Endpoints["v1"]); err != nil {
		t.Fatalf("another advertised proxy route was ignored: %v", err)
	}
	if _, err := selectEndpoint(accountProperties{CustomSubDomainName: "canonical"}, "https://canonical.services.ai.azure.com/"); err != nil {
		t.Fatalf("explicit endpoint verified by custom subdomain metadata rejected: %v", err)
	}
	for _, missing := range []accountProperties{
		{},
		{Endpoint: "https://canonical.cognitiveservices.azure.com/"},
		{Endpoints: map[string]string{"Azure OpenAI Legacy API - Latest moniker": "not a URL"}},
	} {
		if _, err := selectEndpoint(missing, ""); err == nil {
			t.Fatal("an endpoint was fabricated from unsuitable or missing metadata")
		}
	}
	ambiguous := accountProperties{Endpoints: map[string]string{
		"one": "https://one.services.ai.azure.com/openai/v1",
		"two": "https://two.services.ai.azure.com/openai/v1",
	}}
	if _, err := selectEndpoint(ambiguous, ""); err == nil {
		t.Fatal("ambiguous endpoint metadata accepted")
	}
}
