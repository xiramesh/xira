package routing

import "testing"

// #151：session dimensions 硬编码 [chat]，配置里的 dimensions 被忽略。

func TestNormalizeSessionPolicyUsesDefaultDimensions(t *testing.T) {
	policy := NormalizeSessionPolicy(SessionPolicy{})

	if len(policy.Dimensions) != 1 || policy.Dimensions[0] != "chat" {
		t.Fatalf("dimensions = %+v, want [chat]", policy.Dimensions)
	}
	if policy.IdentityLinks != nil {
		t.Fatalf("identity links = %+v, want nil", policy.IdentityLinks)
	}
}

func TestNormalizeSessionPolicyIgnoresConfiguredDimensions(t *testing.T) {
	// 配置里写了 sender，但 NormalizeSessionPolicy 应该忽略，始终用 [chat]。
	policy := NormalizeSessionPolicy(SessionPolicy{
		Dimensions: []string{"channel", "sender", "topic"},
	})
	if len(policy.Dimensions) != 1 || policy.Dimensions[0] != "chat" {
		t.Fatalf("dimensions = %+v, want [chat] (configured dimensions should be ignored)", policy.Dimensions)
	}
}
