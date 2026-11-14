package configedit

import "slices"

// region is the processes block cut into entry bodies and the gaps between
// them (blank lines, and comments separated from an entry by a blank line).
// Gaps stay where they are when bodies are removed, moved or swapped.
type region struct {
	eol    string
	lead   []string   // between "processes:" and the first body
	bodies [][]string // one per entry, head comment included
	seps   [][]string // seps[i] lies between bodies[i] and bodies[i+1]
	tail   []string   // after the last body (normally empty)
}

func (d *doc) region() *region {
	r := &region{eol: d.eol}
	if len(d.entries) == 0 {
		r.lead = slices.Clone(d.lines[d.procLine+1 : d.regionEnd])
		return r
	}
	r.lead = slices.Clone(d.lines[d.procLine+1 : d.entries[0].start])
	for i, e := range d.entries {
		if i > 0 {
			r.seps = append(r.seps, slices.Clone(d.lines[d.entries[i-1].end+1:e.start]))
		}
		r.bodies = append(r.bodies, slices.Clone(d.lines[e.start:e.end+1]))
	}
	return r
}

func (r *region) lines() []string {
	out := slices.Clone(r.lead)
	for i, b := range r.bodies {
		if i > 0 {
			out = append(out, r.seps[i-1]...)
		}
		out = append(out, b...)
	}
	return append(out, r.tail...)
}

// remove drops body i. Of the two gaps around it one survives: the one with
// comments in it, or the first if both are blank (both, if both have
// comments), so the spacing between the remaining entries stays the same.
func (r *region) remove(i int) {
	n := len(r.bodies)
	switch {
	case n == 1:
		// Undo the blank line insert puts after a comment-only lead.
		r.lead = trimTrailingBlank(r.lead)
	case i == 0:
		r.lead = mergeGaps(r.lead, r.seps[0])
		r.seps = r.seps[1:]
	case i == n-1:
		if gap := trimTrailingBlank(r.seps[i-1]); len(gap) > 0 {
			r.tail = append(gap, r.tail...)
		}
		r.seps = r.seps[:i-1]
	default:
		r.seps[i-1] = mergeGaps(r.seps[i-1], r.seps[i])
		r.seps = slices.Delete(r.seps, i, i+1)
	}
	r.bodies = slices.Delete(r.bodies, i, i+1)
}

// insert puts body at index j. It is separated from its neighbours the way
// the neighbouring entries are: by one blank line if they are, else by none.
func (r *region) insert(j int, body []string) {
	n := len(r.bodies)
	var sep []string
	if n >= 2 {
		near := r.seps[min(max(j-1, 0), n-2)]
		if slices.ContainsFunc(near, isBlank) {
			sep = []string{r.eol}
		}
	}
	switch {
	case n == 0:
		// Keep a comment above processes' first entry from becoming its head
		// comment, which would go wherever the entry goes.
		if len(r.lead) > 0 && !isBlank(r.lead[len(r.lead)-1]) {
			r.lead = append(r.lead, r.eol)
		}
	case j == 0:
		r.seps = slices.Insert(r.seps, 0, sep)
	case j >= n:
		r.seps = append(r.seps, sep)
	default:
		r.seps = slices.Insert(r.seps, j-1, sep)
	}
	r.bodies = slices.Insert(r.bodies, min(j, n), body)
}

func mergeGaps(a, b []string) []string {
	switch {
	case blankOnly(b):
		return a
	case blankOnly(a):
		return b
	}
	return append(slices.Clone(a), b...)
}

func blankOnly(lines []string) bool {
	return !slices.ContainsFunc(lines, func(l string) bool { return !isBlank(l) })
}

func trimTrailingBlank(lines []string) []string {
	for len(lines) > 0 && isBlank(lines[len(lines)-1]) {
		lines = lines[:len(lines)-1]
	}
	return lines
}
