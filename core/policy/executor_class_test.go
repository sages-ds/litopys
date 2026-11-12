package policy

import "testing"

// TestExecutorClass: the class of the profile, every class for a project a person opened to it or for all.
func TestExecutorClass(t *testing.T) {
	cases := []struct {
		profile, project, want string
	}{
		{``, "x", ""},
		{`{"max_data_class":"public"}`, "x", "public"},
		{`{"max_data_class":"public","access":{"mode":"preset"}}`, "x", "public"},
		{`{"max_data_class":"public","access":{"mode":"projects","projects":["kitchen"]}}`, "kitchen", "local"},
		{`{"max_data_class":"public","access":{"mode":"projects","projects":["kitchen"]}}`, "bank", "public"},
		{`{"max_data_class":"internal","access":{"mode":"all"}}`, "bank", "local"},
		{`not json`, "x", "public"},
	}
	for _, c := range cases {
		if got := ExecutorClass([]byte(c.profile), c.project); got != c.want {
			t.Errorf("ExecutorClass(%s, %s) = %q, want %q", c.profile, c.project, got, c.want)
		}
	}
}
