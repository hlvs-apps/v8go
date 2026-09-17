package v8go_test

import (
	"os"
	"strings"
	"testing"
)

// cgo compiles this package with -I. (the module root), and macOS filesystems
// are case-insensitive, so a root file named like a C++ standard header (a
// VERSION file is <version>) replaces that header for every consumer building
// on macOS. Linux CI cannot see it; this test can.
func TestNoRootFileShadowsStandardHeader(t *testing.T) {
	headers := map[string]bool{}
	for _, h := range strings.Fields(`algorithm any array atomic bitset chrono cmath cstddef cstdint cstdio cstdlib cstring
		deque exception filesystem format functional iterator limits list locale map memory mutex new numeric
		optional queue random ranges regex set shared_mutex span stack string thread tuple type_traits
		unordered_map utility variant vector version`) {
		headers[h] = true
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if headers[strings.ToLower(e.Name())] {
			t.Errorf("%s shadows the C++ <%s> header on case-insensitive filesystems", e.Name(), strings.ToLower(e.Name()))
		}
	}
}
