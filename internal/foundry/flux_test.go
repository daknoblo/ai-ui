package foundry

import "testing"

func TestFluxEndpointComesFromSameResourceARMMetadata(t *testing.T) {
	const reference = "https://pictures.openai.azure.com/openai/v1"
	properties := accountProperties{Endpoint: "https://pictures.cognitiveservices.azure.com/",
		Endpoints: map[string]string{
			"Legacy":                  reference,
			"Foundry Model Inference": "https://pictures.services.ai.azure.com/models",
			"Unrelated":               "https://attacker.example/providers/blackforestlabs/v1/flux-2-pro",
		}}
	if got := selectFluxEndpoint(properties, reference); got != "https://pictures.services.ai.azure.com" {
		t.Fatalf("BFL endpoint selection = %s", got)
	}
	for _, endpoint := range []string{
		"https://other.services.ai.azure.com",
		"https://pictures.services.ai.azure.com.attacker.example",
		"https://pictures.services.ai.azure.com:8443",
		"http://pictures.services.ai.azure.com",
		"https://pictures.services.ai.azure.com/other",
		"https://pictures@attacker.example",
	} {
		if err := ValidateFluxEndpoint(reference, endpoint); err == nil {
			t.Fatalf("untrusted BFL endpoint accepted: %s", endpoint)
		}
	}
	delete(properties.Endpoints, "Foundry Model Inference")
	if got := selectFluxEndpoint(properties, reference); got != "" {
		t.Fatal("a BFL host was guessed instead of discovered")
	}
	properties.Endpoints["BFL"] = "https://pictures.api.cognitive.microsoft.com/"
	if got := selectFluxEndpoint(properties, reference); got != "https://pictures.api.cognitive.microsoft.com" {
		t.Fatalf("documented BFL host not supported: %s", got)
	}
	if _, err := FluxURL(reference, "flux-2-pro"); err == nil {
		t.Fatal("BFL was sent to an OpenAI-only surface")
	}
}
