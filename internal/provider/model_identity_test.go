package provider

import "testing"

func TestModelIdentitySeparatesDisplayCanonicalAndUpstream(t *testing.T) {
	identity := NewModelIdentity("z-ai/glm-5.2", "usecpa")

	if identity.Display != "usecpa - glm-5.2" {
		t.Fatalf("display = %q, want usecpa - glm-5.2", identity.Display)
	}
	if identity.Qualified != "z-ai/glm-5.2@usecpa" {
		t.Fatalf("qualified = %q, want z-ai/glm-5.2@usecpa", identity.Qualified)
	}
	if identity.Upstream != "z-ai/glm-5.2" {
		t.Fatalf("upstream = %q, want z-ai/glm-5.2", identity.Upstream)
	}
	for _, want := range []string{"z-ai/glm-5.2", "z-ai/glm-5.2:latest", "z-ai/glm-5.2@usecpa", "z-ai/glm-5.2@usecpa:latest", "glm-5.2", "glm-5.2:latest"} {
		if !containsString(identity.Aliases, want) {
			t.Fatalf("alias %q missing in %#v", want, identity.Aliases)
		}
	}
}

func TestModelIdentityUsesProviderDisplayName(t *testing.T) {
	identity := NewModelIdentityWithDisplay("z-ai/glm-5.2", "usecpa", "UseCpa")
	if identity.Display != "UseCpa - glm-5.2" {
		t.Fatalf("display = %q, want UseCpa - glm-5.2", identity.Display)
	}
}

func TestDisplayNameModelSuffixAcceptsTaggedAndUntaggedDisplayNames(t *testing.T) {
	for _, input := range []string{"USECPA - glm-5.2", "USECPA - glm-5.2:latest"} {
		if got := DisplayNameModelSuffix(input); got != "glm-5.2" {
			t.Fatalf("%s suffix = %q, want glm-5.2", input, got)
		}
	}
}

func TestStripModelTagOnlyRemovesLatestCompatibilityTag(t *testing.T) {
	tests := map[string]string{
		"glm-5.2:latest":             "glm-5.2",
		"z-ai/glm-5.2:latest":        "z-ai/glm-5.2",
		"qwen3-coder:480b":           "qwen3-coder:480b",
		"mistral-large-3:675b":       "mistral-large-3:675b",
		"openrouter/model:free":      "openrouter/model:free",
		"z-ai/glm-5.2@usecpa:latest": "z-ai/glm-5.2@usecpa",
	}
	for input, want := range tests {
		if got := StripModelTag(input); got != want {
			t.Fatalf("%s stripped to %q, want %q", input, got, want)
		}
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
