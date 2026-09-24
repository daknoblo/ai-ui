package foundry

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// SameGroup accepts only Cognitive Services accounts inside the configured
// subscription and resource group, never arbitrary IDs from ARM or disk.
func SameGroup(a, b string) bool {
	if ValidateResourceID(a) != nil || ValidateResourceID(b) != nil {
		return false
	}
	return strings.EqualFold(strings.Join(strings.Split(strings.Trim(a, "/"), "/")[:4], "/"),
		strings.Join(strings.Split(strings.Trim(b, "/"), "/")[:4], "/"))
}

// RefreshGroup retains a last-known account on partial failure. New accounts
// are inventory only: callers must explicitly enable each deployment.
func (c *Client) RefreshGroup(ctx context.Context, override string, previous Snapshot) (Snapshot, error) {
	caller := ctx
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	root, err := c.Refresh(ctx, override)
	if err != nil {
		if !strings.EqualFold(previous.ResourceID, c.resourceID) || previous.Endpoint == "" {
			return Snapshot{}, err
		}
		root = previous
		root.DiscoveryErrors = []string{fmt.Sprintf("%s: %v", c.resourceID, err)}
	}
	root.Accounts = nil
	known := make(map[string]Snapshot)
	for _, account := range previous.Accounts {
		if SameGroup(c.resourceID, account.ResourceID) {
			known[strings.ToLower(account.ResourceID)] = account
		}
	}
	ids, listErr := c.groupAccounts(ctx)
	if listErr != nil {
		root.DiscoveryErrors = append(root.DiscoveryErrors,
			fmt.Sprintf("resource-group discovery requires Reader on the resource group; configured accounts remain available: %v", listErr))
		for _, account := range known {
			root.Accounts = append(root.Accounts, account)
		}
	} else {
		for index, id := range ids {
			if strings.EqualFold(id, c.resourceID) {
				continue
			}
			client, cloneErr := c.WithResource(id)
			if cloneErr != nil {
				return Snapshot{}, cloneErr
			}
			account, refreshErr := client.Refresh(ctx, "")
			if refreshErr != nil {
				root.DiscoveryErrors = append(root.DiscoveryErrors, fmt.Sprintf("%s: %v", id, refreshErr))
				if cached, ok := known[strings.ToLower(id)]; ok {
					root.Accounts = append(root.Accounts, cached)
				}
				if ctx.Err() != nil {
					for _, pending := range ids[index+1:] {
						if cached, ok := known[strings.ToLower(pending)]; ok {
							root.Accounts = append(root.Accounts, cached)
						}
					}
					break
				}
				continue
			}
			root.Accounts = append(root.Accounts, account)
		}
	}
	if err := caller.Err(); err != nil {
		return Snapshot{}, err
	}
	if err := ctx.Err(); err != nil {
		root.DiscoveryErrors = append(root.DiscoveryErrors, fmt.Sprintf("resource-group discovery stopped at its time limit: %v", err))
	}
	sort.Slice(root.Accounts, func(i, j int) bool { return root.Accounts[i].ResourceID < root.Accounts[j].ResourceID })
	return root, nil
}

func (c *Client) groupAccounts(ctx context.Context) ([]string, error) {
	parts := strings.Split(strings.Trim(c.resourceID, "/"), "/")
	path := "/" + strings.Join(parts[:4], "/") + "/providers/Microsoft.CognitiveServices/accounts"
	next, err := c.resourceURL(path)
	if err != nil {
		return nil, err
	}
	budget := int64(maxCatalogBytes)
	seen, names := map[string]bool{}, map[string]bool{}
	var ids []string
	for page := 0; next != ""; page++ {
		if page >= maxPages || seen[next] {
			return nil, errors.New("azure account discovery exceeds the page limit or repeats a page")
		}
		seen[next] = true
		var response struct {
			Value *[]struct {
				ID   string `json:"id"`
				Kind string `json:"kind"`
			} `json:"value"`
			NextLink string `json:"nextLink"`
		}
		if err := c.getJSON(ctx, next, "resource-group account discovery", &response, &budget); err != nil {
			return nil, err
		}
		if response.Value == nil || len(names)+len(*response.Value) > 256 {
			return nil, errors.New("azure account inventory is missing or exceeds the 256 account limit")
		}
		for _, account := range *response.Value {
			if !SameGroup(c.resourceID, account.ID) || names[strings.ToLower(account.ID)] {
				return nil, errors.New("azure account discovery returned an out-of-scope or duplicate resource")
			}
			names[strings.ToLower(account.ID)] = true
			if strings.EqualFold(account.Kind, "AIServices") || strings.EqualFold(account.Kind, "OpenAI") {
				ids = append(ids, account.ID)
			}
		}
		if response.NextLink == "" {
			break
		}
		next, err = c.paginationPathURL(next, response.NextLink, path)
		if err != nil {
			return nil, err
		}
	}
	sort.Strings(ids)
	return ids, nil
}
