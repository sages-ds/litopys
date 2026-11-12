package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"litopys.smart-gw.co.ua/core/privacy"
	"litopys.smart-gw.co.ua/core/verbs"
	"litopys.smart-gw.co.ua/internal/pg"
)

// runPurge: `litopys purge --type statement --id <uuid> --reason R [--email E] [--yes]` is journal.purge
// run by a person outside the API: the pilot facade has no admin pool (LITOPYS_ADMIN_DATABASE_URL),
// so the owner runs it with a login that may SET ROLE litopys_admin (the superuser of the stand). Without
// --yes it only shows what would be erased (the title masked) and changes nothing. The purge is recorded
// as done by the human of --email, or for a statement without it by the statement's owner.
func runPurge(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("purge", flag.ContinueOnError)
	typ := fs.String("type", "statement", "entity type (target.entity_type of journal.purge)")
	id := fs.String("id", "", "entity id (uuid)")
	reason := fs.String("reason", "", "why the content is erased (kept in the journal)")
	basis := fs.String("legal-basis", "", "legal basis, optional (e.g. a leaked secret, art.17)")
	email := fs.String("email", "", "e-mail of the human who purges (an existing actor); a statement defaults to its owner")
	yes := fs.Bool("yes", false, "erase; without it only the preview is shown")
	if err := fs.Parse(args); err != nil {
		return err
	}
	entity, err := uuid.Parse(*id)
	if err != nil {
		return fmt.Errorf("purge: --id: %w", err)
	}
	if *reason == "" {
		return errors.New("purge: --reason is required")
	}
	if *email == "" && *typ != "statement" {
		return errors.New("purge: --email is required for a " + *typ)
	}
	db, err := pg.Pool(ctx) // LITOPYS_SET_ROLE=litopys_app, as everywhere
	if err != nil {
		return err
	}
	defer db.Close()
	admin, err := rolePool(ctx, "litopys_admin")
	if err != nil {
		return err
	}
	defer admin.Close()
	sys := pg.WithScope(ctx, pg.ScopeSystem)
	var tenant, actor uuid.UUID
	if err := db.QueryRow(sys, `SELECT tenant_id FROM tenant WHERE name = 'default'`).Scan(&tenant); err != nil {
		return err
	}
	if *email != "" {
		err = db.QueryRow(sys, `SELECT actor_id FROM projection.actor WHERE tenant_id = $1 AND kind = 'human' AND external_identity = $2`,
			tenant, *email).Scan(&actor)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("purge: no human with e-mail %q", *email)
		} else if err != nil {
			return err
		}
	}
	if *typ == "statement" {
		var project, kind, status, title string
		var recorded time.Time
		var owner uuid.UUID
		err := db.QueryRow(sys, `SELECT p.name, s.type, s.status, s.title, s.recorded_at, s.owner_id FROM projection.statement s
		        JOIN projection.project p USING (project_id) WHERE s.statement_id = $1`, entity).Scan(&project, &kind, &status, &title, &recorded, &owner)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("purge: statement %s not found (purged already, or another id)", entity)
		} else if err != nil {
			return err
		}
		fmt.Printf("statement %s\n  project %s · %s · %s · %s\n  title   %s\n", entity, project, kind, status,
			recorded.Format("2006-01-02 15:04"), privacy.MaskText(title))
		if actor == uuid.Nil {
			actor = owner
		}
	} else {
		fmt.Printf("%s %s (no preview for this type)\n", *typ, entity)
	}
	if !*yes {
		fmt.Println("preview only: nothing erased; run again with --yes to purge")
		return nil
	}
	s := &verbs.Service{DB: db, Admin: admin}
	var lb *string
	if *basis != "" {
		lb = basis
	}
	ack, err := s.JournalPurge(ctx, verbs.Principal{TenantID: tenant, ActorID: actor, Role: "admin"}, *typ, entity, *reason, lb)
	if err != nil {
		return err
	}
	fmt.Printf("purged: event %s, seq %d\n", ack.EventID, ack.Seq)
	return nil
}

// rolePool: a pool on the same login whose every connection runs as role (the application roles are NOLOGIN).
func rolePool(ctx context.Context, role string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(pg.DSN())
	if err != nil {
		return nil, fmt.Errorf("pool config: %w", err)
	}
	cfg.MaxConns = 2
	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		_, err := c.Exec(ctx, "SET ROLE "+pgx.Identifier{role}.Sanitize())
		return err
	}
	cfg.PrepareConn = pg.PrepareConn // litopys_admin is a member of litopys_app: same RLS (0008)
	return pgxpool.NewWithConfig(ctx, cfg)
}
