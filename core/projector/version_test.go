package projector

import (
	"testing"

	"litopys.smart-gw.co.ua/core/textnorm"
)

// TestNormVersionPinned: statement_search is textnorm.Analyze output, so a new textnorm.Version must come with a
// new projector Version (or stored rows keep the old analysis and nothing rebuilds them). Bump both, then this pin.
func TestNormVersionPinned(t *testing.T) {
	const normAtVersion32 = "3"
	if textnorm.Version != normAtVersion32 || Version != "32" {
		t.Fatalf("textnorm.Version %q with projector Version %q: bump projector.Version and update this pin together",
			textnorm.Version, Version)
	}
}
