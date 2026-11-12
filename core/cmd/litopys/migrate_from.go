package main

import (
	"context"
	"errors"
	"fmt"
)

// profiles of `litopys migrate-from`: named by the data format, not by a product. A migration from
// another provider's database is a separate tool beside the core.
var profiles = map[string]string{
	"vault-curated-v1": "curated zone of an Obsidian vault: markdown notes with frontmatter (uid, title, project, derived_from)",
}

// runMigrateFrom: `litopys migrate-from vault-curated-v1 --dir <vault> --owner <e-mail> [--dry-run] [--report <file>]`.
// Target: this instance's database, as for every command (LITOPYS_SET_ROLE=litopys_app). --dry-run runs the import in
// one transaction that is rolled back. Repeatable: a second run writes nothing that is already there.
func runMigrateFrom(ctx context.Context, args []string) error {
	usage := errors.New("migrate-from: usage: litopys migrate-from vault-curated-v1 --dir <vault> --owner <e-mail> [--dry-run] [--report <file>]")
	if len(args) == 0 {
		return usage
	}
	profile := args[0]
	desc, ok := profiles[profile]
	if !ok {
		return fmt.Errorf("migrate-from: unknown profile %q (profiles: vault-curated-v1)", profile)
	}
	mode, reportFile, dir, owner := "migrate", "", "", ""
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--dry-run":
			mode = "dry-run"
		case "--verify":
			mode = "verify"
		case "--report", "--dir", "--owner":
			if i+1 >= len(args) {
				return usage
			}
			i++
			switch args[i-1] {
			case "--report":
				reportFile = args[i]
			case "--dir":
				dir = args[i]
			default:
				owner = args[i]
			}
		default:
			return usage
		}
	}
	return runCurated(ctx, desc, dir, owner, mode, reportFile)
}
