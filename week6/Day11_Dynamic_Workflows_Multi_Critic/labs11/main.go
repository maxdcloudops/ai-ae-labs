// ДЗ 11 — Multi-Critic Manifest Agent (альтернативна тема: Kubernetes).
//
// Цикл maker → ОБ'ЄКТИВНИЙ валідатор → панель із трьох LLM-критиків →
// фідбек, доки всі критики не пройдені або не вичерпано бюджет спроб.
//
//	go run . -task pay_api_deployment       # одне завдання
//	go run . -task pay_api_deployment -trace
//	go run . -eval                          # усі три завдання + таблиця
//	go run . -task ledger_cronjob web webui api   # Web UI лаунчера на :8080
//
// Модель беремо з internal/modelcfg (apps/.env, MODEL, DEFAULT_MODEL_PROVIDER).
// Без провайдера maker скриптований, а критики працюють за правилами —
// і вивід прямо каже, що саме зараз несправжнє.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/cmd/launcher"
	"google.golang.org/adk/v2/cmd/launcher/full"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/workflow"

	"github.com/dimetron/ai-eng-course/labs/internal/fakellm"
	"github.com/dimetron/ai-eng-course/labs/internal/modelcfg"
	"github.com/dimetron/ai-eng-course/labs/week6/Day11_Dynamic_Workflows_Multi_Critic/labs11/internal/manifest"
	"github.com/dimetron/ai-eng-course/labs/week6/Day11_Dynamic_Workflows_Multi_Critic/labs11/internal/usage"
)

// maxAttempts — бюджет циклу. Не «100 про всяк випадок»: якщо агент не
// впорався за 4 спроби, п'ята рахунок збільшить, а результат — ні.
const maxAttempts = 4

const makerInstruction = `Ти пишеш Kubernetes-маніфести для LEDGERWORKS. На вхід — JSON:
task (що написати), spec (правила, які маніфест ОБОВ'ЯЗКОВО проходить — це
специфікація), code (твоя попередня версія) і feedback (що не так).
Відповідай ЛИШЕ вмістом одного YAML-файлу, без пояснень і без markdown-огорожі.
Якщо feedback не порожній — виправ рівно те, що там написано, і не чіпай решти.`

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "помилка:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("labs11", flag.ContinueOnError)
	fs.SetOutput(out)
	taskID := fs.String("task", "pay_api_deployment", "id завдання: "+strings.Join(manifest.IDs(), ", "))
	attempts := fs.Int("attempts", maxAttempts, "бюджет спроб")
	trace := fs.Bool("trace", false, "друкувати потік подій циклу")
	evalAll := fs.Bool("eval", false, "прогнати всі завдання й надрукувати таблицю")
	offline := fs.Bool("offline", false, "не шукати провайдера")
	makerKind := fs.String("maker", "buggy", "офлайн-maker: buggy (майже правильний) | insecure (падає на критиках, потім виправляється) | reference (еталон) | stub (нічого не вміє)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	llm, label := makerModel(ctx, *offline)
	isOffline := llm == nil
	fmt.Fprintln(out, label)
	if isOffline {
		fmt.Fprintf(out, "офлайн-maker: %s\n", *makerKind)
	}

	if *evalAll {
		return runEval(ctx, out, llm, *attempts, isOffline, *makerKind)
	}

	task, ok := manifest.ByID(*taskID)
	if !ok {
		return fmt.Errorf("невідоме завдання %q (є: %s)", *taskID, strings.Join(manifest.IDs(), ", "))
	}

	// Слова після прапорців (`web webui api`, `console`) — режим лаунчера ADK.
	if rest := fs.Args(); len(rest) > 0 {
		meter := &usage.Meter{}
		launchLLM := llm
		if isOffline {
			launchLLM = offlineMaker(task, *makerKind)
		}
		ag, _, err := buildAgent(task, launchLLM, *attempts, meter, isOffline)
		if err != nil {
			return err
		}
		l := full.NewLauncher()
		if err := l.Execute(ctx, &launcher.Config{AgentLoader: agent.NewSingleLoader(ag)}, rest); err != nil {
			return fmt.Errorf("лаунчер: %w\n\n%s", err, l.CommandLineSyntax())
		}
		return nil
	}

	res, events, meter, err := runTask(ctx, task, llm, *attempts, isOffline, *makerKind)
	if *trace {
		printTrace(out, events)
	}
	report(out, task, res, meter)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "\nприйнятий маніфест:\n%s\n", res.Accepted)
	return nil
}

// buildAgent wires the maker node, the critic panel and the loop.
// offline is passed explicitly rather than sniffed from the model's type: the
// scripted model is wrapped in usage.Estimating, so a type assertion on it
// silently answers "live" and the critics get a model that cannot judge.
func buildAgent(task manifest.Task, llm model.LLM, attempts int, meter *usage.Meter, offline bool) (agent.Agent, []string, error) {
	return buildAgentWith(task, llm, attempts, meter, criticModelFor(llm, offline))
}

// criticModelFor picks the critics' model. Offline every critic becomes its own
// rule, so the panel genuinely judges the candidate.
func criticModelFor(llm model.LLM, offline bool) func(critic) model.LLM {
	return func(c critic) model.LLM {
		if offline {
			return usage.Estimating(offlineCritic{role: c.Role, rule: c.Rule})
		}
		return llm
	}
}

// buildAgentWith is buildAgent with the critics' models injected. The seam
// exists so a test can hand the panel a critic that misbehaves — the broken
// verdict paths are not reachable otherwise, and they are the ones п.3 is about.
func buildAgentWith(task manifest.Task, llm model.LLM, attempts int, meter *usage.Meter, criticModel func(critic) model.LLM) (agent.Agent, []string, error) {
	makerCfg := llmagent.Config{
		Name:        "maker",
		Model:       llm,
		Description: "Пише і виправляє Kubernetes-маніфест за завданням та зауваженнями.",
		Instruction: makerInstruction,
	}
	if meter != nil {
		makerCfg.AfterModelCallbacks = []llmagent.AfterModelCallback{meter.Callback("maker")}
	}
	makerAgent, err := llmagent.New(makerCfg)
	if err != nil {
		return nil, nil, fmt.Errorf("maker: %w", err)
	}
	makerNode, err := workflow.NewAgentNodeTyped[Draft, string](makerAgent, workflow.NodeConfig{})
	if err != nil {
		return nil, nil, fmt.Errorf("вузол maker: %w", err)
	}

	panelNode, roles, err := buildPanel(criticModel, meter)
	if err != nil {
		return nil, nil, err
	}

	eng := Engine{Task: task, MaxAttempts: attempts}
	ag, err := eng.Agent(makerNode, panelNode, roles)
	if err != nil {
		return nil, nil, err
	}
	return ag, roles, nil
}

// runTask runs one task and collects the event stream.
func runTask(ctx context.Context, task manifest.Task, llm model.LLM, attempts int, offline bool, makerKind string) (LoopResult, []*session.Event, *usage.Meter, error) {
	meter := &usage.Meter{}
	if offline {
		// The scripted maker's script is per task, so it is built per task.
		llm = offlineMaker(task, makerKind)
	}
	ag, _, err := buildAgent(task, llm, attempts, meter, offline)
	if err != nil {
		return LoopResult{TaskID: task.ID}, nil, meter, err
	}
	res, events, err := runLoop(ctx, ag, task)
	res.TaskID = task.ID
	return res, events, meter, err
}

// makerModel picks the live model, or a scripted stand-in and says why.
func makerModel(ctx context.Context, offline bool) (model.LLM, string) {
	scripted := func(reason string) (model.LLM, string) {
		return nil, "ОФЛАЙН (" + reason + "): maker скриптований (сценарій — прапорець -maker), " +
			"критики працюють за правилами. Граф, валідатор, бюджет і облік токенів — справжні."
	}
	if offline {
		return scripted("прапорець -offline")
	}
	_ = modelcfg.LoadEnv(".")
	llm, choice, err := modelcfg.Load(ctx)
	if err == nil {
		return llm, fmt.Sprintf("модель: %s / %s (%s)", choice.Provider, choice.Model, choice.Reason)
	}
	return scripted(strings.SplitN(err.Error(), "\n", 2)[0])
}

// offlineMaker is the scripted maker. The first turn is always a stub that the
// validator rejects, so the gate is exercised; what comes after is the choice.
//
// The choice exists so the false-accept number can be checked instead of
// taken on faith. With "buggy" the maker writes a manifest that satisfies every
// rule it was shown and breaks a hidden one; with "reference" it writes the
// correct manifest. If the eval reported the same figure for both, the harness
// would be measuring itself.
func offlineMaker(task manifest.Task, kind string) model.LLM {
	const stub = "apiVersion: apps/v1\nkind: Deployment\n# TODO\n"
	answer := task.Buggy
	switch kind {
	case "reference":
		answer = task.Reference
	case "insecure":
		// stub → insecure (gate passes, critics refuse) → reference.
		// This is the only mode that drives the feedback path: the critics'
		// comments go into Draft.Feedback and the next draft fixes them.
		first := task.Insecure
		if first == "" {
			first = task.Buggy // Service має не под, а селектор — критикам нічого сказати
		}
		return usage.Estimating(fakellm.New("offline-maker",
			fakellm.TextTurn(stub),
			fakellm.TextTurn(first),
			fakellm.TextTurn(task.Reference),
			fakellm.TextTurn(task.Reference)))
	case "stub":
		// Never produces anything valid: this is the budget-exhausted path.
		return usage.Estimating(fakellm.New("offline-maker",
			fakellm.TextTurn(stub), fakellm.TextTurn(stub),
			fakellm.TextTurn(stub), fakellm.TextTurn(stub), fakellm.TextTurn(stub)))
	}
	return usage.Estimating(fakellm.New("offline-maker",
		fakellm.TextTurn(stub),
		fakellm.TextTurn(answer), fakellm.TextTurn(answer), fakellm.TextTurn(answer),
	))
}

// report prints the per-run summary.
func report(out io.Writer, task manifest.Task, res LoopResult, meter *usage.Meter) {
	fmt.Fprintf(out, "\nзавдання:  %s\n", task.ID)
	fmt.Fprintf(out, "спроб:     %d із %d\n", res.Attempts, maxAttempts)
	fmt.Fprintf(out, "валідатор: відхилив %d\n", res.GateFailures)
	fmt.Fprintf(out, "панель:    відхилила %d, відкинуто вердиктів %d\n", res.PanelFailures, res.RejectedVerdicts)
	fmt.Fprintf(out, "час:       %s\n", res.Elapsed.Round(time.Millisecond))
	if t := meter.Total(); t.Calls > 0 {
		fmt.Fprintf(out, "токени:    %d prompt + %d output за %d викликів\n", t.Prompt, t.Output, t.Calls)
		fmt.Fprint(out, meter.Table())
	} else {
		fmt.Fprintln(out, "токени:    модель не повернула UsageMetadata")
	}
}

// printTrace prints the loop's own events.
func printTrace(out io.Writer, events []*session.Event) {
	fmt.Fprintln(out, "--- потік подій ---")
	for i, ev := range events {
		text := ""
		if ev.LLMResponse.Content != nil {
			var b strings.Builder
			for _, p := range ev.LLMResponse.Content.Parts {
				if p != nil && p.Text != "" && !p.Thought {
					b.WriteString(p.Text)
				}
			}
			text = clip(b.String())
		}
		keys := make([]string, 0, len(ev.Actions.StateDelta))
		for k := range ev.Actions.StateDelta {
			keys = append(keys, fmt.Sprintf("%s=%v", k, ev.Actions.StateDelta[k]))
		}
		switch {
		case text != "" && len(keys) > 0:
			fmt.Fprintf(out, "%3d  %-24s %s   {%s}\n", i, authorOf(ev), text, strings.Join(keys, " "))
		case text != "":
			fmt.Fprintf(out, "%3d  %-24s %s\n", i, authorOf(ev), text)
		case len(keys) > 0:
			fmt.Fprintf(out, "%3d  %-24s {%s}\n", i, authorOf(ev), strings.Join(keys, " "))
		}
	}
	fmt.Fprintln(out, "--- кінець потоку ---")
}

func authorOf(ev *session.Event) string {
	if ev.NodeInfo != nil && ev.NodeInfo.Path != "" {
		p := ev.NodeInfo.Path
		if i := strings.LastIndex(p, "/"); i >= 0 {
			p = p[i+1:]
		}
		return strings.SplitN(p, "@", 2)[0]
	}
	if ev.Author != "" {
		return ev.Author
	}
	return "workflow"
}

// clip shortens a line for the terminal, by runes so Ukrainian text survives.
func clip(s string) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ⏎ ")
	r := []rune(s)
	if len(r) > 88 {
		return string(r[:88]) + "…"
	}
	return s
}

// stripFence знімає markdown-огорожу, якщо модель її додала попри інструкцію.
func stripFence(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "```"); i >= 0 {
		s = s[i:]
		if nl := strings.Index(s, "\n"); nl >= 0 {
			s = s[nl+1:]
		}
		if j := strings.Index(s, "```"); j >= 0 {
			s = s[:j]
		}
	}
	return s
}
