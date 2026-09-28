package config

import (
	"net/http"
	"slices"
	"testing"

	"github.com/daknoblo/ai-ui/internal/foundry"
)

func TestImageModelReplicasAndCredentialBoundary(t *testing.T) {
	s, a, b := replicaStore(t, Overrides{})
	a.Endpoint = "https://primary.openai.azure.com/openai/v1"
	a.FluxEndpoint = "https://primary.services.ai.azure.com"
	pro := foundry.Deployment{Name: "pro-se", ModelName: "FLUX.2-pro", ModelFormat: "BlackForestLabs", ModelVersion: "1", ProvisioningState: "Succeeded"}
	flex := pro
	flex.Name, flex.ModelName = "flex-se", "FLUX.2-flex"
	a.Deployments = append(a.Deployments, pro, flex)
	replica := pro
	replica.Name = "pro-pl"
	b.Deployments = append(b.Deployments, replica)
	a.Accounts = []foundry.Snapshot{b}
	if err := s.SetCatalog(a); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(s.ImageModels(foundry.Images), "flux.2-pro") {
		t.Fatal("discovery activated a new model")
	}
	cfg := s.Get()
	cfg.EnabledDeployments = map[foundry.Operation][]string{
		foundry.Images:     {a.Key(pro), a.Key(flex), b.Key(replica)},
		foundry.ImageEdits: {b.Key(replica)},
	}
	if err := s.Save(cfg); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(s.ImageModels(foundry.Images), []string{"flux.2-flex", "flux.2-pro"}) {
		t.Fatal("replicas were not collapsed to canonical models")
	}
	for _, test := range []struct {
		selection string
		names     []string
	}{
		{"", []string{"pro-se", "flex-se", "pro-pl", "pro-se"}},
		{"flux.2-pro", []string{"pro-se", "pro-pl", "pro-se"}},
		{"pro-se", []string{"pro-se", "pro-pl"}},
		{"flux.2-flex", []string{"flex-se", "flex-se"}},
	} {
		for _, name := range test.names {
			bound, d, err := s.SelectImageRoute(foundry.Images, test.selection)
			if err != nil || d.Name != name {
				t.Fatalf("selection %q: %s, %v", test.selection, d.Name, err)
			}
			endpoint, err := foundry.FluxURL(bound.Get().ImageHost(), d.FluxPath())
			if err != nil {
				t.Fatal(err)
			}
			req, err := http.NewRequest(http.MethodPost, endpoint, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := bound.Authorize(req, foundry.Images, d.Name); err != nil {
				t.Fatal(err)
			}
			for _, wrong := range []string{
				"https://attacker.example/providers/blackforestlabs/v1/" + d.FluxPath() + "?api-version=preview",
				bound.Get().ImageHost() + "/images/generations?api-version=preview",
				endpoint + "&redirect=evil",
			} {
				req, err := http.NewRequest(http.MethodPost, wrong, nil)
				if err != nil {
					t.Fatal(err)
				}
				if err := bound.Authorize(req, foundry.Images, d.Name); err == nil || req.Header.Get("Authorization") != "" {
					t.Fatal("FLUX credentials escaped the exact provider operation")
				}
			}
		}
	}
	if err := s.ValidateImageSelection(foundry.ImageEdits, "flux.2-flex"); err == nil {
		t.Fatal("generation activation incorrectly enabled edits")
	}
	frozen, _, err := s.SelectImageRoute(foundry.Images, "flux.2-pro")
	if err != nil {
		t.Fatal(err)
	}
	cfg.EnabledDeployments[foundry.Images] = []string{a.Key(flex)}
	if err := s.Save(cfg); err != nil {
		t.Fatal(err)
	}
	for _, unavailable := range []string{"flux.2-pro", "pro-se", a.Key(pro), "unknown"} {
		if _, _, err := s.SelectImageRoute(foundry.Images, unavailable); err == nil {
			t.Fatalf("unavailable selection %s silently switched model", unavailable)
		}
	}
	if frozen.Get().ImageDeployment == "flex-se" {
		t.Fatal("a frozen image route changed with settings")
	}
}
