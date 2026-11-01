package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Plan is what the coordinator decides BEFORE any specialist runs: the topic,
// which desks are asked, in which order their sections appear, and who the
// artifact is for.
//
// Order lives here rather than being read off the join map, because the join
// output is a Go map and ranging over it is deliberately unordered. A document
// whose sections shuffle between runs cannot be hashed, diffed or approved.
type Plan struct {
	Topic      string   `json:"topic"`
	Order      []string `json:"order"`
	Recipients []string `json:"recipients"`
	Deadline   string   `json:"deadline"`
}

// Section is one desk's contribution. It carries the evidence, not only prose:
// Findings is what the desk's source returned, verbatim.
//
// Status is the whole point of the containment requirement. A desk that did not
// answer produces a section with Status "unverified", a named Gap and a machine
// readable Code — never a plausible paragraph with no source behind it.
type Section struct {
	Desk     string    `json:"desk"`
	Title    string    `json:"title"`
	Status   string    `json:"status"`
	Findings []Finding `json:"findings,omitempty"`
	// Summary is the specialist model's prose. It is decoration: the human
	// approves on Findings, and Summary is never produced for a gap.
	Summary string `json:"summary,omitempty"`
	Gap     string `json:"gap,omitempty"`
	Code    string `json:"code,omitempty"`
}

// Section statuses.
const (
	StatusVerified   = "verified"
	StatusUnverified = "unverified"
)

// Report is the artifact the board would receive.
type Report struct {
	Topic    string    `json:"topic"`
	Sections []Section `json:"sections"`
}

// Gaps returns the unverified sections, in report order.
func (r Report) Gaps() []Section {
	var out []Section
	for _, s := range r.Sections {
		if s.Status != StatusVerified {
			out = append(out, s)
		}
	}
	return out
}

// Verified reports whether every section has a source behind it.
func (r Report) Verified() bool { return len(r.Gaps()) == 0 }

// Sources lists every source identifier the report stands on, deduplicated and
// sorted. An empty list means the document rests on nothing.
func (r Report) Sources() []string {
	seen := map[string]struct{}{}
	for _, s := range r.Sections {
		for _, f := range s.Findings {
			if f.Source != "" {
				seen[f.Source] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// Hash pins the artifact for the approval ledger.
//
// It is computed over the canonical JSON of the whole report, so a changed
// finding, a reordered section or a gap that was silently filled in later all
// produce a different hash — and approvals.Spend refuses the send. Approving
// v1 does not approve v2.
func (r Report) Hash() string {
	b, err := json.Marshal(r)
	if err != nil {
		// Report holds only strings and slices of strings; a failure here is
		// not recoverable data, so surface it as a hash that can never match.
		return "unhashable:" + err.Error()
	}
	return fmt.Sprintf("%x", sha256.Sum256(b))
}

// Render is the human-readable document. Gaps are printed as gaps, in place,
// with their reason — the alternative is a document that reads as complete.
func (r Report) Render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "План міграції: %s\n", r.Topic)
	for _, s := range r.Sections {
		fmt.Fprintf(&b, "\n## %s\n", s.Title)
		if s.Status != StatusVerified {
			fmt.Fprintf(&b, "ПРОГАЛИНА (%s): %s\n", s.Code, s.Gap)
			fmt.Fprintf(&b, "Розділ не заповнено: джерело не відповіло. Даних немає — і їх тут немає.\n")
			continue
		}
		if s.Summary != "" {
			fmt.Fprintf(&b, "%s\n", s.Summary)
		}
		for _, f := range s.Findings {
			fmt.Fprintf(&b, "- %s (%s)\n", f.Claim, f.Source)
		}
	}
	if gaps := r.Gaps(); len(gaps) > 0 {
		fmt.Fprintf(&b, "\nНеперевірених розділів: %d — документ не надсилається.\n", len(gaps))
	}
	return b.String()
}

// Facts is the evidence block handed to the reviewer and stored in the ledger.
//
// It is built from the report's own fields, not written by the coordinator.
// That is the rule this lab is about: the human is shown the action, the
// recipients, the artifact hash, the gaps and the sources — not a paragraph
// that assures them everything is fine.
func (r Report) Facts(p Plan) map[string]any {
	gaps := make([]string, 0, len(r.Sections))
	for _, s := range r.Gaps() {
		gaps = append(gaps, fmt.Sprintf("%s: %s (%s)", s.Desk, s.Gap, s.Code))
	}
	return map[string]any{
		"action":       "надіслати план міграції раді директорів",
		"topic":        r.Topic,
		"recipients":   p.Recipients,
		"deadline":     p.Deadline,
		"hash":         r.Hash(),
		"sections":     sectionStates(r),
		"gaps":         gaps,
		"sources":      r.Sources(),
		"verified":     r.Verified(),
		"blocked_note": blockedNote(r),
	}
}

// sectionStates is the per-desk status line of the evidence block.
func sectionStates(r Report) []string {
	out := make([]string, 0, len(r.Sections))
	for _, s := range r.Sections {
		out = append(out, fmt.Sprintf("%s=%s", s.Desk, s.Status))
	}
	return out
}

// blockedNote tells the reviewer, before they answer, that approving will not
// make this document send. Hiding that would waste their decision.
func blockedNote(r Report) string {
	if r.Verified() {
		return ""
	}
	return "УВАГА: є неперевірені розділи. Навіть схвалення не надішле цей документ — " +
		"спершу потрібне джерело, а не підпис."
}
