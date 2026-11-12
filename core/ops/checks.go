package ops

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Level of a doctor line; the exit code of doctor is the worst level (0 ok, 1 warning, 2 critical).
type Level int

const (
	OK Level = iota
	Warn
	Crit
)

func (l Level) String() string { return [...]string{"ok", "warn", "crit"}[l] }

func (l Level) MarshalJSON() ([]byte, error) { return json.Marshal(l.String()) }

// Result is one doctor line: what was checked, how bad, and what to do.
type Result struct {
	Check  string `json:"check"`
	Level  Level  `json:"level"`
	Detail string `json:"detail"`
	Advice string `json:"advice,omitempty"`
}

// Thresholds of the doctor checks.
const (
	LagGrace      = 5 * time.Minute
	BackupMaxAge  = 26 * time.Hour
	PoolMinFree   = 0.15
	CertWarnAhead = 30 * 24 * time.Hour
	CertCritAhead = 7 * 24 * time.Hour
	ClockMaxSkew  = 2 * time.Second
)

// Worst is the level of a whole report.
func Worst(rs []Result) Level {
	w := OK
	for _, r := range rs {
		w = max(w, r.Level)
	}
	return w
}

// Evaluate turns the facts into doctor lines. Pure: every provocation is a Facts value.
// A part that could not be read is a warning (doctor cannot vouch for it), except the database and the
// service, which are critical: without them nothing else matters.
func Evaluate(f *Facts) []Result {
	var out []Result
	add := func(check string, l Level, detail, advice string) {
		out = append(out, Result{Check: check, Level: l, Detail: detail, Advice: advice})
	}

	// services and /healthz
	if f.AppsErr != "" {
		add("services", Warn, "platform not readable: "+f.AppsErr, "check the adapter access (truenas.ssh in install.yaml)")
	} else {
		for _, role := range sortedKeys(f.Apps) {
			a := f.Apps[role]
			if a.State != "RUNNING" {
				add("services", Crit, fmt.Sprintf("%s (%s) is %s", a.Name, role, a.State), "see the service logs; then litopys update --resume")
				continue
			}
			add("services", OK, fmt.Sprintf("%s (%s) running", a.Name, role), "")
		}
	}
	if f.Health.OK {
		add("healthz", OK, "core answers ok", "")
	} else {
		add("healthz", Crit, "core: "+f.Health.Err, "see the service logs; then litopys update --resume")
	}

	// image digests and commit against the release manifest
	if f.Manifest == nil {
		add("image", Warn, "no release manifest ("+f.ManifestErr+"): the running image is not checked", "install the manifest of the release (manifest in install.yaml)")
	} else {
		evalImage(f, add)
	}

	// the database: schema, projector, integrity, clock
	if f.DB == nil {
		add("database", Crit, "not readable: "+f.DBErr, "check the database service and the PG* / LITOPYS_DATABASE_URL credentials")
	} else {
		evalSchema(f, add)
		evalProjector(f, add)
		if len(f.DB.Findings) > 0 {
			add("verify", Crit, strings.Join(f.DB.Findings, "; "), "integrity incident: stop writes and restore from the last verified backup")
		} else {
			add("verify", OK, "journal gapless, statements have their events", "")
		}
		if skew := f.DB.ClockSkew.Abs(); skew > ClockMaxSkew {
			add("clock", Warn, fmt.Sprintf("database clock differs from this host by %s", f.DB.ClockSkew.Round(time.Millisecond)), "synchronise time (NTP) on the host and the database server")
		} else {
			add("clock", OK, fmt.Sprintf("skew %s", f.DB.ClockSkew.Round(time.Millisecond)), "")
		}
	}

	// backup age and the restore check `litopys backup` recorded beside the dump (a dump without a record — made by
	// another tool — counts by its age only)
	switch b := f.Backup; {
	case b.Err != "":
		add("backup", Warn, "backup directory not readable from this host: "+b.Err, "run doctor on the backup host, or litopys backup")
	case b.At == nil:
		add("backup", Crit, "no dump in "+b.Dir, "litopys backup")
	case f.At.Sub(*b.At) > BackupMaxAge:
		add("backup", Crit, fmt.Sprintf("newest %s is %s old", b.Newest, age(f.At.Sub(*b.At))), "litopys backup")
	case b.Checked && !b.Verified:
		add("backup", Crit, fmt.Sprintf("newest %s failed its restore check: %s", b.Newest, strings.Join(b.Findings, "; ")),
			"litopys backup --verify <dump>; the last good backup is an older dump")
	default:
		add("backup", OK, fmt.Sprintf("newest %s, %s old", b.Newest, age(f.At.Sub(*b.At))), "")
	}

	// data pool
	if f.PoolErr != "" {
		add("pool", Warn, "pool not readable: "+f.PoolErr, "")
	} else if p := f.Pool; p != nil && p.Size > 0 {
		share := float64(p.Free) / float64(p.Size)
		l := OK
		if share < PoolMinFree {
			l = Warn
		}
		add("pool", l, fmt.Sprintf("%s: %.0f %% free", p.Name, share*100), map[bool]string{true: "free space on the data pool", false: ""}[l == Warn])
	}

	// certificate of the public address
	if f.CertErr != "" {
		add("certificate", Warn, "not readable: "+f.CertErr, "")
	} else if c := f.Cert; c != nil {
		left := c.NotAfter.Sub(f.At)
		detail := fmt.Sprintf("%s expires %s (in %s)", c.Subject, c.NotAfter.Format("2006-01-02"), age(left))
		switch {
		case c.VerifyErr != "":
			add("certificate", Crit, detail+": "+c.VerifyErr, "litopys cert install")
		case left < CertCritAhead:
			add("certificate", Crit, detail, "litopys cert install")
		case left < CertWarnAhead:
			add("certificate", Warn, detail, "litopys cert install")
		default:
			add("certificate", OK, detail, "")
		}
	}

	// identity provider
	if i := f.IdP; i != nil {
		if i.OK {
			add("idp", OK, "OpenID discovery answers; orphan DCR clients are not checked (needs admin credentials)", "")
		} else {
			add("idp", Warn, "OpenID discovery: "+i.Err, "check the identity provider service")
		}
	}
	return out
}

func evalImage(f *Facts, add func(string, Level, string, string)) {
	want := f.Manifest.Images["core"]
	core, ok := f.Apps["core"]
	switch {
	case want == "":
		add("image", Warn, "the manifest names no core image", "")
	case !ok || f.ImagesErr != "":
		add("image", Warn, "running images not readable "+f.ImagesErr, "")
	default:
		var bad []string
		for _, c := range core.Containers {
			if !hasDigest(f.Images[c.Image], want) {
				bad = append(bad, fmt.Sprintf("%s runs %s", c.Service, c.Image))
			}
		}
		if len(bad) > 0 {
			add("image", Warn, "not the release image: "+strings.Join(bad, ", ")+"; release "+want,
				"litopys update "+f.Manifest.Version)
		} else {
			add("image", OK, "core runs the release digest "+shortDigest(want), "")
		}
	}
	if f.Health.Commit != "" && f.Manifest.Commit != "" && !sameCommit(f.Health.Commit, f.Manifest.Commit) {
		add("image", Warn, fmt.Sprintf("core reports commit %s, release %s", short(f.Health.Commit), short(f.Manifest.Commit)),
			"litopys update "+f.Manifest.Version)
	}
}

var migrationVersion = regexp.MustCompile(`^(\d{4})_`)

func evalSchema(f *Facts, add func(string, Level, string, string)) {
	want := versionOf(f.Expected.MigrationHead)
	applied := map[int]Applied{}
	got := 0
	for _, a := range f.DB.Applied {
		applied[a.Version] = a
		got = max(got, a.Version)
	}
	advice := "litopys update (not a manual migrate up)"
	switch {
	case want == 0:
		add("migrations", Warn, "expected migration head unknown", "")
		return
	case got < want:
		add("migrations", Crit, fmt.Sprintf("database is at %04d, the %s expects %s", got, f.Expected.Source, f.Expected.MigrationHead), advice)
		return
	case got > want:
		add("migrations", Crit, fmt.Sprintf("database is at %04d, ahead of the %s (%s): an older binary serves a newer schema", got, f.Expected.Source, f.Expected.MigrationHead), advice)
		return
	}
	var issues []string
	emb := embedded()
	for v := 1; v <= want; v++ {
		a, ok := applied[v]
		if !ok {
			if _, known := emb[v]; known || f.Expected.Source == "manifest" {
				issues = append(issues, fmt.Sprintf("%04d not applied", v))
			}
			continue
		}
		if m, ok := emb[v]; ok && f.Expected.Source == "binary" && !m.Accepts(a.Checksum) {
			issues = append(issues, a.Name+" was edited after it ran (checksum)")
		}
	}
	if len(issues) > 0 {
		add("migrations", Crit, strings.Join(issues, "; "), advice)
		return
	}
	add("migrations", OK, fmt.Sprintf("database at %s, as the %s expects", f.Expected.MigrationHead, f.Expected.Source), "")
}

func evalProjector(f *Facts, add func(string, Level, string, string)) {
	advice := "projector incident: see the operations runbook (stuck projections)"
	for _, t := range f.DB.Tenants {
		lag := t.Head - t.ProjectorSeq
		switch {
		case t.ProjectorVer != f.Expected.ProjectorVersion:
			add("projector", Crit, fmt.Sprintf("tenant %s: projections built by v%s, the %s expects v%s", t.Name, t.ProjectorVer, f.Expected.Source, f.Expected.ProjectorVersion), advice)
		case lag > 0 && t.OldestPendingAt != nil && f.At.Sub(*t.OldestPendingAt) > LagGrace:
			add("projector", Crit, fmt.Sprintf("tenant %s: %d events behind for %s", t.Name, lag, age(f.At.Sub(*t.OldestPendingAt))), advice)
		case lag > 0:
			add("projector", OK, fmt.Sprintf("tenant %s: %d events behind, catching up", t.Name, lag), "")
		default:
			add("projector", OK, fmt.Sprintf("tenant %s: v%s, caught up at %d", t.Name, t.ProjectorVer, t.Head), "")
		}
	}
}

func versionOf(name string) int {
	m := migrationVersion.FindStringSubmatch(name)
	if m == nil {
		return 0
	}
	v, _ := strconv.Atoi(m[1])
	return v
}

func hasDigest(digests []string, want string) bool {
	_, wd, _ := strings.Cut(want, "@")
	for _, d := range digests {
		if _, dd, _ := strings.Cut(d, "@"); dd != "" && dd == wd {
			return true
		}
	}
	return false
}

// sameCommit: one may be abbreviated (image tags carry 12 characters, /version the full hash).
func sameCommit(a, b string) bool {
	n := min(len(a), len(b))
	return n >= 7 && a[:n] == b[:n]
}

func short(c string) string { return c[:min(len(c), 12)] }

func shortDigest(ref string) string {
	_, d, _ := strings.Cut(ref, "@sha256:")
	return "sha256:" + d[:min(len(d), 12)]
}

// age renders a duration the way an operator reads it: 3d4h, 5h12m, 40s.
func age(d time.Duration) string {
	if d < 0 {
		return "-" + age(-d)
	}
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
	case d >= time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	case d >= time.Minute:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return d.Round(time.Second).String()
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// MigrationNumber is the number of a migration file name (0042_maintenance.sql → 42); 0 for anything else.
func MigrationNumber(name string) int { return versionOf(name) }
