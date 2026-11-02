package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/dimetron/ai-eng-course/labs/week6/Day11_Dynamic_Workflows_Multi_Critic/labs11/internal/manifest"
	"github.com/dimetron/ai-eng-course/labs/week6/Day11_Dynamic_Workflows_Multi_Critic/labs11/internal/usage"
)

// runLoop runs the agent once and reads the outcome back out of the event log.
//
// The counters are derived from the events rather than kept in a struct the
// loop writes to. Same reason as the audit log of ДЗ 9: if the event stream is
// not a sufficient record of what happened, then "the event log shows the
// trajectory" is a sentence and not a property.
func runLoop(ctx context.Context, ag agent.Agent, task manifest.Task) (LoopResult, []*session.Event, error) {
	start := time.Now()
	r, err := runner.New(runner.Config{
		AppName: "labs11", Agent: ag,
		SessionService: session.InMemoryService(), AutoCreateSession: true,
	})
	if err != nil {
		return LoopResult{}, nil, fmt.Errorf("runner: %w", err)
	}
	var events []*session.Event
	var runErr error
	var final string
	msg := genai.NewContentFromText(task.Prompt, genai.RoleUser)
	for ev, e := range r.Run(ctx, "lab", "eval-"+task.ID, msg, agent.RunConfig{}) {
		if e != nil {
			runErr = e
			break
		}
		events = append(events, ev)
		if s, ok := ev.Output.(string); ok && strings.TrimSpace(s) != "" {
			final = s
		}
	}
	res := statsFromEvents(events)
	res.TaskID = task.ID
	res.Accepted = final
	res.Elapsed = time.Since(start)
	res.Err = runErr
	return res, events, runErr
}

// statsFromEvents reconstructs the run from its own log.
func statsFromEvents(events []*session.Event) LoopResult {
	var res LoopResult
	for _, ev := range events {
		for key, v := range ev.Actions.StateDelta {
			if !strings.HasPrefix(key, "w6p1:attempt:") {
				continue
			}
			s := fmt.Sprint(v)
			if n := attemptNumber(key); n > res.Attempts {
				res.Attempts = n
			}
			switch {
			case strings.HasPrefix(s, "gate:") && s != "gate:ok":
				res.GateFailures++
			case strings.HasPrefix(s, "panel:"):
				res.PanelFailures++
			case strings.HasPrefix(s, "verdict_rejected:"):
				res.RejectedVerdicts++
			}
		}
	}
	return res
}

func attemptNumber(key string) int {
	parts := strings.Split(key, ":")
	if len(parts) != 3 {
		return 0
	}
	n := 0
	for _, r := range parts[2] {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}

// EvalRow is one line of the eval report.
type EvalRow struct {
	Task     string
	Accepted bool
	Attempts int
	Elapsed  time.Duration
	Tokens   usage.Tokens
	// HoldoutOK is the result of the hidden rules on the accepted manifest.
	HoldoutOK bool
	// HoldoutFailed names the hidden rules the accepted manifest broke.
	HoldoutFailed []string
	// FalseAccept is the headline number: the loop said yes, the holdout said no.
	FalseAccept bool
	Note        string
}

// runEval runs every task and prints the report.
//
// The table's point is the last column. Iterations and seconds say how hard the
// loop worked; false accepts say whether the work was worth trusting. A panel
// that passes everything quickly and is wrong is worse than one that burns its
// budget honestly.
func runEval(ctx context.Context, out io.Writer, llm model.LLM, attempts int, offline bool, makerKind string) error {
	rows := make([]EvalRow, 0, 3)
	for _, task := range manifest.All() {
		res, _, meter, err := runTask(ctx, task, llm, attempts, offline, makerKind)
		row := EvalRow{
			Task: task.ID, Attempts: res.Attempts, Elapsed: res.Elapsed,
			Tokens: meter.Total(),
		}
		switch {
		case errors.Is(err, ErrBudgetExhausted):
			row.Note = "бюджет вичерпано"
		case errors.Is(err, ErrCriticBroken):
			row.Note = "критик зламаний"
		case err != nil:
			row.Note = "збій: " + firstLine(err.Error())
		default:
			row.Accepted = true
			hold := manifest.Grade(res.Accepted, task.Holdout)
			row.HoldoutOK = hold.OK
			row.HoldoutFailed = hold.Failed
			row.FalseAccept = !hold.OK
			if row.FalseAccept {
				row.Note = "ХИБНЕ ПРИЙНЯТТЯ: " + strings.Join(hold.Failed, ", ")
			}
		}
		rows = append(rows, row)
	}
	printEval(out, rows)
	return nil
}

func printEval(out io.Writer, rows []EvalRow) {
	fmt.Fprintf(out, "\n%-22s %-9s %-6s %-8s %-9s %-9s %s\n",
		"завдання", "прийнято", "спроб", "час", "токени", "holdout", "примітка")
	fmt.Fprintln(out, strings.Repeat("-", 100))
	falseAccepts := 0
	for _, r := range rows {
		if r.FalseAccept {
			falseAccepts++
		}
		fmt.Fprintf(out, "%-22s %-9s %-6d %-8s %-9d %-9s %s\n",
			r.Task, yesNo(r.Accepted), r.Attempts, r.Elapsed.Round(time.Millisecond),
			r.Tokens.Total(), holdoutCell(r), r.Note)
	}
	fmt.Fprintln(out, strings.Repeat("-", 100))
	fmt.Fprintf(out, "ХИБНИХ ПРИЙНЯТТІВ: %d із %d\n", falseAccepts, len(rows))
	if falseAccepts > 0 {
		fmt.Fprintln(out, "Цикл прийняв маніфест, який не пройшов приховані правила — тобто панель")
		fmt.Fprintln(out, "перевірила все, що їй показали, і цього виявилось недостатньо.")
	}
}

func holdoutCell(r EvalRow) string {
	if !r.Accepted {
		return "—"
	}
	if r.HoldoutOK {
		return "пройдено"
	}
	return "ПРОВАЛ"
}

func yesNo(b bool) string {
	if b {
		return "так"
	}
	return "ні"
}

func firstLine(s string) string { return strings.SplitN(s, "\n", 2)[0] }
