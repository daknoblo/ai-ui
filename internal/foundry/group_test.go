package foundry

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestGroupDiscoveryRetainsAccountsAndSurfacesReaderFailure(t *testing.T) {
	sibling := testResourceID + "-poland"
	path := testResourceID[:strings.LastIndex(testResourceID, "/")]
	failListing, failSibling := false, false
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case path:
			if failListing {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			writeJSON(t, w, map[string]any{"value": []any{
				map[string]string{"id": testResourceID, "kind": "AIServices"},
				map[string]string{"id": sibling, "kind": "OpenAI"},
			}})
		case testResourceID, sibling:
			if failSibling && r.URL.Path == sibling {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			writeJSON(t, w, map[string]any{"id": r.URL.Path, "location": "polandcentral",
				"properties": map[string]any{"endpoint": "https://canonical.openai.azure.com/"}})
		case testResourceID + "/deployments", sibling + "/deployments":
			d := deploymentJSON("shared", "gpt-4o")
			d["id"] = strings.TrimSuffix(r.URL.Path, "/deployments") + "/deployments/shared"
			writeJSON(t, w, map[string]any{"value": []any{d}})
		default:
			t.Errorf("unexpected ARM path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	first, err := client.RefreshGroup(t.Context(), "", Snapshot{})
	if err != nil || len(first.Accounts) != 1 || first.Accounts[0].Location != "polandcentral" {
		t.Fatalf("group discovery failed: %+v, %v", first, err)
	}
	if first.Key(first.Deployments[0]) == first.Accounts[0].Key(first.Accounts[0].Deployments[0]) {
		t.Fatal("same-name deployments collided")
	}
	failSibling = true
	second, err := client.RefreshGroup(t.Context(), "", first)
	if err != nil || len(second.Accounts) != 1 || len(second.DiscoveryErrors) != 1 {
		t.Fatalf("failed resource was erased or hidden: %+v, %v", second, err)
	}
	failListing = true
	third, err := client.RefreshGroup(t.Context(), "", second)
	if err != nil || third.Endpoint == "" || len(third.Accounts) != 1 ||
		len(third.DiscoveryErrors) != 1 || !strings.Contains(third.DiscoveryErrors[0], "Reader") {
		t.Fatalf("group Reader failure disabled healthy accounts: %+v, %v", third, err)
	}
}

func TestGroupDiscoveryRejectsUntrustedNextLinkAndResource(t *testing.T) {
	for _, next := range []string{
		"https://attacker.example/accounts?api-version=2024-10-01",
		"/subscriptions/other/resourceGroups/elsewhere/providers/Microsoft.CognitiveServices/accounts",
	} {
		t.Run(next, func(t *testing.T) {
			client, _ := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, map[string]any{"value": []any{}, "nextLink": next})
			})
			if _, err := client.groupAccounts(t.Context()); err == nil {
				t.Fatal("untrusted nextLink was accepted")
			}
		})
	}
	client, _ := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{"value": []any{map[string]string{
			"id": strings.Replace(testResourceID, "/resourceGroups/", "/resourceGroups/other-", 1), "kind": "AIServices",
		}}})
	})
	if _, err := client.groupAccounts(t.Context()); err == nil {
		t.Fatal("out-of-group resource accepted")
	}
}

func TestGroupDeadlinePreservesHealthyRootButCallerCancellationAborts(t *testing.T) {
	for _, cancelCaller := range []bool{false, true} {
		t.Run(map[bool]string{false: "deadline", true: "cancellation"}[cancelCaller], func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case testResourceID:
					writeJSON(t, w, map[string]any{"id": testResourceID,
						"properties": map[string]any{"endpoint": "https://canonical.openai.azure.com/"}})
				case testResourceID + "/deployments":
					writeJSON(t, w, map[string]any{"value": []any{deploymentJSON("chat", "gpt-4o")}})
				default:
					if cancelCaller {
						cancel()
					}
					<-r.Context().Done()
				}
			})
			client.timeout = 250 * time.Millisecond
			snapshot, err := client.RefreshGroup(ctx, "", Snapshot{})
			if cancelCaller {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("caller cancellation was ignored: %v", err)
				}
			} else if err != nil || snapshot.Endpoint == "" || len(snapshot.Deployments) != 1 || len(snapshot.DiscoveryErrors) == 0 {
				t.Fatalf("listing timeout discarded healthy configured account: %+v, %v", snapshot, err)
			}
		})
	}
}
