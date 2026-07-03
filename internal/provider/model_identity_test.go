package provider

import "testing"

func TestModelIdentitySeparatesDisplayCanonicalAndUpstream(t *testing.T) {
	identity := NewModelIdentity("z-ai/glm-5.2", "usecpa")

	if identity.Display != "USECPA - glm-5.2" {
		t.Fatalf("display = %q, want USECPA - glm-5.2", identity.Display)
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

func TestDisplayNameModelSuffixAcceptsTaggedAndUntaggedDisplayNames(t *testing.T) {
	for _, input := range []string{"USECPA - glm-5.2", "USECPA - glm-5.2:latest"} {
		if got := DisplayNameModelSuffix(input); got != "glm-5.2" {
			t.Fatalf("%s suffix = %q, want glm-5.2", input, got)
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
