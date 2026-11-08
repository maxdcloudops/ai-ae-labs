// Package improve is the control loop of the autoresearch pattern: brakes (b),
// (c) and (e) — a fixed budget, one objective metric, and a full log of every
// experiment including the discarded ones.
//
// Adapted from the course reference (labs12/solution/improve.go), which the
// assignment says to adapt rather than rewrite. What is kept: the
// Proposer/Scorer seam, the budget accounting, the keep/discard rule with an
// epsilon, and writing the candidate to disk before scoring it. What is mine:
// the Entry shape required by the reviewer checklist, the two-sided metric
// (dev decides, heldout only reports), the revert-on-discard path through a
// sandbox with two mutable entries, and the git SHA in every record.
//
// It imports no ADK: the loop and its brakes are testable without a model,
// without a key and without an agent. That is deliberate — if the control loop
// needed an LLM to be exercised, nobody would exercise it.
package improve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Verdict is the outcome of one experiment.
type Verdict string

const (
	// Keep — the candidate measured better by more than epsilon.
	Keep Verdict = "keep"
	// Discard — it did not, so the previous strategy stays.
	Discard Verdict = "discard"
)

// StopReason says why the loop ended. Every one of them is a named stop: the
// loop never just "finishes".
type StopReason string

const (
	// StopBudget — the experiment budget is spent.
	StopBudget StopReason = "budget_exhausted"
	// StopTokens — the token budget is spent.
	StopTokens StopReason = "token_budget_exhausted"
	// StopWall — the wall-clock budget is spent.
	StopWall StopReason = "time_budget_exhausted"
	// StopProposer — the proposer could not produce a candidate.
	StopProposer StopReason = "proposer_gave_up"
	// StopRunning — not a stop; the loop may continue.
	StopRunning StopReason = ""
)

// Errors a caller branches on.
var (
	// ErrBudget — the loop stopped because a budget ran out.
	ErrBudget = errors.New("improve: бюджет вичерпано")
	// ErrNoProposer — the engine was built without a proposer.
	ErrNoProposer = errors.New("improve: немає пропонента")
	// ErrNoScorer — the engine was built without a scorer.
	ErrNoScorer = errors.New("improve: немає оцінювача")
	// ErrGoldenLeak — the candidate strategy contains answers from the eval
	// set. Keeping it would make the metric measure the leak, not the agent.
	ErrGoldenLeak = errors.New("improve: у стратегії є відповіді з eval-набору")
)

// Budget is brake (b): a hard stop, declared up front.
//
// Zero is not "unlimited" for Experiments — Validate refuses it. "Unlimited"
// is not a valid setting for a loop that calls a paid model, and the zero value
// reading like "default" is how that happens by accident.
type Budget struct {
	// Experiments caps the number of candidates tried.
	Experiments int
	// Tokens caps total model tokens; 0 means do not cap on tokens.
	Tokens int
	// Wall caps wall-clock time; 0 means do not cap on time.
	Wall time.Duration
	// Epsilon is the improvement a candidate must beat to be kept. Without it
	// noise in the metric looks like progress.
	Epsilon float64
}

// Validate refuses a budget that cannot stop.
func (b Budget) Validate() error {
	switch {
	case b.Experiments <= 0:
		return fmt.Errorf("improve: Experiments = %d; нуль означає «без обмежень», а це не налаштування для платної моделі", b.Experiments)
	case b.Tokens < 0:
		return fmt.Errorf("improve: Tokens = %d", b.Tokens)
	case b.Wall < 0:
		return fmt.Errorf("improve: Wall = %s", b.Wall)
	case b.Epsilon < 0:
		return fmt.Errorf("improve: Epsilon = %f", b.Epsilon)
	}
	return nil
}

// Measurement is what the scorer reports.
//
// Dev decides keep/discard; Heldout is reported and never decides. Optimising
// against the set you also report on is how a strategy that memorises the dev
// questions looks like a strategy that got better.
type Measurement struct {
	Dev     float64 `json:"dev"`
	Heldout float64 `json:"heldout"`
	// Found and Total are the raw counts behind Dev, so a reader can see
	// whether 0.917 means 11/12 or 110/120.
	Found int `json:"found"`
	Total int `json:"total"`
	// Tokens spent measuring this candidate.
	Tokens int `json:"tokens"`
}

// Proposal is one candidate strategy plus the one-line reason for it.
type Proposal struct {
	Content string
	// Summary is the diff_summary of the log record: what changed and why, in
	// one line a human can scan.
	Summary string
}

// Proposer suggests the next candidate strategy.
type Proposer interface {
	Propose(ctx context.Context, current string, m Measurement, missing []string) (Proposal, error)
}

// Scorer measures a candidate. It must be a pure function of the candidate as
// far as the loop is concerned: same strategy, same score.
type Scorer interface {
	Score(ctx context.Context, candidate string) (Measurement, error)
}

// Surface is the write surface the engine is allowed to touch. The sandbox
// package satisfies it; the interface keeps this package free of that import
// and lets a test hand in a surface that refuses everything.
type Surface interface {
	ReadStrategy() ([]byte, error)
	WriteStrategy([]byte) error
	Write(path string, content []byte) error
}

// LeakChecker reports eval answers that leaked into a candidate strategy.
type LeakChecker func(text string) []string

// Entry is one line of experiments.log.
//
// The four fields the reviewer checklist names — prev_metric, new_metric,
// verdict, diff_summary — plus what makes a record reproducible: when, against
// which commit, how long, and how many tokens. Discarded experiments are
// logged too: compliance wants to see why you did NOT improve just as much as
// how you did.
type Entry struct {
	N            int      `json:"n"`
	TS           string   `json:"ts"`
	PrevMetric   float64  `json:"prev_metric"`
	NewMetric    float64  `json:"new_metric"`
	Verdict      Verdict  `json:"verdict"`
	DiffSummary  string   `json:"diff_summary"`
	Reason       string   `json:"reason"`
	HeldoutPrev  float64  `json:"heldout_prev"`
	HeldoutNew   float64  `json:"heldout_new"`
	Found        int      `json:"found"`
	Total        int      `json:"total"`
	Tokens       int      `json:"tokens"`
	DurationSec  float64  `json:"duration_sec"`
	GitSHA       string   `json:"git_sha"`
	Missing      []string `json:"missing,omitempty"`
	DiscardedRel string   `json:"discarded_rel,omitempty"`
}

// floatTol absorbs binary rounding in the keep/discard comparison.
//
// It is not decoration. The metric is a fraction (found/total) and epsilon is a
// decimal, so "improved by exactly epsilon" is usually not representable:
// 0.54 - 0.5 evaluates to 0.040000000000000036, which is greater than 0.04.
// Without the tolerance, a candidate that improves by exactly the threshold is
// kept or discarded depending on where the rounding fell — and the same
// numbers on a different machine could decide differently. The intent is
// "strictly better than epsilon", so the tolerance makes equality a discard
// deterministically.
const floatTol = 1e-9

// Decide is the keep/discard rule.
//
// A candidate must beat the previous score by MORE than epsilon. Equality is a
// discard: an identical score means the change did nothing measurable, and
// keeping it would let the strategy drift on noise.
func Decide(prev, candidate, epsilon float64) (Verdict, string) {
	delta := candidate - prev
	if delta > epsilon+floatTol {
		return Keep, fmt.Sprintf("метрика зросла %.4f → %.4f (+%.4f > ε=%.4f)", prev, candidate, delta, epsilon)
	}
	return Discard, fmt.Sprintf("метрика %.4f → %.4f (Δ=%+.4f ≤ ε=%.4f)", prev, candidate, delta, epsilon)
}

// Engine runs the loop.
type Engine struct {
	Budget   Budget
	Surface  Surface
	Proposer Proposer
	Scorer   Scorer
	// Leaks, when set, refuses a candidate that quotes the eval answers.
	Leaks LeakChecker
	// Log receives each entry. nil discards them (tests), which is why the
	// engine never relies on reading its own log back.
	Log func(Entry) error
	// Now is injectable so tests do not sleep.
	Now func() time.Time
	// ReportOnly measures candidates and never writes the strategy back. It is
	// the L1 mode of the lecture: see what the loop WOULD do.
	ReportOnly bool

	started  bool
	current  string
	baseline Measurement
	used     int
	tokens   int
	deadline time.Time
	sha      string
	// lastMissing carries the facts the current strategy failed to find, so
	// the proposer has something concrete to aim at instead of "do better".
	lastMissing []string
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

// Start loads the current strategy and measures the baseline.
func (e *Engine) Start(ctx context.Context) (Measurement, error) {
	if err := e.Budget.Validate(); err != nil {
		return Measurement{}, err
	}
	if e.Proposer == nil {
		return Measurement{}, ErrNoProposer
	}
	if e.Scorer == nil {
		return Measurement{}, ErrNoScorer
	}
	if e.Surface == nil {
		return Measurement{}, errors.New("improve: немає поверхні змін")
	}
	cur, err := e.Surface.ReadStrategy()
	if err != nil {
		return Measurement{}, fmt.Errorf("improve: читання стратегії: %w", err)
	}
	e.current = string(cur)
	m, err := e.Scorer.Score(ctx, e.current)
	if err != nil {
		return Measurement{}, fmt.Errorf("improve: базова метрика: %w", err)
	}
	e.baseline = m
	e.tokens += m.Tokens
	e.sha = gitSHA()
	if e.Budget.Wall > 0 {
		e.deadline = e.now().Add(e.Budget.Wall)
	}
	e.started = true
	return m, nil
}

// Peek reports why the loop would stop now, or StopRunning.
func (e *Engine) Peek() StopReason {
	switch {
	case e.used >= e.Budget.Experiments:
		return StopBudget
	case e.Budget.Tokens > 0 && e.tokens >= e.Budget.Tokens:
		return StopTokens
	case e.Budget.Wall > 0 && !e.now().Before(e.deadline):
		return StopWall
	}
	return StopRunning
}

// Step runs one experiment.
//
// Order: check the budget BEFORE asking the proposer, because after the call
// the budget has already been spent on a step we decided not to take. Then
// write the candidate, score it, and revert if it lost — the write happens
// before the measurement because the scorer is allowed to read the strategy
// from disk, exactly as the live agent does.
func (e *Engine) Step(ctx context.Context) (Entry, StopReason, error) {
	if !e.started {
		return Entry{}, StopRunning, errors.New("improve: Step до Start")
	}
	if stop := e.Peek(); stop != StopRunning {
		return Entry{}, stop, nil
	}
	start := e.now()
	e.used++

	prev := e.baseline
	missing := e.lastMissing
	p, err := e.Proposer.Propose(ctx, e.current, prev, missing)
	if err != nil {
		return Entry{}, StopProposer, fmt.Errorf("improve: пропонент: %w", err)
	}
	if strings.TrimSpace(p.Content) == "" {
		return Entry{}, StopProposer, fmt.Errorf("improve: пропонент віддав порожню стратегію")
	}

	entry := Entry{
		N: e.used, TS: start.UTC().Format(time.RFC3339),
		PrevMetric: prev.Dev, HeldoutPrev: prev.Heldout,
		DiffSummary: p.Summary, GitSHA: e.sha,
	}

	// Golden-leak check before anything is measured: a candidate that quotes
	// the expected answers would score 1.0 and teach us nothing.
	if e.Leaks != nil {
		if leaked := e.Leaks(p.Content); len(leaked) > 0 {
			entry.Verdict = Discard
			entry.NewMetric = prev.Dev
			entry.HeldoutNew = prev.Heldout
			entry.Reason = fmt.Sprintf("%v: %s", ErrGoldenLeak, strings.Join(leaked, ", "))
			return e.finish(entry, start)
		}
	}

	if !e.ReportOnly {
		if err := e.Surface.WriteStrategy([]byte(p.Content)); err != nil {
			return Entry{}, StopRunning, fmt.Errorf("improve: запис кандидата: %w", err)
		}
	}
	m, err := e.Scorer.Score(ctx, p.Content)
	if err != nil {
		// A failed measurement is not a discard: we do not know whether the
		// candidate was worse. Revert and surface the error.
		if !e.ReportOnly {
			_ = e.Surface.WriteStrategy([]byte(e.current))
		}
		return Entry{}, StopRunning, fmt.Errorf("improve: оцінка кандидата: %w", err)
	}
	e.tokens += m.Tokens

	verdict, reason := Decide(prev.Dev, m.Dev, e.Budget.Epsilon)
	entry.Verdict, entry.Reason = verdict, reason
	entry.NewMetric, entry.HeldoutNew = m.Dev, m.Heldout
	entry.Found, entry.Total, entry.Tokens = m.Found, m.Total, m.Tokens

	if verdict == Keep {
		e.current = p.Content
		e.baseline = m
		e.lastMissing = nil
	} else {
		// Discarded candidates go to experiments/ — the only other mutable
		// entry. A discard that leaves no trace is a discard nobody can audit.
		rel := fmt.Sprintf("experiments/discarded-%02d.md", e.used)
		if werr := e.Surface.Write(rel, []byte(p.Content)); werr == nil {
			entry.DiscardedRel = rel
		}
		if !e.ReportOnly {
			if werr := e.Surface.WriteStrategy([]byte(e.current)); werr != nil {
				return entry, StopRunning, fmt.Errorf("improve: відкат стратегії: %w", werr)
			}
		}
	}
	return e.finish(entry, start)
}

// finish stamps the entry, logs it and reports whether the loop may continue.
func (e *Engine) finish(entry Entry, start time.Time) (Entry, StopReason, error) {
	entry.DurationSec = e.now().Sub(start).Seconds()
	if e.Log != nil {
		if err := e.Log(entry); err != nil {
			return entry, StopRunning, fmt.Errorf("improve: журнал: %w", err)
		}
	}
	return entry, e.Peek(), nil
}

// Run steps until a budget stops it and returns every entry.
func (e *Engine) Run(ctx context.Context) ([]Entry, StopReason, error) {
	if !e.started {
		if _, err := e.Start(ctx); err != nil {
			return nil, StopRunning, err
		}
	}
	var out []Entry
	for {
		entry, stop, err := e.Step(ctx)
		if err != nil {
			return out, stop, err
		}
		if entry.N == 0 { // budget stopped us before the experiment ran
			return out, stop, nil
		}
		out = append(out, entry)
		if stop != StopRunning {
			return out, stop, nil
		}
	}
}

// Baseline is the measurement of the strategy currently kept.
func (e *Engine) Baseline() Measurement { return e.baseline }

// Strategy is the content currently kept.
func (e *Engine) Strategy() string { return e.current }

// Tokens is the total spent so far.
func (e *Engine) Tokens() int { return e.tokens }

// SetMissing tells the engine which facts the current strategy misses, so the
// next proposal has a target. The caller gets them from the scorer.
func (e *Engine) SetMissing(names []string) { e.lastMissing = names }

// AppendJSONL returns a Log function that appends entries to path as JSON
// lines. It writes through the surface, so experiments.log obeys the same
// bounded change surface as everything else.
func AppendJSONL(path string) func(Entry) error {
	return func(entry Entry) error {
		b, err := json.Marshal(entry)
		if err != nil {
			return fmt.Errorf("improve: кодування запису: %w", err)
		}
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return fmt.Errorf("improve: відкриття журналу: %w", err)
		}
		defer f.Close()
		if _, err := f.Write(append(b, '\n')); err != nil {
			return fmt.Errorf("improve: запис у журнал: %w", err)
		}
		return nil
	}
}

// gitSHA returns the current commit, or "unknown" outside a repository. It is
// stamped on every record so a result can be tied to the code that produced it.
func gitSHA() string {
	out, err := exec.Command("git", "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}
