package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/workflowagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/workflow"

	"github.com/dimetron/ai-eng-course/labs/week5/Day10_Collaboration_Agents_HITL/labs10/internal/approvals"
)

// branchOut is what each specialist branch returns: its section AND the plan
// it worked from.
//
// The plan rides along because a node after a JoinNode receives
// map[predecessor]output and NOTHING from before the fan-out. Session state
// was the first thing I tried and it does not survive: a write from a graph
// node is not visible to the gate when it resumes in another process. So the
// context travels inside the branch output, which is exactly what the ADK
// workflow notes recommend. Three copies of a small struct is the price.
type branchOut struct {
	Plan    Plan    `json:"plan"`
	Section Section `json:"section"`
}

// Dossier is the merged artifact plus the plan that produced it. Both the gate
// and the send node need the plan (recipients, deadline), so it stays attached
// to the thing travelling down the graph rather than living in state.
type Dossier struct {
	Plan   Plan   `json:"plan"`
	Report Report `json:"report"`
}

// Engine builds the collaborative graph.
type Engine struct {
	Plan  Plan
	Store *approvals.Store
	Desks []desk
	// ModelFor returns the model for one desk. Offline it is a fresh scripted
	// fake per desk; live it is the provider's model.
	ModelFor func(desk) model.LLM
	// Window is how long silence is tolerated at the gate.
	Window time.Duration
	// Role is who this run acts for. The zero value is RoleCoordinator so an
	// engine built without a role keeps working; the CLI always sets it.
	Role Role
}

// role returns the effective role of the run.
func (e Engine) role() Role {
	if e.Role == "" {
		return RoleCoordinator
	}
	return e.Role
}

// Gated is what the gate hands the send node: the artifact plus the decision
// that was actually recorded in the ledger.
type Gated struct {
	Dossier   Dossier `json:"dossier"`
	Status    string  `json:"status"`
	DecidedBy string  `json:"decided_by"`
	Reason    string  `json:"reason"`
	// Interrupt is the ledger key, needed to spend the approval.
	Interrupt string `json:"interrupt"`
}

// Agent assembles the graph:
//
//	Start → plan → ⎡risk_desk      ⎤
//	               ⎢compliance_desk⎥ → join_desks → merge → gate → send
//	               ⎣integration_desk⎦
//
// The gate sits on the sequential path after the join, not inside a branch:
// ADK returns ErrParallelHITLUnsupported for a human-input request raised from
// concurrent branches, and rightly so — there is no sensible way to pause three
// branches on one person.
func (e Engine) Agent() (agent.Agent, error) {
	planNode := workflow.NewFunctionNode("plan", e.planFn, workflow.NodeConfig{})

	branches := make([]workflow.Node, 0, len(e.Desks))
	for _, d := range e.Desks {
		node, err := e.branch(d)
		if err != nil {
			return nil, err
		}
		branches = append(branches, node)
	}

	join := workflow.NewJoinNode("join_desks")
	merge := workflow.NewFunctionNode("merge", e.mergeFn, workflow.NodeConfig{})

	// &true: re-run the gate from the top when the human answers, so the code
	// after ResumeOrRequestInput runs. With nil or &false the reply is handed
	// to the SUCCESSOR and the gate never finishes its own body — the run
	// would resume and quietly send without reading the ledger.
	rerun := true
	gate := workflow.NewEmittingFunctionNode("gate", e.gateFn, workflow.NodeConfig{RerunOnResume: &rerun})
	send := workflow.NewFunctionNode("send", e.sendFn, workflow.NodeConfig{})

	b := workflow.NewEdgeBuilder()
	b.Add(workflow.Start, planNode)
	b.AddFanOut(planNode, branches...)
	b.AddFanIn(join, branches...)
	b.Add(join, merge)
	b.Add(merge, gate)
	b.Add(gate, send)

	return workflowagent.New(workflowagent.Config{
		Name:        "collaborative_workflow",
		Description: "Координатор, три спеціалісти й затвор схвалення.",
		Edges:       b.Build(),
	})
}

// planFn is the coordinator's decision before anyone works: what is asked, of
// whom, in which order, and for whom the artifact is.
func (e Engine) planFn(_ agent.Context, request string) (Plan, error) {
	p := e.Plan
	if s := strings.TrimSpace(request); s != "" {
		p.Topic = s
	}
	if len(p.Order) == 0 {
		p.Order = make([]string, 0, len(e.Desks))
		for _, d := range e.Desks {
			p.Order = append(p.Order, d.Name)
		}
	}
	return p, nil
}

// branch wraps one desk in a dynamic node.
//
// Dynamic rather than a plain AgentNode because the section needs the desk's
// EVIDENCE, and an agent node's output is the model's text. The findings come
// straight from the source; the model's prose is attached beside them as
// Summary. Evidence that has passed through a model is no longer evidence —
// and the human in this lab approves on the evidence.
func (e Engine) branch(d desk) (workflow.Node, error) {
	a, err := d.Agent(e.ModelFor(d), e.role())
	if err != nil {
		return nil, err
	}
	child, err := workflow.NewAgentNode(a, workflow.NodeConfig{})
	if err != nil {
		return nil, fmt.Errorf("%s: вузол агента: %w", d.Name, err)
	}
	return workflow.NewDynamicNode(d.Name,
		func(ctx agent.Context, in Plan, _ func(*session.Event) error) (branchOut, error) {
			sec := Section{Desk: d.Name, Title: d.Title}

			// Authorization is checked HERE, not only in the callback: this
			// path reads the source directly, so a policy enforced only on the
			// model's tool call would leave the data in the document anyway.
			//
			// And it is checked BEFORE the read, not after. Fetching and then
			// discarding would mean the data was already read — the first
			// version of this did exactly that, which is not an authorization
			// check but a filter on the way out.
			var findings []Finding
			err := error(nil)
			if !may(e.role(), d.ToolName) {
				err = policyError(e.role(), d.ToolName)
			} else {
				findings, err = d.Fetch()
			}
			if err != nil {
				// Containment: the section becomes a named gap and the model is
				// NOT asked. Asking it here is how a dead source turns into a
				// plausible paragraph — we would be paying a model to invent
				// the thing we failed to look up.
				// Code and reason are kept apart: the code routes the
				// on-call to a system, the reason is for the human reading the
				// evidence. Putting err.Error() in Gap printed the code twice.
				code, why := "DESK_UNAVAILABLE", err.Error()
				var de *deskError
				if errors.As(err, &de) {
					code, why = de.Code, de.Why
				}
				sec.Status, sec.Code, sec.Gap = StatusUnverified, code, why
				return branchOut{Plan: in, Section: sec}, nil
			}

			sec.Status, sec.Findings = StatusVerified, findings
			prose, err := workflow.RunNode[string](ctx, child, promptFor(d, in, findings))
			if err != nil {
				// The source answered; only the prose failed. The section keeps
				// its evidence and says the summary is missing — losing a
				// verified finding because a sentence could not be written
				// would be the worse trade.
				sec.Summary = fmt.Sprintf("(підсумок не склався: %v — факти нижче незмінні)", err)
				return branchOut{Plan: in, Section: sec}, nil
			}
			sec.Summary = strings.TrimSpace(prose)
			return branchOut{Plan: in, Section: sec}, nil
		}, workflow.NodeConfig{}), nil
}

// promptFor is what the specialist is asked. The findings are included so a
// live model has them in context; it is still instructed to call its tool.
func promptFor(d desk, p Plan, findings []Finding) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Тема: %s. Напрям: %s.\n", p.Topic, d.Title)
	fmt.Fprintf(&b, "Виклич %s і перекажи його факти з джерелами.\n", d.ToolName)
	for _, f := range findings {
		fmt.Fprintf(&b, "- %s (%s)\n", f.Claim, f.Source)
	}
	return b.String()
}

// mergeFn turns the join map into the report.
//
// Two rules live here. The order comes from the stored plan, never from
// ranging over the map — Go randomizes that, and a document whose sections
// move cannot be hashed or diffed. And a desk that is in the plan but missing
// from the join map becomes a gap, not a shorter document.
func (e Engine) mergeFn(_ agent.Context, joined map[string]any) (Dossier, error) {
	// Join values are `any`: whatever each branch emitted, round-tripped
	// through the event log. Converting through JSON is the documented way; a
	// type assertion to branchOut fails here.
	outs := make(map[string]branchOut, len(joined))
	var plan Plan
	for name, raw := range joined {
		var bo branchOut
		if err := remarshal(raw, &bo); err != nil {
			outs[name] = branchOut{Section: Section{
				Desk: name, Status: StatusUnverified, Code: "DESK_UNREADABLE",
				Gap: fmt.Sprintf("розділ не розбирається: %v", err),
			}}
			continue
		}
		if len(bo.Plan.Order) > 0 {
			plan = bo.Plan
		}
		outs[name] = bo
	}
	if len(plan.Order) == 0 {
		// Every branch failed to report a plan. Falling back to the engine's
		// configured order keeps the document deterministic; it is named here
		// so a reader knows the order did not come from this run.
		plan = e.Plan
		plan.Order = deskNames(e.Desks)
	}

	rep := Report{Topic: plan.Topic}
	for _, name := range plan.Order {
		bo, ok := outs[name]
		if !ok {
			// A desk in the plan that never reported becomes a gap, not a
			// shorter document. A missing section is information.
			rep.Sections = append(rep.Sections, Section{
				Desk: name, Status: StatusUnverified, Code: "DESK_MISSING",
				Gap: "гілка не відзвітувала у join",
			})
			continue
		}
		sec := bo.Section
		if sec.Desk == "" {
			sec.Desk = name
		}
		rep.Sections = append(rep.Sections, sec)
	}
	return Dossier{Plan: plan, Report: rep}, nil
}

// deskNames is the configured order, used only as a named fallback.
func deskNames(ds []desk) []string {
	out := make([]string, 0, len(ds))
	for _, d := range ds {
		out = append(out, d.Name)
	}
	return out
}

// gateFn is the approval gate.
//
// Order matters: the request is written to the ledger BEFORE the pause, so a
// crash between "asked" and "answered" leaves a durable row with a deadline
// instead of a question nobody remembers. Ask is idempotent on InterruptID,
// which is what makes a node that runs twice safe here.
func (e Engine) gateFn(ctx agent.Context, d Dossier, emit func(*session.Event) error) (Gated, error) {
	p, rep := d.Plan, d.Report
	id := "approval-" + ctx.InvocationID()

	if _, err := e.Store.Ask(ctx, approvals.Request{
		InterruptID: id,
		SessionID:   ctx.SessionID(),
		Action:      "send_migration_plan",
		ArtifactURI: "report://" + p.Topic,
		Hash:        rep.Hash(),
		Recipients:  p.Recipients,
		Facts:       rep.Facts(p),
		Window:      e.Window,
	}); err != nil {
		return Gated{}, fmt.Errorf("gate: записати запит: %w", err)
	}

	// Everything above this line runs twice (once before the pause, once on
	// resume) and must be safe to repeat: Ask is a no-op the second time and
	// does not reset the deadline.
	if _, err := workflow.ResumeOrRequestInput(ctx, emit, session.RequestInput{
		InterruptID: id,
		Message:     askText(rep, p, id),
		Payload:     rep.Facts(p),
	}); err != nil {
		return Gated{}, err // first pass: ErrNodeInterrupted → the run parks
	}

	// The reply that unparked the run is NOT the decision. It is only the
	// signal that someone answered somewhere. The decision of record is the
	// ledger row: it is written by the `decide` command with a named human, it
	// cannot be written twice, and the timeout sweep can write it instead. A
	// forged or garbled reply in the session therefore cannot approve anything.
	rec, err := e.Store.Get(ctx, id)
	if err != nil {
		return Gated{}, fmt.Errorf("gate: прочитати рішення: %w", err)
	}
	return Gated{
		Dossier: d, Status: string(rec.Status),
		DecidedBy: rec.DecidedBy, Reason: rec.Reason, Interrupt: id,
	}, nil
}

// askText is the question the human sees: facts, not prose.
//
// Action, recipients, hash, per-desk status, gaps and sources. No sentence
// from the coordinator assuring anyone that it all looks fine.
func askText(rep Report, p Plan, id string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "СХВАЛЕННЯ ПОТРІБНЕ (%s)\n", id)
	fmt.Fprintf(&b, "дія:         надіслати план міграції раді директорів\n")
	fmt.Fprintf(&b, "отримувачі:  %s\n", strings.Join(p.Recipients, ", "))
	fmt.Fprintf(&b, "дедлайн:     %s\n", p.Deadline)
	fmt.Fprintf(&b, "хеш:         %s\n", rep.Hash())
	fmt.Fprintf(&b, "розділи:     %s\n", strings.Join(sectionStates(rep), " "))
	for _, g := range rep.Gaps() {
		fmt.Fprintf(&b, "ПРОГАЛИНА:   %s — %s (%s)\n", g.Desk, g.Gap, g.Code)
	}
	fmt.Fprintf(&b, "джерела:     %s\n", strings.Join(rep.Sources(), ", "))
	if note := blockedNote(rep); note != "" {
		fmt.Fprintf(&b, "%s\n", note)
	}
	fmt.Fprintf(&b, "\n%s", rep.Render())
	return b.String()
}

// sendFn is the side effect, and it is the last place that can stop it.
//
// Two independent gates, each with its own reason, because they fail for
// different causes: a human said no (or nobody did), and the artifact is not
// verifiable. Either alone is enough to refuse.
func (e Engine) sendFn(ctx agent.Context, g Gated) (string, error) {
	rep, p := g.Dossier.Report, g.Dossier.Plan
	if g.Status != string(approvals.Approved) {
		return fmt.Sprintf("НЕ надіслано: %s (%s) — %s",
			g.Status, orUnknown(g.DecidedBy), orUnknown(g.Reason)), nil
	}
	if !rep.Verified() {
		gaps := make([]string, 0, len(rep.Sections))
		for _, s := range rep.Gaps() {
			gaps = append(gaps, s.Desk+"="+s.Code)
		}
		// Approved, and still not sent. An unverified section is not a
		// formality a signature can cover: nobody can approve a claim that has
		// no source behind it, because there is no claim yet.
		return "НЕ надіслано: схвалено, але розділи неперевірені: " + strings.Join(gaps, ", "), nil
	}

	// Spend re-checks the hash and the recipient list and marks the approval
	// used, in one conditional UPDATE. Whatever the graph does — a re-run after
	// resume, two processes racing — the document goes out at most once, and
	// only the document that was approved.
	if err := e.Store.Spend(ctx, g.Interrupt, rep.Hash(), p.Recipients); err != nil {
		return fmt.Sprintf("НЕ надіслано: %v", err), nil
	}
	return notifyBoard(rep, p), nil
}

// notifyBoard is the fixture for the outward-facing action.
func notifyBoard(rep Report, p Plan) string {
	return fmt.Sprintf("надіслано %d розділів до %s (хеш %s)",
		len(rep.Sections), strings.Join(p.Recipients, ", "), rep.Hash()[:12])
}

func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

// remarshal moves a value through JSON into dst. It is how `any` coming out of
// a join map or session state becomes a typed struct.
func remarshal(src any, dst any) error {
	b, err := json.Marshal(src)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, dst)
}
