package ops

import (
	"slices"
	"testing"
)

// TestSplitAccepted: only a finding written out word for word passes; another count of the same finding stops.
func TestSplitAccepted(t *testing.T) {
	acc := []string{"237 imported statements lost their source id to earlier native writes"}
	stop, passed := SplitAccepted([]string{acc[0], "projector lag 3"}, acc)
	if !slices.Equal(stop, []string{"projector lag 3"}) || !slices.Equal(passed, acc) {
		t.Fatalf("stop %q passed %q", stop, passed)
	}
	if stop, _ := SplitAccepted([]string{"238 imported statements lost their source id to earlier native writes"}, acc); len(stop) != 1 {
		t.Fatalf("another count passed: %q", stop)
	}
	if stop, passed := SplitAccepted([]string{"x"}, nil); len(stop) != 1 || passed != nil {
		t.Fatalf("no accepted list: %q %q", stop, passed)
	}
}

// TestStageConfigAccepts: the pilot's install.yaml carries the finding of #275, the production one none.
func TestStageConfigAccepts(t *testing.T) {
	stage, err := LoadConfig(installConfig(t, "install.stage.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(stage.AcceptedFindings) != 1 {
		t.Fatalf("stage accepted_findings: %q", stage.AcceptedFindings)
	}
	prod, err := LoadConfig(installConfig(t, "install.prod.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(prod.AcceptedFindings) != 0 {
		t.Fatalf("prod accepts findings: %q", prod.AcceptedFindings)
	}
}
