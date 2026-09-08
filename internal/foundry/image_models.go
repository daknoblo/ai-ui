package foundry

import (
	"context"
	"errors"
	"sort"
)

// ImageModels reads provider model IDs separately from ARM deployment aliases.
// Listing a model is not proof that an image generation request will succeed.
func (c *Client) ImageModels(ctx context.Context, endpoint string) ([]Deployment, error) {
	if c == nil || c.credential == nil || ctx == nil {
		return nil, errors.New("image model discovery requires an initialized identity and context")
	}
	base, err := NormalizeEndpoint(endpoint)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	var response struct {
		Data *[]struct {
			ID string `json:"id"`
		} `json:"data"`
		HasMore  bool   `json:"has_more"`
		NextLink string `json:"nextLink"`
	}
	budget := int64(maxCatalogBytes)
	if err := c.getJSONWithScope(ctx, base+"/models?api-version=preview",
		"image model catalog", inferenceScope, &response, &budget); err != nil {
		return nil, err
	}
	if response.Data == nil || response.HasMore || response.NextLink != "" {
		return nil, errors.New("image model catalog response is missing or incomplete")
	}
	if len(*response.Data) > maxDeployments {
		return nil, errors.New("image model catalog exceeds the model limit")
	}
	ids := make(map[string]bool, len(*response.Data))
	for _, model := range *response.Data {
		ids[model.ID] = true
	}
	var models []Deployment
	for id := range ids {
		name := canonicalModel(id)
		profile, known := modelProfiles[name]
		if !known || profile.format != "openai" || profile.operations&imagesBit == 0 || !validSegment(id) {
			continue
		}
		// Prefer the provider alias when both it and its dated variants exist.
		if id != name && ids[name] {
			continue
		}
		version := ""
		if id != name && len(id) > len(name)+1 {
			version = id[len(name)+1:]
		}
		models = append(models, Deployment{
			Name: id, ModelName: name, ModelFormat: "OpenAI", ModelVersion: version,
			Source: ModelsAPISource,
		})
	}
	sort.Slice(models, func(i, j int) bool { return models[i].Name < models[j].Name })
	return models, nil
}
