package auth

import "testing"

func TestAuthenticateAndDeploymentPolicy(t *testing.T) {
	authenticator := New(NewClient("device-a", "secret-a", []string{"terra"}))
	client, err := authenticator.AuthenticateAuthorization("Bearer secret-a")
	if err != nil {
		t.Fatal(err)
	}
	if client.ID != "device-a" || !client.Allows("terra") || client.Allows("sol") {
		t.Fatalf("unexpected client policy: %#v", client)
	}
}

func TestAuthenticateRejectsInvalidToken(t *testing.T) {
	authenticator := New(NewClient("device-a", "secret-a", []string{"terra"}))
	for _, header := range []string{"", "Basic secret-a", "Bearer wrong"} {
		if _, err := authenticator.AuthenticateAuthorization(header); err == nil {
			t.Fatalf("header %q unexpectedly authenticated", header)
		}
	}
}
