package ops

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// ComposeSecrets maps the placeholders of the core's compose template to the environment variables that hold their
// values (secrets only from the environment, never in arguments or files).
var ComposeSecrets = map[string]string{
	"__MIGRATOR_PASSWORD__": "LITOPYS_MIGRATOR_PASSWORD",
	"__APP_PASSWORD__":      "LITOPYS_APP_PASSWORD",
	"__WORKER_PASSWORD__":   "LITOPYS_WORKER_PASSWORD",
	"__DIRECTORY_SECRET__":  "LITOPYS_DIRECTORY_SECRET",
}

// ComposeOptional: placeholders filled from these variables when set and empty otherwise — switches that are off
// by default (the channel canary, evals/canary).
var ComposeOptional = map[string]string{
	"__CANARY__":        "LITOPYS_CANARY",
	"__CANARY_ACTORS__": "LITOPYS_CANARY_ACTORS",
	// the app key of executors' provider keys and the runner sidecar's secret: empty — keys are not
	// stored by the app and /v1/runner/* is off
	"__SECRET_KEY__":    "LITOPYS_SECRET_KEY",
	"__RUNNER_SECRET__": "LITOPYS_RUNNER_SECRET",
}

var placeholder = regexp.MustCompile(`__[A-Z_]+__`)

// RenderCompose fills the core's compose template: __IMAGE__ with the release's image (by digest), each secret from
// its environment variable as a YAML single-quoted scalar. A placeholder left unfilled is an error; the template
// never gets a value written back.
func RenderCompose(template, image string, getenv func(string) string) (string, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	if !strings.Contains(image, "@sha256:") {
		return "", fmt.Errorf("compose: image %q is not pinned by digest", image)
	}
	out := strings.ReplaceAll(template, "__IMAGE__", image)
	for ph, env := range ComposeSecrets {
		if !strings.Contains(out, ph) {
			continue
		}
		v := getenv(env)
		if v == "" {
			return "", fmt.Errorf("compose: %s wants %s, which is not set", ph, env)
		}
		out = strings.ReplaceAll(out, ph, "'"+strings.ReplaceAll(v, "'", "''")+"'")
	}
	for ph, env := range ComposeOptional {
		out = strings.ReplaceAll(out, ph, "'"+strings.ReplaceAll(getenv(env), "'", "''")+"'")
	}
	if m := placeholder.FindString(out); m != "" {
		return "", fmt.Errorf("compose: unfilled placeholder %s", m)
	}
	return out, nil
}
