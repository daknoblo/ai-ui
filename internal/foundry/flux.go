package foundry

import (
	"fmt"
	"net/url"
	"slices"
	"strings"
)

// FluxURL uses the inventoried BFL resource host, never a hostname guessed
// from a deployment alias. BFL is served at the resource root.
func FluxURL(endpoint, modelPath string) (string, error) {
	if modelPath != "flux-2-pro" && modelPath != "flux-2-flex" {
		return "", fmt.Errorf("unsupported FLUX model")
	}
	u, err := url.Parse(strings.TrimRight(endpoint, "/"))
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		(u.Scheme != "https" && u.Scheme != "http") {
		return "", fmt.Errorf("invalid FLUX resource endpoint")
	}
	if u.Path != "" && u.Path != "/openai/v1" && u.Path != "/openai" {
		return "", fmt.Errorf("FLUX requires a resource root or OpenAI resource endpoint")
	}
	if _, domain := azureResourceHost(u.Hostname()); domain == "openai" {
		return "", fmt.Errorf("FLUX requires an inventoried AI Services/BFL resource endpoint; refresh the inventory")
	}
	u.Path = "/providers/blackforestlabs/v1/" + modelPath
	u.RawPath = ""
	u.RawQuery = "api-version=preview"
	return u.String(), nil
}

// ValidateFluxEndpoint allows only the same resource's documented BFL hosts.
// The alternate root must additionally come from the ARM endpoint metadata.
func ValidateFluxEndpoint(reference, candidate string) error {
	if candidate == "" {
		return nil
	}
	base, err := parseEndpoint(reference, false)
	if err != nil {
		return err
	}
	target, err := parseEndpoint(candidate, false)
	if err != nil || target.Path != "" {
		return fmt.Errorf("invalid BFL resource endpoint")
	}
	baseName, _ := azureResourceHost(base.Hostname())
	targetName, domain := azureResourceHost(target.Hostname())
	if baseName == "" || baseName != targetName || (domain != "services" && domain != "bfl") ||
		(target.Port() != "" && target.Port() != "443") || (base.Port() != "" && base.Port() != "443") {
		return fmt.Errorf("BFL endpoint is outside the discovered resource")
	}
	return nil
}

func selectFluxEndpoint(properties accountProperties, reference string) string {
	candidates := []string{properties.Endpoint}
	for _, endpoint := range properties.Endpoints {
		candidates = append(candidates, endpoint)
	}
	slices.Sort(candidates)
	for _, raw := range candidates {
		u, err := parseEndpoint(raw, false)
		if err != nil {
			continue
		}
		u.Path = ""
		root := u.String()
		if ValidateFluxEndpoint(reference, root) == nil {
			return root
		}
	}
	return ""
}
