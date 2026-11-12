package ops

// SplitAccepted: the verify findings that still stop an update or a rollback, and those install.yaml accepts word
// for word (accepted_findings).
func SplitAccepted(findings, accepted []string) (stop, passed []string) {
	ok := map[string]bool{}
	for _, a := range accepted {
		ok[a] = true
	}
	for _, f := range findings {
		if ok[f] {
			passed = append(passed, f)
		} else {
			stop = append(stop, f)
		}
	}
	return stop, passed
}
