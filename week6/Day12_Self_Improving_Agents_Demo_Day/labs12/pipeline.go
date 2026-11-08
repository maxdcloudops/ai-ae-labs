package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/agent/workflowagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/workflow"
	"google.golang.org/genai"

	"github.com/dimetron/ai-eng-course/labs/week6/Day12_Self_Improving_Agents_Demo_Day/labs12/internal/ledgerkb"
)

// evalNodeTimeout bounds every node that calls a model. The reviewer checklist
// asks for it by name: without it one hanging provider call holds the whole
// improvement run, and the budget that was supposed to stop the loop never
// gets a turn.
const evalNodeTimeout = 60 * time.Second

// criticBudget caps the critic loop. Same rule as everywhere: a loop that
// calls a paid model gets a number, not "until it is happy".
const criticBudget = 3

// Plan is what the coordinator decides before anyone works.
type Plan struct {
	Query    string   `json:"query"`
	Branches []string `json:"branches"`
	Strategy string   `json:"strategy"`
}

// Item is one fact with the identifier of where it came from.
type Item struct {
	Claim  string `json:"claim"`
	Source string `json:"source"`
}

// Evidence is one branch's harvest.
type Evidence struct {
	Branch string `json:"branch"`
	Items  []Item `json:"items"`
	Gap    string `json:"gap,omitempty"`
}

// branchOut carries the plan along with the evidence.
//
// A node after a JoinNode receives map[predecessor]output and nothing from
// before the fan-out, so the plan rides inside each branch output. Learned the
// hard way in ДЗ 10, where session state did not survive the boundary.
type branchOut struct {
	Plan Plan     `json:"plan"`
	Ev   Evidence `json:"ev"`
}

// Draft is the report under review.
type Draft struct {
	Plan     Plan     `json:"plan"`
	Text     string   `json:"text"`
	Sources  []string `json:"sources"`
	Attempt  int      `json:"attempt"`
	Feedback []string `json:"feedback,omitempty"`
}

// Approved is what the HITL gate hands the final node.
type Approved struct {
	Draft  Draft  `json:"draft"`
	Status string `json:"status"`
	By     string `json:"by"`
	Reason string `json:"reason"`
}

// Pipeline is the full stitch.
type Pipeline struct {
	KB    *ledgerkb.KB
	Model model.LLM
	// Strategy is the content of strategy.md: the worker's instruction.
	Strategy string
	// AutoApprove skips the human pause. The eval harness sets it, because a
	// metric that needs a person 65 times is not a metric — and the README
	// says the number measures the pipeline up to the human, not the human.
	AutoApprove bool
	// Critics off makes the graph draft→approve directly. Used by nothing in
	// the shipped commands; it exists so a test can isolate the loop.
	SkipCritics bool
	// Log receives one line per stage, for the event-log section of the README.
	//
	// It is called from the fan-out branches, which run concurrently, so the
	// calls are serialised by the mutex below rather than by the caller. The
	// first version left that to the caller and the race detector found it
	// immediately: two branches appending to one slice.
	Log func(string)

	// logMu guards Log. It is a pointer so every copy of this value — and the
	// node closures all capture copies — shares one mutex. Agent() sets it.
	logMu *sync.Mutex
}

func (p Pipeline) note(format string, a ...any) {
	if p.Log == nil {
		return
	}
	if p.logMu != nil {
		p.logMu.Lock()
		defer p.logMu.Unlock()
	}
	p.Log(fmt.Sprintf(format, a...))
}

// Agent assembles:
//
//	Start → plan → ⎡kb_branch ⎤ → gather → draft → critics → approve ⏸ → final
//	                ⎣web_branch⎦
func (p Pipeline) Agent() (agent.Agent, error) {
	// Set before any closure below captures p, so all nodes share one mutex.
	p.logMu = &sync.Mutex{}

	planNode := workflow.NewFunctionNode("plan", p.planFn, workflow.NodeConfig{})
	kbNode := workflow.NewFunctionNode("kb_branch", p.kbFn, workflow.NodeConfig{Timeout: evalNodeTimeout})
	webNode := workflow.NewFunctionNode("web_branch", p.webFn, workflow.NodeConfig{Timeout: evalNodeTimeout})
	gather := workflow.NewJoinNode("gather")
	draft := workflow.NewDynamicNode("draft_report", p.draftFn, workflow.NodeConfig{Timeout: evalNodeTimeout})

	rerun := true
	approve := workflow.NewEmittingFunctionNode("approve", p.approveFn, workflow.NodeConfig{RerunOnResume: &rerun})
	final := workflow.NewFunctionNode("final_report", p.finalFn, workflow.NodeConfig{})

	b := workflow.NewEdgeBuilder()
	b.Add(workflow.Start, planNode)
	b.AddFanOut(planNode, kbNode, webNode)
	b.AddFanIn(gather, kbNode, webNode)
	b.Add(gather, draft)
	if p.SkipCritics {
		b.Add(draft, approve)
	} else {
		critics := workflow.NewDynamicNode("critic_loop", p.criticFn, workflow.NodeConfig{Timeout: evalNodeTimeout * criticBudget})
		b.Add(draft, critics)
		b.Add(critics, approve)
	}
	b.Add(approve, final)

	return workflowagent.New(workflowagent.Config{
		Name:        "personal_research_agent",
		Description: "Запит → план → паралельний збір → чернетка → критики → HITL → звіт.",
		Edges:       b.Build(),
	})
}

// planFn is the coordinator: it fixes the branch order before anyone works, so
// the report's sections do not shuffle between runs.
func (p Pipeline) planFn(_ agent.Context, query string) (Plan, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return Plan{}, errors.New("plan: порожній запит")
	}
	p.note("plan: 2 гілки для запиту %q", clipLine(q, 60))
	return Plan{Query: q, Branches: []string{"kb_branch", "web_branch"}, Strategy: p.Strategy}, nil
}

// kbFn is the internal branch: the ДЗ 5–6 retrieval over the knowledge base.
func (p Pipeline) kbFn(_ agent.Context, in Plan) (branchOut, error) {
	ev := Evidence{Branch: "kb_branch"}
	hits := p.KB.Search(in.Query, 3)
	if len(hits) == 0 {
		ev.Gap = "у базі знань нічого не знайдено"
		p.note("kb_branch: прогалина")
		return branchOut{Plan: in, Ev: ev}, nil
	}
	for _, h := range hits {
		doc, ok := p.KB.Get(h.ID)
		if !ok {
			continue
		}
		ev.Items = append(ev.Items, Item{Claim: doc.Body, Source: doc.ID})
	}
	p.note("kb_branch: %d документів", len(ev.Items))
	return branchOut{Plan: in, Ev: ev}, nil
}

// webFn is the external branch. Its items are marked as a human-written source:
// the draft may quote them, but the report never treats them as policy.
func (p Pipeline) webFn(ctx agent.Context, in Plan) (branchOut, error) {
	ev := Evidence{Branch: "web_branch"}
	out, err := searchWeb(ctx, struct {
		Query string `json:"query" jsonschema:"Пошуковий запит"`
	}{Query: in.Query})
	if err != nil {
		ev.Gap = "зовнішнє джерело не відповіло: " + err.Error()
		return branchOut{Plan: in, Ev: ev}, nil
	}
	for _, h := range out.Hits {
		ev.Items = append(ev.Items, Item{
			Claim:  fmt.Sprint(h["snippet"]),
			Source: fmt.Sprint(h["url"]),
		})
	}
	if len(ev.Items) == 0 {
		ev.Gap = "зовнішніх джерел за запитом немає"
	}
	p.note("web_branch: %d джерел", len(ev.Items))
	return branchOut{Plan: in, Ev: ev}, nil
}

// draftFn synthesises the report. This is the node the strategy governs: the
// worker's instruction IS strategy.md.
func (p Pipeline) draftFn(ctx agent.Context, joined map[string]any, _ func(*session.Event) error) (Draft, error) {
	var plan Plan
	evs := map[string]Evidence{}
	for name, raw := range joined {
		var bo branchOut
		if err := remarshal(raw, &bo); err != nil {
			return Draft{}, fmt.Errorf("гілка %s не розбирається: %w", name, err)
		}
		if bo.Plan.Query != "" {
			plan = bo.Plan
		}
		evs[name] = bo.Ev
	}
	if plan.Query == "" {
		return Draft{}, errors.New("draft: план не доїхав із жодної гілки")
	}

	var sources []string
	for _, name := range plan.Branches {
		for _, it := range evs[name].Items {
			sources = append(sources, it.Source)
		}
	}
	sort.Strings(sources)
	sources = dedup(sources)

	a, err := worker(p.Model, p.KB, plan.Strategy)
	if err != nil {
		return Draft{}, err
	}
	text, _, err := runNodeAgent(ctx, a, plan.Query)
	if err != nil {
		return Draft{}, fmt.Errorf("draft: %w", err)
	}
	p.note("draft_report: %d джерел, %d символів", len(sources), len(text))
	return Draft{Plan: plan, Text: strings.TrimSpace(text), Sources: sources, Attempt: 1}, nil
}

// criticFn is the ДЗ 11 loop, with two critics and a budget.
//
// Their comments go back into the draft, and the loop stops when both pass or
// the budget is spent. Offline both critics are their own rules wrapped as a
// model, so the loop genuinely judges the draft without a provider.
func (p Pipeline) criticFn(ctx agent.Context, d Draft, _ func(*session.Event) error) (Draft, error) {
	for attempt := 1; ; attempt++ {
		if attempt > criticBudget {
			p.note("critic_loop: бюджет %d вичерпано, віддаю як є", criticBudget)
			d.Feedback = append(d.Feedback, "[loop] бюджет критиків вичерпано")
			return d, nil
		}
		d.Attempt = attempt
		var comments []string
		for _, c := range reportCritics() {
			raw, err := p.askCritic(ctx, c, d)
			if err != nil {
				return Draft{}, fmt.Errorf("критик %s: %w", c.Role, err)
			}
			v, err := parseReportVerdict(raw, c.Role)
			if err != nil {
				// Fail-closed: an unreadable verdict is never a pass.
				comments = append(comments, fmt.Sprintf("[%s] вердикт відкинуто: %v", c.Role, err))
				continue
			}
			if !v.Passed {
				for _, cm := range v.Comments {
					comments = append(comments, "["+c.Role+"] "+cm)
				}
			}
		}
		if len(comments) == 0 {
			p.note("critic_loop: усі критики пройдені на спробі %d", attempt)
			return d, nil
		}
		p.note("critic_loop: спроба %d — %d зауважень", attempt, len(comments))
		d.Feedback = comments
		// Redraft with the comments in hand.
		a, err := worker(p.Model, p.KB, d.Plan.Strategy+"\n\nВиправ зауваження:\n"+strings.Join(comments, "\n"))
		if err != nil {
			return Draft{}, err
		}
		text, _, err := runNodeAgent(ctx, a, d.Plan.Query)
		if err != nil {
			return Draft{}, err
		}
		if s := strings.TrimSpace(text); s != "" {
			d.Text = s
		}
	}
}

// approveFn is the human gate. It pauses unless AutoApprove is set.
func (p Pipeline) approveFn(ctx agent.Context, d Draft, emit func(*session.Event) error) (Approved, error) {
	if p.AutoApprove {
		return Approved{Draft: d, Status: "auto", By: "eval-harness", Reason: "режим оцінки: людину не питають"}, nil
	}
	id := "report-" + ctx.InvocationID()
	reply, err := workflow.ResumeOrRequestInput(ctx, emit, session.RequestInput{
		InterruptID: id,
		Message:     approvalQuestion(d),
		Payload:     map[string]any{"sources": d.Sources, "attempt": d.Attempt},
	})
	if err != nil {
		return Approved{}, err // first pass: the run parks
	}
	return p.approveFromReply(d, fmt.Sprint(reply))
}

// approveFromReply classifies a human answer. It is separate from the node so
// the three outcomes can be tested without parking a graph.
//
// Three different records for the same consequence: a person refusing, and a
// UI sending nonsense, are not the same event — and an unreadable answer must
// never be mistaken for a yes.
func (p Pipeline) approveFromReply(d Draft, reply string) (Approved, error) {
	switch strings.ToLower(strings.TrimSpace(reply)) {
	case "approve", "approved", "yes", "так":
		return Approved{Draft: d, Status: "approved", By: "human", Reason: "схвалено людиною"}, nil
	case "deny", "denied", "no", "ні":
		return Approved{Draft: d, Status: "denied", By: "human", Reason: "людина відмовила"}, nil
	default:
		return Approved{Draft: d, Status: "invalid", By: "human",
			Reason: fmt.Sprintf("нерозпізнана відповідь %q", strings.TrimSpace(reply))}, nil
	}
}

// approvalQuestion shows the reviewer facts, not prose: what will be published,
// on how many sources, with which gaps.
func approvalQuestion(d Draft) string {
	var b strings.Builder
	fmt.Fprintf(&b, "СХВАЛЕННЯ ЗВІТУ\nзапит:    %s\n", d.Plan.Query)
	fmt.Fprintf(&b, "джерела:  %s\n", strings.Join(d.Sources, ", "))
	fmt.Fprintf(&b, "спроб:    %d\n", d.Attempt)
	if len(d.Feedback) > 0 {
		fmt.Fprintf(&b, "незакриті зауваження:\n  %s\n", strings.Join(d.Feedback, "\n  "))
	}
	fmt.Fprintf(&b, "\n%s\n", d.Text)
	return b.String()
}

// finalFn is the report, or a named refusal.
func (p Pipeline) finalFn(_ agent.Context, a Approved) (string, error) {
	if a.Status == "denied" || a.Status == "invalid" {
		p.note("final_report: НЕ опубліковано (%s)", a.Status)
		return fmt.Sprintf("НЕ опубліковано: %s — %s", a.Status, a.Reason), nil
	}
	p.note("final_report: опубліковано (%s)", a.Status)
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\nДжерела: %s", a.Draft.Text, strings.Join(a.Draft.Sources, ", "))
	return b.String(), nil
}

// --- критики звіту ----------------------------------------------------------

// reportVerdict is one critic's answer.
type reportVerdict struct {
	Critic   string   `json:"critic"`
	Passed   bool     `json:"passed"`
	Comments []string `json:"comments,omitempty"`
}

// reportCritic is one member of the panel.
type reportCritic struct {
	Role string
	Why  string
	Rule func(Draft) []string
}

// reportCritics are the two the capstone needs: every claim carries a source,
// and the answer actually answers.
func reportCritics() []reportCritic {
	return []reportCritic{
		{Role: "sources", Why: "кожне твердження має джерело", Rule: ruleSources},
		{Role: "coverage", Why: "відповідь містить конкретні цифри, а не загальні слова", Rule: ruleCoverage},
	}
}

func ruleSources(d Draft) []string {
	if len(d.Sources) == 0 {
		return []string{"у чернетці немає жодного джерела — висновок без джерел це догадка"}
	}
	return nil
}

func ruleCoverage(d Draft) []string {
	if len(strings.TrimSpace(d.Text)) < 40 {
		return []string{"відповідь коротша за 40 символів: це не звіт"}
	}
	if !strings.ContainsAny(d.Text, "0123456789") {
		return []string{"у відповіді немає жодної цифри, хоча питання про терміни та ліміти"}
	}
	return nil
}

// parseReportVerdict validates a critic's raw reply. Nothing is repaired: a
// verdict that fails its own contract is evidence about the critic.
func parseReportVerdict(raw, role string) (reportVerdict, error) {
	var v reportVerdict
	if err := json.Unmarshal([]byte(stripFence(raw)), &v); err != nil {
		return reportVerdict{}, fmt.Errorf("вердикт не розбирається як JSON: %w", err)
	}
	switch {
	case v.Critic != role:
		return reportVerdict{}, fmt.Errorf("роль у полі critic (%q) не та, що в вузла (%q)", v.Critic, role)
	case !v.Passed && len(v.Comments) == 0:
		return reportVerdict{}, fmt.Errorf("passed=false без жодного коментаря")
	case v.Passed && len(v.Comments) > 0:
		return reportVerdict{}, fmt.Errorf("passed=true з непорожніми comments")
	}
	return v, nil
}

// askCritic runs one critic and returns its raw reply.
func (p Pipeline) askCritic(ctx agent.Context, c reportCritic, d Draft) (string, error) {
	a, err := llmagent.New(llmagent.Config{
		Name:        "critic_" + c.Role,
		Model:       p.criticModel(c),
		Description: "Критик звіту: " + c.Why,
		Instruction: fmt.Sprintf(`Ти — критик %q. Твій і тільки твій фокус: %s.
Відповідай ЛИШЕ JSON: {"critic":%q,"passed":true|false,"comments":[…]}.
passed=false обовʼязково з коментарями; passed=true — з порожніми comments.`,
			c.Role, c.Why, c.Role),
	})
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(d)
	if err != nil {
		return "", err
	}
	text, _, err := runNodeAgent(ctx, a, string(payload))
	return text, err
}

// criticModel returns the live model, or the critic's own rule wrapped as one.
func (p Pipeline) criticModel(c reportCritic) model.LLM {
	if p.Model != nil && !isOfflineModel(p.Model) {
		return p.Model
	}
	return ruleCritic{role: c.Role, rule: c.Rule}
}

// ruleCritic is a model.LLM that applies a critic's rule to the draft in the
// request. Not a recorded script: change the draft and the verdict changes.
type ruleCritic struct {
	role string
	rule func(Draft) []string
}

func (r ruleCritic) Name() string { return "rule-critic-" + r.role }

func (r ruleCritic) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		v := reportVerdict{Critic: r.role, Passed: true}
		var d Draft
		if err := json.Unmarshal([]byte(lastUserText(req)), &d); err != nil {
			v.Passed, v.Comments = false, []string{"вхід критика не розбирається як Draft"}
		} else if cs := r.rule(d); len(cs) > 0 {
			v.Passed, v.Comments = false, cs
		}
		b, err := json.Marshal(v)
		if err != nil {
			b = []byte(`{"critic":"` + r.role + `","passed":false,"comments":["не вдалося закодувати вердикт"]}`)
		}
		yield(&model.LLMResponse{Content: genai.NewContentFromText(string(b), genai.RoleModel)}, nil)
	}
}

func lastUserText(req *model.LLMRequest) string {
	for i := len(req.Contents) - 1; i >= 0; i-- {
		c := req.Contents[i]
		if c == nil || c.Role != genai.RoleUser {
			continue
		}
		var b strings.Builder
		for _, part := range c.Parts {
			if part != nil {
				b.WriteString(part.Text)
			}
		}
		if s := strings.TrimSpace(b.String()); s != "" {
			return s
		}
	}
	return ""
}

// --- хелпери ----------------------------------------------------------------

// runNodeAgent runs an agent to completion inside a graph node and returns its
// text plus the tokens the provider reported.
func runNodeAgent(ctx context.Context, a agent.Agent, input string) (string, int, error) {
	return runOnce(ctx, a, input)
}

// runOnce runs an agent once over a throwaway session.
func runOnce(ctx context.Context, a agent.Agent, input string) (string, int, error) {
	r, err := runner.New(runner.Config{
		AppName: "labs12", Agent: a,
		SessionService: session.InMemoryService(), AutoCreateSession: true,
	})
	if err != nil {
		return "", 0, fmt.Errorf("runner: %w", err)
	}
	var text string
	tokens := 0
	msg := genai.NewContentFromText(input, genai.RoleUser)
	for ev, runErr := range r.Run(ctx, "lab", "once", msg, agent.RunConfig{}) {
		if runErr != nil {
			return "", tokens, runErr
		}
		if u := ev.LLMResponse.UsageMetadata; u != nil {
			tokens += int(u.PromptTokenCount) + int(u.CandidatesTokenCount)
		}
		// Both places have to be read. A workflow function node puts its
		// result in Event.Output; an LLM or agent event carries it in Content
		// parts. Reading only Content gives an empty final report from a graph,
		// which is exactly how this was first wrong.
		if s, ok := ev.Output.(string); ok && strings.TrimSpace(s) != "" {
			text = s
		}
		if ev.LLMResponse.Content == nil {
			continue
		}
		for _, p := range ev.LLMResponse.Content.Parts {
			if p != nil && p.Text != "" && !p.Thought {
				text = p.Text
			}
		}
	}
	return text, tokens, nil
}

// answerOne answers a single eval question with the given strategy.
func answerOne(ctx context.Context, llm model.LLM, kb *ledgerkb.KB, strategy, question string) (string, int, error) {
	a, err := worker(llm, kb, strategy)
	if err != nil {
		return "", 0, err
	}
	return runOnce(ctx, a, question)
}

func remarshal(src, dst any) error {
	b, err := json.Marshal(src)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, dst)
}

func dedup(in []string) []string {
	out := in[:0]
	var last string
	for i, s := range in {
		if i == 0 || s != last {
			out = append(out, s)
		}
		last = s
	}
	return out
}

func clipLine(s string, n int) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
