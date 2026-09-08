package foundry

import (
	"strings"
	"testing"
)

func TestWithResourceSharesIdentityAndTransport(t *testing.T) {
	client := newClient(testResourceID, &fakeCredential{})
	imageResource := strings.Replace(testResourceID, "/accounts/test-account", "/accounts/image-account", 1)
	clone, err := client.WithResource(imageResource + "/")
	if err != nil {
		t.Fatal(err)
	}
	if clone == client || clone.resourceID != imageResource || clone.credential != client.credential ||
		clone.httpClient != client.httpClient || clone.armEndpoint != client.armEndpoint ||
		clone.timeout != client.timeout {
		t.Fatal("resource clone did not preserve the shared client state")
	}
	if _, err := client.WithResource(imageResource + "/deployments/image"); err == nil {
		t.Fatal("deployment resource ID was accepted as an account")
	}
	var nilClient *Client
	if _, err := nilClient.WithResource(imageResource); err == nil {
		t.Fatal("nil client was cloned")
	}
}
