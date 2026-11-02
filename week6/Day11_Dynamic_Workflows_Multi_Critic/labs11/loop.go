package main

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/workflowagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/workflow"

	"github.com/dimetron/ai-eng-course/labs/week6/Day11_Dynamic_Workflows_Multi_Critic/labs11/internal/manifest"
)

// ErrBudgetExhausted ends a run that did not converge. It is a sentinel so the
// eval harness can tell "did not converge" from "crashed", which are different
// numbers in the report.
var ErrBudgetExhausted = errors.New("бюджет спроб вичерпано")

// ErrCriticBroken ends a run whose critic produced an invalid verdict twice.
//
// One malformed verdict is treated as a refusal and the loop continues: a
// flaky critic should not kill a run that could still converge. The SECOND
// one from the same role is a different fact — the critic is broken, not the
// draft — and continuing would burn the whole budget on it.
var ErrCriticBroken = errors.New("критик двічі віддав невалідний вердикт")

// attemptKey is the state key of one attempt's gate decision. The prefix is
// the lab's own namespace so a reader of the event log can grep for it.
func attemptKey(n int) string { return fmt.Sprintf("w6p1:attempt:%d", n) }

// Draft is the maker's work plus the comments it must address.
type Draft struct {
	Task     string   `json:"task"`
	Spec     string   `json:"spec"`
	Attempt  int      `json:"attempt"`
	Code     string   `json:"code,omitempty"`
	Feedback []string `json:"feedback,omitempty"`
}

// LoopResult is everything one run produced, for the eval table.
type LoopResult struct {
	TaskID string
	// Accepted is the manifest the loop accepted, empty when it did not.
	Accepted string
	Attempts int
	Elapsed  time.Duration
	// GateFailures counts attempts rejected by the objective validator.
	GateFailures int
	// PanelFailures counts attempts rejected by the critic panel.
	PanelFailures int
	// RejectedVerdicts counts verdicts thrown out by validation.
	RejectedVerdicts int
	Err              error
}

// Engine builds the multi-critic loop for one task.
type Engine struct {
	Task        manifest.Task
	MaxAttempts int
	// MakerModel writes and fixes the manifest.
	MakerModel model.LLM
	// CriticModel returns the model of one critic.
	CriticModel func(critic) model.LLM
}

// Agent assembles the graph: one dynamic node holding the loop, with the maker
// and the critic panel as child nodes run from inside it.
//
// The loop is dynamic rather than a static cycle because the number of
// iterations is a run-time fact. The guard rails are mine: maxAttempts is
// checked BEFORE the maker is called, so the last iteration cannot pay for a
// step we already decided not to take.
func (e Engine) Agent(makerNode workflow.Node, panelNode workflow.Node, roles []string) (agent.Agent, error) {
	loop := workflow.NewDynamicNode[string, string]("multi_critic_loop",
		func(ctx agent.Context, _ string, emit func(*session.Event) error) (string, error) {
			res, err := e.run(ctx, emit, makerNode, panelNode, roles)
			if err != nil {
				return "", err
			}
			return res, nil
		},
		workflow.NodeConfig{Timeout: e.loopTimeout()},
	)
	return workflowagent.New(workflowagent.Config{
		Name:        "multi_critic_manifest_agent",
		Description: "Цикл maker → валідатор → панель критиків над Kubernetes-маніфестами.",
		Edges:       workflow.Chain(workflow.Start, loop),
	})
}

// loopTimeout bounds the whole loop, and is larger than the sum of what runs
// inside it. A per-node timeout that is smaller than its children's turns a
// slow provider into a confusing partial failure.
func (e Engine) loopTimeout() time.Duration {
	return time.Duration(e.attempts()) * 90 * time.Second
}

func (e Engine) attempts() int {
	if e.MaxAttempts > 0 {
		return e.MaxAttempts
	}
	return 4
}

// run is the maker/checker loop.
func (e Engine) run(ctx agent.Context, emit func(*session.Event) error, makerNode, panelNode workflow.Node, roles []string) (string, error) {
	d := Draft{Task: e.Task.Prompt, Spec: e.Task.Spec()}
	broken := map[string]int{}

	for attempt := 1; ; attempt++ {
		// The cap is checked BEFORE the maker runs: after the call, the budget
		// has already been spent on a step we decided not to take.
		if attempt > e.attempts() {
			e.note(ctx, emit, "budget_exhausted",
				fmt.Sprintf("%d спроб без проходження", e.attempts()), nil)
			return "", fmt.Errorf("%w: %d спроб", ErrBudgetExhausted, e.attempts())
		}
		d.Attempt = attempt

		out, err := workflow.RunNode[string](ctx, makerNode, d, workflow.WithUseSubBranch())
		if err != nil {
			return "", fmt.Errorf("maker (спроба %d): %w", attempt, err)
		}
		d.Code = stripFence(out)

		// The objective critic runs FIRST, and that order is the point. A
		// manifest that does not parse cannot be judged on its privileges, and
		// paying three models to describe the security posture of a file with a
		// YAML error is the cheapest way to waste a budget.
		rep := manifest.Grade(d.Code, e.Task.Visible)
		if !rep.OK {
			e.note(ctx, emit, "gate_failed", rep.Stage, map[string]any{
				attemptKey(attempt): "gate:" + rep.Stage,
			})
			// Raw validator output is the best feedback there is: the maker
			// fixes what the tool said, not someone's retelling of it.
			d.Feedback = []string{"[validator] " + rep.Output}
			continue
		}
		e.note(ctx, emit, "gate_ok", "схема й видимі правила пройдені", map[string]any{
			attemptKey(attempt): "gate:ok",
		})

		joined, err := workflow.RunNode[map[string]any](ctx, panelNode, d, workflow.WithUseSubBranch())
		if err != nil {
			return "", fmt.Errorf("панель критиків (спроба %d): %w", attempt, err)
		}
		agg := aggregate(joined, roles)

		for _, v := range agg.Verdicts {
			e.note(ctx, emit, "verdict", fmt.Sprintf("%s passed=%t comments=%d", v.Critic, v.Passed, len(v.Comments)), nil)
		}
		for _, r := range agg.Rejected {
			role := strings.SplitN(r, ":", 2)[0]
			broken[role]++
			e.note(ctx, emit, "verdict_rejected", r, map[string]any{
				attemptKey(attempt): "verdict_rejected:" + role,
			})
			if broken[role] >= 2 {
				return "", fmt.Errorf("%w: %s", ErrCriticBroken, role)
			}
		}

		if agg.Passed {
			e.note(ctx, emit, "loop_done", fmt.Sprintf("прийнято на спробі %d: %s", attempt, agg.Summary()), map[string]any{
				attemptKey(attempt): "accepted",
			})
			return d.Code, nil
		}
		e.note(ctx, emit, "panel_failed", agg.Summary(), map[string]any{
			attemptKey(attempt): "panel:" + agg.Summary(),
		})
		d.Feedback = agg.Feedback
	}
}

// note emits one event: readable text for a human, StateDelta for a grep.
//
// It never sets ev.Output. A node execution may carry exactly one output, and
// this node's output is its return value — an emitted event with Output set
// fails the run with "only one event per execution can carry output".
func (e Engine) note(ctx agent.Context, emit func(*session.Event) error, kind, detail string, delta map[string]any) {
	if emit == nil {
		return
	}
	ev := session.NewEvent(ctx, ctx.InvocationID())
	ev.Author = "multi_critic_loop"
	ev.LLMResponse.Content = textContent(fmt.Sprintf("[%s] %s", kind, detail))
	if len(delta) > 0 {
		ev.Actions.StateDelta = delta
	}
	// An emit failure is bookkeeping, not the answer: losing a log line must
	// not fail a run that otherwise converged.
	_ = emit(ev)
}
