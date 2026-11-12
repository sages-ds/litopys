package verbs

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// AttachmentAdd records a file the caller already stored by its SHA-256 (core/files): one event with what describes
// it. The same file in the same project again answers with the one there.
func (s *Service) AttachmentAdd(ctx context.Context, p Principal, project, sha, name, mime string, size int64) (AttachmentOut, error) {
	var out AttachmentOut
	name = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(name, "/", "_"), "\\", "_"))
	if err := checkLen("name", name, 1, 255); err != nil {
		return out, err
	}
	// "text/plain; charset=utf-8" is what clients send: keep the media type, drop the parameters (roast r2, Р2-6)
	if i := strings.IndexByte(mime, ';'); i >= 0 {
		mime = mime[:i]
	}
	mime = strings.ToLower(strings.TrimSpace(mime))
	if mime == "" {
		mime = "application/octet-stream"
	}
	if !mimeRe.MatchString(mime) {
		return out, invalid("mime", "a media type, e.g. application/pdf")
	}
	err := s.exec(ctx, s.DB, p, func(c *cmd) error {
		projectID, err := c.projectID(project)
		if err != nil {
			return err
		}
		out.SHA256, out.Size, out.Name, out.Ref = sha, size, name, "att:"+sha[:12]
		err = c.tx.QueryRow(ctx, `SELECT attachment_id FROM projection.attachment WHERE project_id = $1 AND sha256 = $2`,
			projectID, sha).Scan(&out.AttachmentID)
		if err == nil {
			out.Already = true
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		out.AttachmentID = uuid.New()
		e, err := c.emit("attachment.added", "attachment", out.AttachmentID, nil, nil,
			map[string]any{"project_id": projectID, "sha256": sha, "name": name, "mime": mime, "size": size})
		if err != nil {
			return err
		}
		out.Ack = ack(e)
		return nil
	})
	return out, err
}

var mimeRe = regexp.MustCompile(`^[a-z]+/[a-z0-9.+-]{1,90}$`)

// AttachmentOut answers attachment.put.
type AttachmentOut struct {
	AttachmentID uuid.UUID `json:"attachment_id"`
	Ref          string    `json:"ref"` // att:<first 12 of sha256> — what files of a record name
	SHA256       string    `json:"sha256"`
	Name         string    `json:"name"`
	Size         int64     `json:"size"`
	Ack
}
