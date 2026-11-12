package query

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// AttachmentRow is one file of a project (0049).
type AttachmentRow struct {
	Ref     string    `json:"ref"`
	SHA256  string    `json:"sha256"`
	Project string    `json:"project"`
	Name    string    `json:"name"`
	Mime    string    `json:"mime"`
	Size    int64     `json:"size"`
	AddedAt time.Time `json:"added_at"`
	Path    string    `json:"path"` // in the viewer of this server
}

// Attachments lists the files of a project (or of every project), newest first; sha narrows to one file.
func (s *Service) Attachments(ctx context.Context, p Principal, project, sha string) ([]AttachmentRow, error) {
	ctx, _, err := s.enter(ctx, p)
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(ctx, `SELECT 'att:' || left(a.sha256, 12), a.sha256, pr.name, a.name, a.mime, a.size, a.added_at,
		       '/viewer/att/' || a.sha256
		  FROM projection.attachment a JOIN projection.project pr ON pr.project_id = a.project_id
		 WHERE a.tenant_id = $1 AND ($2 = '' OR pr.name = $2) AND ($3 = '' OR a.sha256 LIKE $3 || '%')
		 ORDER BY a.added_at DESC LIMIT 200`, p.TenantID, project, sha)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (AttachmentRow, error) {
		var x AttachmentRow
		return x, r.Scan(&x.Ref, &x.SHA256, &x.Project, &x.Name, &x.Mime, &x.Size, &x.AddedAt, &x.Path)
	})
}
