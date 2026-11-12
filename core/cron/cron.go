// Package cron reads the schedule of a workflow trigger (kind schedule): the five fields minute
// hour day-of-month month day-of-week, each *, a number, a list (1,15), a range (1-5) or a step (*/15, 8-18/2).
// Times are Europe/Kyiv, as the rest of the person's days (retention, due dates).
package cron

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Spec is a parsed schedule.
type Spec struct{ min, hour, dom, month, dow [64]bool }

var limits = [5][2]int{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 6}}

// Parse reads "m h dom mon dow"; a day-of-week of 7 is Sunday too.
func Parse(s string) (Spec, error) {
	var sp Spec
	f := strings.Fields(s)
	if len(f) != 5 {
		return sp, fmt.Errorf("cron %q: five fields: minute hour day month weekday", s)
	}
	sets := []*[64]bool{&sp.min, &sp.hour, &sp.dom, &sp.month, &sp.dow}
	for i, field := range f {
		lo, hi := limits[i][0], limits[i][1]
		if i == 4 {
			hi = 7
		}
		for _, part := range strings.Split(field, ",") {
			rng, step := part, 1
			if a, b, ok := strings.Cut(part, "/"); ok {
				n, err := strconv.Atoi(b)
				if err != nil || n < 1 {
					return sp, fmt.Errorf("cron %q: step %q", s, b)
				}
				rng, step = a, n
			}
			from, to := lo, hi
			if rng != "*" {
				a, b, isRange := strings.Cut(rng, "-")
				x, err := strconv.Atoi(a)
				if err != nil {
					return sp, fmt.Errorf("cron %q: %q", s, part)
				}
				from, to = x, x
				if isRange {
					if to, err = strconv.Atoi(b); err != nil {
						return sp, fmt.Errorf("cron %q: %q", s, part)
					}
				} else if step > 1 {
					to = hi
				}
			}
			if from < lo || to > hi || from > to {
				return sp, fmt.Errorf("cron %q: %q outside %d-%d", s, part, lo, hi)
			}
			for v := from; v <= to; v += step {
				if i == 4 && v == 7 {
					v = 0
					sets[i][v] = true
					break
				}
				sets[i][v] = true
			}
		}
	}
	return sp, nil
}

// Kyiv is the zone schedules are read in.
var Kyiv = func() *time.Location {
	if l, err := time.LoadLocation("Europe/Kyiv"); err == nil {
		return l
	}
	return time.UTC
}()

// Due: the minute of t (in Kyiv) is one the schedule names.
func (sp Spec) Due(t time.Time) bool {
	t = t.In(Kyiv)
	return sp.min[t.Minute()] && sp.hour[t.Hour()] && sp.dom[t.Day()] && sp.month[int(t.Month())] && sp.dow[int(t.Weekday())]
}
