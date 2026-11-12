package ops

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

// PrintStatus is `litopys status` for a person: release, services, schema, projector, backup, certificate.
func PrintStatus(w io.Writer, f *Facts) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	row := func(k, v string) { fmt.Fprintf(tw, "%s\t%s\n", k, v) }
	if m := f.Manifest; m != nil {
		row("release", fmt.Sprintf("%s (commit %s)", m.Version, short(m.Commit)))
	} else {
		row("release", "no manifest: "+f.ManifestErr)
	}
	if f.AppsErr != "" {
		row("services", "not readable: "+f.AppsErr)
	}
	for _, role := range sortedKeys(f.Apps) {
		a := f.Apps[role]
		var imgs []string
		for _, c := range a.Containers {
			d := ""
			if ds := f.Images[c.Image]; len(ds) > 0 {
				d = " " + shortDigest(ds[0])
			}
			imgs = append(imgs, fmt.Sprintf("%s=%s %s%s", c.Service, c.State, c.Image, d))
		}
		row(role, strings.TrimSpace(a.Name+" "+a.State+"  "+strings.Join(imgs, ", ")))
	}
	switch {
	case f.Health.OK:
		row("health", fmt.Sprintf("healthz ok, commit %s, migration %s", short(f.Health.Commit), f.Health.Migration))
	default:
		row("health", "healthz: "+f.Health.Err)
	}
	if db := f.DB; db != nil {
		head := "-"
		if n := len(db.Applied); n > 0 {
			head = db.Applied[n-1].Name
		}
		row("schema", fmt.Sprintf("database %s; %s expects %s, projector v%s", head, f.Expected.Source, f.Expected.MigrationHead, f.Expected.ProjectorVersion))
		for _, t := range db.Tenants {
			row("journal", fmt.Sprintf("tenant %s: head %d, projector %d (v%s), lag %d", t.Name, t.Head, t.ProjectorSeq, t.ProjectorVer, t.Head-t.ProjectorSeq))
		}
	} else {
		row("database", "not readable: "+f.DBErr)
	}
	switch b := f.Backup; {
	case b.Err != "":
		row("backup", "not readable: "+b.Err)
	case b.At == nil:
		row("backup", "no dump in "+b.Dir)
	default:
		check := "not recorded"
		switch {
		case b.Checked && b.Verified:
			check = "ok"
		case b.Checked:
			check = "FAILED: " + strings.Join(b.Findings, "; ")
		}
		row("backup", fmt.Sprintf("%s, %s old (restore check: %s)", b.Newest, age(f.At.Sub(*b.At)), check))
	}
	if c := f.Cert; c != nil {
		row("certificate", fmt.Sprintf("%s until %s (%s)", c.Subject, c.NotAfter.Format("2006-01-02"), age(c.NotAfter.Sub(f.At))))
	} else if f.CertErr != "" {
		row("certificate", "not readable: "+f.CertErr)
	}
	if f.Target == "truenas" {
		row("openbao", "not part of a truenas installation")
	}
	_ = tw.Flush()
}

// PrintDoctor is `litopys doctor` for a person: one line per check, advice after the arrow.
func PrintDoctor(w io.Writer, rs []Result) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	counts := map[Level]int{}
	for _, r := range rs {
		counts[r.Level]++
		line := fmt.Sprintf("%s\t%s\t%s", strings.ToUpper(r.Level.String()), r.Check, r.Detail)
		if r.Advice != "" {
			line += "  → " + r.Advice
		}
		fmt.Fprintln(tw, line)
	}
	_ = tw.Flush()
	fmt.Fprintf(w, "doctor: %d critical, %d warning, %d ok\n", counts[Crit], counts[Warn], counts[OK])
}
