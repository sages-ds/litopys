package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"litopys.smart-gw.co.ua/core/auth"
	"litopys.smart-gw.co.ua/core/policy"
	"litopys.smart-gw.co.ua/core/verbs"
	"litopys.smart-gw.co.ua/internal/pg"
)

// runAuth: `litopys auth bootstrap --email E --name N` registers the first human (or finds it)
// and issues an admin token. The raw token goes to stdout ONCE — run it yourself, in your own
// terminal, never through an assistant session or a log.
func runAuth(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] != "bootstrap" {
		return fmt.Errorf("auth: usage: litopys auth bootstrap --email <email> --name <display name> [--label <label>] [--role admin|owner] [--days N] [--profile full|core|chat|readonly] [--vendor V]")
	}
	fs := flag.NewFlagSet("auth bootstrap", flag.ContinueOnError)
	email := fs.String("email", "", "external identity of the human (e-mail)")
	name := fs.String("name", "", "display name")
	label := fs.String("label", "bootstrap", "token label")
	role := fs.String("role", "admin", "token role: admin (at most 7 days) | owner (everyday plugin token)")
	days := fs.Int("days", 0, "lifetime in days (0 = default of the role; capped by its maximum)")
	profile := fs.String("profile", "", "grant profile: full (default) | core | chat | readonly — e.g. a benchmark that sees what an OAuth client sees")
	vendor := fs.String("vendor", "", "grant vendor for audit and vendor rules (anthropic, openai, google…)")
	projects := fs.String("projects", "", "comma-separated project allowlist of the grant (slugs); empty = every project — e.g. a health-check canary")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *email == "" || *name == "" {
		return fmt.Errorf("auth bootstrap: --email and --name are required")
	}
	if !slices.Contains([]string{"", policy.ProfileFull, policy.ProfileCore, policy.ProfileChat, policy.ProfileReadonly}, *profile) {
		return fmt.Errorf("auth bootstrap: --profile full | core | chat | readonly")
	}
	db, err := pg.Pool(ctx)
	if err != nil {
		return err
	}
	defer db.Close()
	var tenant uuid.UUID
	if err := db.QueryRow(ctx, `SELECT tenant_id FROM tenant WHERE name = 'default'`).Scan(&tenant); err != nil {
		return err
	}
	actor, created, err := (&verbs.Service{DB: db}).RegisterHuman(ctx, tenant, *name, *email)
	if err != nil {
		return err
	}
	if *role != "admin" && *role != "owner" {
		return fmt.Errorf("auth bootstrap: --role admin | owner")
	}
	g := policy.Grant{Profile: *profile, Vendor: *vendor}
	if *projects != "" {
		g.ProjectAllowlist = strings.Split(*projects, ",")
	}
	raw, id, err := auth.IssueGrant(ctx, db, tenant, actor, *role, *label, nil, g, time.Duration(*days)*24*time.Hour)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "actor %s (%s), token %s, role %s — the token below is shown ONCE, store it now:\n",
		actor, map[bool]string{true: "registered", false: "existing"}[created], id, *role)
	fmt.Println(raw)
	return nil
}
