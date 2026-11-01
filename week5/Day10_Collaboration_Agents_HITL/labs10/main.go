// Стартовий шаблон ДЗ 10 — «Collaborative Workflow Engine» команди LEDGERWORKS.
// Станом на ADK Go v2.6.0 (10/2026); перевірте актуальність API.
//
// Ранній win (≤15 хв) — один спеціаліст з ОДНИМ інструментом і справжньою моделлю:
//
//	go run .
//
// Модель обирає internal/modelcfg (apps/.env, MODEL, DEFAULT_MODEL_PROVIDER).
// Без провайдера програма працює на ОФЛАЙН-ФЕЙКУ й каже про це першим рядком —
// «працює» без моделі не те саме, що «працює з моделлю».
//
// Команда й затвор схвалення (два процеси):
//
//	go run . start  -session s1 "Міграція з legacy-процесора"
//	go run . pending
//	go run . decide -id approval-… -by oksana approve
//	go run . audit
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/adk/v2/workflow"
	"google.golang.org/genai"

	"github.com/dimetron/ai-eng-course/labs/internal/fakellm"
	"github.com/dimetron/ai-eng-course/labs/internal/labrun"
	"github.com/dimetron/ai-eng-course/labs/internal/modelcfg"
	"github.com/dimetron/ai-eng-course/labs/week5/Day10_Collaboration_Agents_HITL/labs10/internal/approvals"
)

// AppName scopes the sessions stored next to the approval ledger.
const AppName = "ledgerworks-board"

// user is the person who asks for the plan.
const user = "iryna"

// Finding — один факт із ідентифікатором джерела. Спеціаліст має право
// говорити лише те, що повернув інструмент.
type Finding struct {
	Claim  string `json:"claim"`
	Source string `json:"source"`
}

// riskInput / riskOutput / lookupRisk — контракт раннього win. Вони лишилися
// окремо від deskInput/deskOutput, бо на них стоїть тест стартера, і той тест
// описує саме ранній win: один інструмент, кожен факт із джерелом.
type riskInput struct {
	Topic string `json:"topic" jsonschema:"Тема, за якою потрібні факти"`
}

type riskOutput struct {
	Findings []Finding `json:"findings"`
}

// lookupRisk — фікстура бази інцидентів LEDGERWORKS. Справжня система
// ходила б у incident-db; форма (факт + джерело) та сама.
func lookupRisk(_ agent.Context, _ riskInput) (riskOutput, error) {
	f, err := fetchRisk()
	if err != nil {
		return riskOutput{}, err
	}
	return riskOutput{Findings: f}, nil
}

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "помилка:", err)
		os.Exit(1)
	}
}

// opts is every flag of every subcommand, in one place.
//
// It exists because flags must work on BOTH sides of the verb. flag.Parse
// stops at the first non-flag argument, so a single pass either loses
// `-offline start …` (the verb is never seen) or `start -session s1` (the flag
// is never parsed). Parsing twice over the same struct — the second
// registration taking the first pass's values as its defaults — handles both.
type opts struct {
	db      string
	sid     string
	failing string
	window  time.Duration
	id      string
	by      string
	offline bool
	trace   bool
	role    string
}

func (o *opts) register(fs *flag.FlagSet) {
	fs.StringVar(&o.db, "db", o.db, "SQLite-файл сесій і журналу схвалень")
	fs.StringVar(&o.sid, "session", o.sid, "ідентифікатор сесії")
	fs.StringVar(&o.failing, "fail", o.failing, "імена столів, чиє джерело не відповідає (через кому)")
	fs.DurationVar(&o.window, "window", o.window, "вікно рішення: мовчання після нього — відмова")
	fs.StringVar(&o.id, "id", o.id, "InterruptID запиту (для decide)")
	fs.StringVar(&o.by, "by", o.by, "хто ухвалює рішення (для decide)")
	fs.BoolVar(&o.offline, "offline", o.offline, "не шукати провайдера, використати скриптований фейк")
	fs.BoolVar(&o.trace, "trace", o.trace, "друкувати потік подій: хто що викликав і де пауза")
	fs.StringVar(&o.role, "role", o.role, "роль викликача: coordinator | analyst | guest")
}

func run(ctx context.Context, args []string, out io.Writer) error {
	o := opts{db: ".data/approvals.db", sid: "s1", window: 2 * time.Minute}

	fs := flag.NewFlagSet("labs10", flag.ContinueOnError)
	fs.SetOutput(out)
	o.register(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()

	verb := ""
	if len(rest) > 0 {
		verb, rest = rest[0], rest[1:]
		after := flag.NewFlagSet("labs10 "+verb, flag.ContinueOnError)
		after.SetOutput(out)
		o.register(after)
		if err := after.Parse(rest); err != nil {
			return err
		}
		rest = after.Args()
	}

	if verb == "" {
		return earlyWin(ctx, out, o.offline)
	}

	store, err := approvals.Open(o.db)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	switch verb {
	case "start":
		return cmdStart(ctx, out, store, startOpts{
			sessionID: o.sid, topic: strings.Join(rest, " "),
			failing: parseList(o.failing), window: o.window, offline: o.offline, trace: o.trace,
			role: Role(o.role),
		})
	case "decide":
		return cmdDecide(ctx, out, store, o.sid, o.id, o.by, strings.Join(rest, " "), o.offline)
	case "pending":
		return cmdPending(ctx, out, store)
	case "expire":
		return cmdExpire(ctx, out, store, o.sid, o.offline)
	case "audit":
		return cmdAudit(ctx, out, store)
	default:
		return fmt.Errorf("невідома команда %q: start | decide | pending | expire | audit", verb)
	}
}

// earlyWin is `go run .`: one specialist, one tool, one answer.
func earlyWin(ctx context.Context, out io.Writer, offline bool) error {
	m, label := resolveModel(ctx, offline)
	fmt.Fprintln(out, label)
	if m == nil {
		// No provider: the early win still has to produce the two lines it
		// promises, so it gets its own script here.
		m = fakellm.New("offline-risk",
			fakellm.CallTurn("lookup_risk_desk", map[string]any{"topic": "міграція"}),
			fakellm.TextTurn("Cutover 2025-03 відкотили за 41 хв (incident-db:INC-418); ранбук відкоту давно не перевіряли (runbook:cutover-rollback)."),
		)
	}

	riskTool, err := functiontool.New(functiontool.Config{
		Name:        "lookup_risk_desk",
		Description: "Факти про ризики міграції з бази інцидентів.",
	}, lookupRisk)
	if err != nil {
		return err
	}
	riskDesk, err := llmagent.New(llmagent.Config{
		Name:        "risk_desk_agent",
		Model:       m,
		Description: "Спеціаліст з ризиків міграції.",
		Instruction: "Ти — спеціаліст з ризиків міграції у LEDGERWORKS.\n" +
			"1. ОБОВ'ЯЗКОВО спершу виклич lookup_risk_desk. Без його відповіді не пиши нічого.\n" +
			"2. Говори ЛИШЕ те, що повернув інструмент.\n" +
			"3. Кожен факт — із джерелом у дужках: «твердження (джерело)».\n" +
			"Відповідай українською, не більше трьох речень.",
		Tools: []tool.Tool{riskTool},
	})
	if err != nil {
		return err
	}

	res, err := labrun.Run(ctx, riskDesk, "Ризики міграції з legacy-процесора")
	if err != nil {
		return fmt.Errorf("ран: %w", err)
	}
	fmt.Fprintln(out, "інструменти:", res.ToolCalls)
	fmt.Fprintln(out, "відповідь:  ", res.Final)
	return nil
}

type startOpts struct {
	sessionID string
	topic     string
	failing   map[string]bool
	window    time.Duration
	offline   bool
	trace     bool
	role      Role
}

// cmdStart runs the graph until it parks at the gate.
func cmdStart(ctx context.Context, out io.Writer, store *approvals.Store, o startOpts) error {
	eng, label, err := buildEngine(ctx, store, o.failing, o.window, o.offline)
	eng.Role = o.role
	if err != nil {
		return err
	}
	fmt.Fprintln(out, label)
	ag, err := eng.Agent()
	if err != nil {
		return err
	}
	topic := o.topic
	if strings.TrimSpace(topic) == "" {
		topic = eng.Plan.Topic
	}
	t, err := turn(ctx, ag, store.Sessions(), o.sessionID, genai.NewContentFromText(topic, genai.RoleUser))
	if err != nil {
		return err
	}
	if o.trace {
		printTrace(out, t)
	}
	report(out, t)
	if t.pending == nil {
		fmt.Fprintln(out, "ран завершився без паузи")
		return nil
	}
	fmt.Fprintf(out, "\n%s\n", humanPrompt(t.pending))
	fmt.Fprintf(out, "\n⏸  ран припаркований. Рішення — в ІНШОМУ процесі:\n")
	fmt.Fprintf(out, "   go run . decide -session %s -id %s -by oksana approve\n", o.sessionID, t.pending.ID)
	return nil
}

// cmdDecide writes the human decision to the ledger and then unparks the run.
//
// The order is not cosmetic. The ledger is written first, so the decision
// exists even if the resume fails; and the gate reads the ledger rather than
// the reply, so an unparked run cannot act on anything but a recorded decision.
func cmdDecide(ctx context.Context, out io.Writer, store *approvals.Store, sid, id, by, answer string, offline bool) error {
	if id == "" {
		return errors.New("decide: потрібен -id (подивіться `pending`)")
	}
	if strings.TrimSpace(by) == "" {
		return errors.New("decide: потрібен -by: рішення без імені не є рішенням")
	}
	d, err := parseAnswer(by, answer)
	if err != nil {
		return err
	}
	rec, err := store.Decide(ctx, id, d)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "журнал: %s → %s (by %s) %s\n", rec.InterruptID, rec.Status, rec.DecidedBy, rec.Reason)

	eng, label, err := buildEngine(ctx, store, nil, 0, offline)
	if err != nil {
		return err
	}
	fmt.Fprintln(out, label)
	ag, err := eng.Agent()
	if err != nil {
		return err
	}
	t, err := turn(ctx, ag, store.Sessions(), sid, resumeMessage(id, answer))
	if err != nil {
		return err
	}
	report(out, t)
	return nil
}

// parseAnswer maps a reply to a decision. Three distinct records, one outcome:
// a human refusing, a human never arriving and a UI sending nonsense are
// different events in the ledger even though none of them sends the document.
func parseAnswer(by, answer string) (approvals.Decision, error) {
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "approve", "approved", "yes", "так":
		return approvals.Decision{Status: approvals.Approved, By: by, Reason: "схвалено людиною"}, nil
	case "deny", "denied", "no", "ні":
		return approvals.Decision{Status: approvals.Denied, By: by, Reason: "людина відмовила"}, nil
	case "":
		return approvals.Decision{}, errors.New("decide: потрібна відповідь: approve | deny")
	default:
		// Not an error: an unreadable reply is a recorded, named denial. A UI
		// that sends garbage must not be able to make the run hang, and must
		// not be mistaken for a human who said yes.
		return approvals.Decision{
			Status: approvals.Invalid, By: by,
			Reason: fmt.Sprintf("нерозпізнана відповідь %q", answer),
		}, nil
	}
}

// cmdExpire sweeps overdue requests and unparks their runs with the denial.
func cmdExpire(ctx context.Context, out io.Writer, store *approvals.Store, sid string, offline bool) error {
	expired, err := store.Expire(ctx)
	if err != nil {
		return err
	}
	if len(expired) == 0 {
		fmt.Fprintln(out, "прострочених запитів немає")
		return nil
	}
	eng, _, err := buildEngine(ctx, store, nil, 0, offline)
	if err != nil {
		return err
	}
	ag, err := eng.Agent()
	if err != nil {
		return err
	}
	for _, rec := range expired {
		fmt.Fprintf(out, "журнал: %s → %s (%s)\n", rec.InterruptID, rec.Status, rec.Reason)
		session := rec.SessionID
		if session == "" {
			session = sid
		}
		t, err := turn(ctx, ag, store.Sessions(), session, resumeMessage(rec.InterruptID, "timeout"))
		if err != nil {
			fmt.Fprintf(out, "  не вдалося розпаркувати: %v\n", err)
			continue
		}
		report(out, t)
	}
	return nil
}

func cmdPending(ctx context.Context, out io.Writer, store *approvals.Store) error {
	list, err := store.PendingList(ctx)
	if err != nil {
		return err
	}
	if len(list) == 0 {
		fmt.Fprintln(out, "немає запитів, що чекають")
		return nil
	}
	for _, r := range list {
		fmt.Fprintf(out, "%s  сесія=%s  дія=%s  до %s\n",
			r.InterruptID, r.SessionID, r.Action, r.ExpiresAt.Format(time.RFC3339))
		fmt.Fprintf(out, "   отримувачі: %s\n   хеш: %s\n", strings.Join(r.RecipientList(), ", "), r.Hash)
	}
	return nil
}

func cmdAudit(ctx context.Context, out io.Writer, store *approvals.Store) error {
	rows, err := store.All(ctx)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		fmt.Fprintln(out, "журнал порожній")
		return nil
	}
	for _, r := range rows {
		decided := "—"
		if r.DecidedAt != nil {
			decided = r.DecidedAt.Format("15:04:05")
		}
		consumed := "ні"
		if r.ConsumedAt != nil {
			consumed = r.ConsumedAt.Format("15:04:05")
		}
		fmt.Fprintf(out, "%-10s %-9s by %-14s %s  використано: %s\n",
			r.Status, decided, orUnknown(r.DecidedBy), r.InterruptID, consumed)
		fmt.Fprintf(out, "   причина: %s\n", orUnknown(r.Reason))
	}
	return nil
}

// --- ран і пауза ------------------------------------------------------------

// turnResult is one pass of the runner: what was said, what the nodes output,
// which tools ran, and whether the run is parked waiting for a human.
type turnResult struct {
	final   string
	outputs []string
	tools   []string
	usage   []string
	pending *genai.FunctionCall
	events  []*session.Event
}

// turn runs one message and reports whether the run parked.
func turn(ctx context.Context, ag agent.Agent, svc session.Service, sid string, msg *genai.Content) (turnResult, error) {
	r, err := runner.New(runner.Config{
		AppName: AppName, Agent: ag, SessionService: svc, AutoCreateSession: true,
	})
	if err != nil {
		return turnResult{}, fmt.Errorf("runner: %w", err)
	}
	var t turnResult
	for ev, runErr := range r.Run(ctx, user, sid, msg, agent.RunConfig{}) {
		if runErr != nil {
			return t, fmt.Errorf("ран: %w", runErr)
		}
		t.events = append(t.events, ev)
		collect(&t, ev)
	}
	return t, nil
}

func collect(t *turnResult, ev *session.Event) {
	if ev == nil {
		return
	}
	if s, ok := ev.Output.(string); ok && strings.TrimSpace(s) != "" {
		t.outputs = append(t.outputs, strings.TrimSpace(s))
	}
	if u := ev.LLMResponse.UsageMetadata; u != nil {
		t.usage = append(t.usage, fmt.Sprintf("%s: prompt %d, output %d, total %d",
			authorOf(ev), u.PromptTokenCount, u.CandidatesTokenCount, u.TotalTokenCount))
	}
	if ev.LLMResponse.Content == nil {
		return
	}
	for _, p := range ev.LLMResponse.Content.Parts {
		switch {
		case p == nil:
		case p.FunctionCall != nil:
			if p.FunctionCall.Name == workflow.WorkflowInputFunctionCallName {
				t.pending = p.FunctionCall
				continue
			}
			t.tools = append(t.tools, authorOf(ev)+"→"+p.FunctionCall.Name)
		case p.Text != "" && !p.Thought:
			t.final = strings.TrimSpace(p.Text)
		}
	}
}

// authorOf names who spoke. Top-level static nodes carry the workflow name and
// an empty NodeInfo.Path; nested and dynamic children carry "parent/child@run",
// so the leaf of the path is the node that actually spoke.
func authorOf(ev *session.Event) string {
	if ev.NodeInfo != nil && ev.NodeInfo.Path != "" {
		path := ev.NodeInfo.Path
		if i := strings.LastIndex(path, "/"); i >= 0 {
			path = path[i+1:]
		}
		return strings.SplitN(path, "@", 2)[0]
	}
	if ev.Author != "" {
		return ev.Author
	}
	return "workflow"
}

func report(out io.Writer, t turnResult) {
	if len(t.tools) > 0 {
		fmt.Fprintln(out, "інструменти:", strings.Join(t.tools, " "))
	}
	for _, o := range t.outputs {
		if strings.Contains(o, "\n") {
			fmt.Fprintf(out, "вузол:\n%s\n", o)
			continue
		}
		fmt.Fprintln(out, "вузол:", o)
	}
	if t.final != "" {
		fmt.Fprintln(out, "відповідь:  ", t.final)
	}
	if len(t.usage) == 0 {
		fmt.Fprintln(out, "токени: модель не повернула UsageMetadata (скриптований фейк) — рахувати нема чого")
		return
	}
	fmt.Fprintln(out, "токени:")
	for _, u := range t.usage {
		fmt.Fprintln(out, "  ", u)
	}
}

// printTrace prints the event stream the way п.5 asks for it: who called what,
// in which order, and where the run stopped.
//
// Branch order here is COMPLETION order, not plan order — that is the point of
// showing it. The three desks run concurrently, so the trace shows whichever
// finished first, while the document's sections stay in plan order.
func printTrace(out io.Writer, t turnResult) {
	fmt.Fprintln(out, "--- потік подій ---")
	for i, ev := range t.events {
		who := authorOf(ev)
		if ev.LLMResponse.Content != nil {
			for _, p := range ev.LLMResponse.Content.Parts {
				switch {
				case p == nil:
				case p.FunctionCall != nil && p.FunctionCall.Name == workflow.WorkflowInputFunctionCallName:
					fmt.Fprintf(out, "%3d  %-22s ⏸ ПАУЗА: питає людину (%s)\n", i, who, p.FunctionCall.ID)
				case p.FunctionCall != nil:
					fmt.Fprintf(out, "%3d  %-22s → %s()\n", i, who, p.FunctionCall.Name)
				case p.FunctionResponse != nil:
					fmt.Fprintf(out, "%3d  %-22s ← %s\n", i, who, p.FunctionResponse.Name)
				case p.Text != "" && !p.Thought:
					fmt.Fprintf(out, "%3d  %-22s «%s»\n", i, who, clip(p.Text))
				}
			}
		}
		if ev.Output != nil {
			if s, ok := ev.Output.(string); ok {
				fmt.Fprintf(out, "%3d  %-22s вихід: %s\n", i, who, clip(s))
				continue
			}
			fmt.Fprintf(out, "%3d  %-22s вихід: %T\n", i, who, ev.Output)
		}
	}
	fmt.Fprintln(out, "--- кінець потоку ---")
}

// clip shortens a line for the terminal.
//
// By runes, not bytes: len() on Ukrainian text cuts a two-byte character in
// half and the terminal prints a replacement glyph. The first version of this
// function did exactly that.
func clip(s string) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ⏎ ")
	r := []rune(s)
	if len(r) > 72 {
		return string(r[:72]) + "…"
	}
	return s
}

// humanPrompt pulls the question out of the pending input request. ADK puts
// the RequestInput.Message into the call's "message" argument; everything else
// in Args is the payload, which the ledger already holds verbatim.
func humanPrompt(fc *genai.FunctionCall) string {
	if m, ok := fc.Args["message"].(string); ok && strings.TrimSpace(m) != "" {
		return m
	}
	return fmt.Sprintf("(запит без тексту: %v)", fc.Args)
}

// userText is the plain user message that starts a run.
func userText(s string) *genai.Content { return genai.NewContentFromText(s, genai.RoleUser) }

// resumeMessage builds the user message that unparks a run.
func resumeMessage(interruptID, answer string) *genai.Content {
	part := &genai.Part{FunctionResponse: &genai.FunctionResponse{
		ID:       interruptID,
		Name:     workflow.WorkflowInputFunctionCallName,
		Response: map[string]any{"response": answer},
	}}
	return &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{part}}
}

// --- складання --------------------------------------------------------------

func buildEngine(ctx context.Context, store *approvals.Store, failing map[string]bool, window time.Duration, offline bool) (Engine, string, error) {
	m, label := resolveModel(ctx, offline)
	ds := desks(failing)
	eng := Engine{
		Plan: Plan{
			Topic:      "Міграція з legacy-процесора",
			Recipients: []string{"board@ledgerworks.example", "oksana@ledgerworks.example"},
			Deadline:   "понеділок 18:00",
		},
		Store: store,
		Desks: ds,
		ModelFor: func(d desk) model.LLM {
			if m != nil {
				return m
			}
			return d.OfflineModel()
		},
		Window: window,
	}
	return eng, label, nil
}

// resolveModel returns the provider's model, or nil with a named reason — nil
// tells the engine to give each desk its own scripted fake.
func resolveModel(ctx context.Context, offline bool) (model.LLM, string) {
	if offline {
		return nil, "ОФЛАЙН-ФЕЙК (прапорець -offline): у кожного стола свій скриптований сценарій."
	}
	if err := modelcfg.LoadEnv("."); err != nil {
		fmt.Fprintf(os.Stderr, "попередження: %v\n", err)
	}
	m, choice, err := modelcfg.Load(ctx)
	if err != nil {
		return nil, "ОФЛАЙН-ФЕЙК (справжньої LLM немає): " + err.Error() +
			". Додайте ключ в apps/.env — див. Homework.md."
	}
	return m, "модель: " + choice.Reason
}

func parseList(s string) map[string]bool {
	out := map[string]bool{}
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out[p] = true
		}
	}
	return out
}
