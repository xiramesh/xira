package routing

import "testing"

func TestNormalizeSessionPolicyUsesDefaultDimensions(t *testing.T) {
	policy := NormalizeSessionPolicy(SessionPolicy{})

	if len(policy.Dimensions) != 2 || policy.Dimensions[0] != "chat" || policy.Dimensions[1] != "sender" {
		t.Fatalf("dimensions = %+v, want default chat/sender dimensions", policy.Dimensions)
	}
	if policy.IdentityLinks != nil {
		t.Fatalf("identity links = %+v, want nil", policy.IdentityLinks)
	}
}

func TestNormalizeSessionPolicyFiltersAndDedupesDimensions(t *testing.T) {
	policy := NormalizeSessionPolicy(SessionPolicy{
		Dimensions: []string{" channel ", "sender", "sender", "account", "topic"},
	})

	want := []string{"channel", "sender", "topic"}
	if len(policy.Dimensions) != len(want) {
		t.Fatalf("dimensions = %+v", policy.Dimensions)
	}
	for i := range want {
		if policy.Dimensions[i] != want[i] {
			t.Fatalf("dimensions = %+v, want %+v", policy.Dimensions, want)
		}
	}
}
