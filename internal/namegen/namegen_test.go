package namegen

import "testing"

func TestNewProducesValidSubdomains(t *testing.T) {
	seen := map[string]int{}
	for range 500 {
		name := New()
		if !ValidSubdomain(name) {
			t.Fatalf("generated %q, which is not a valid subdomain", name)
		}
		seen[name]++
	}
	// A generator that returns one name would pass the validity check above.
	if len(seen) < 400 {
		t.Errorf("only %d distinct names in 500 draws; entropy looks wrong", len(seen))
	}
}

func TestValidSubdomain(t *testing.T) {
	valid := []string{"a", "api", "api-x", "x1", "swift-otter-4f2", "0", "a-b-c"}
	invalid := []string{
		"", "-api", "api-", "API", "api_x", "api.x", "a b",
		"xn--punycode", "api--x", "café",
	}
	for _, s := range valid {
		if !ValidSubdomain(s) {
			t.Errorf("ValidSubdomain(%q) = false, want true", s)
		}
	}
	for _, s := range invalid {
		if ValidSubdomain(s) {
			t.Errorf("ValidSubdomain(%q) = true, want false", s)
		}
	}
}

func TestValidSubdomainLengthLimit(t *testing.T) {
	label := make([]byte, 63)
	for i := range label {
		label[i] = 'a'
	}
	if !ValidSubdomain(string(label)) {
		t.Error("a 63-character label should be valid")
	}
	if ValidSubdomain(string(append(label, 'a'))) {
		t.Error("a 64-character label should be rejected")
	}
}
