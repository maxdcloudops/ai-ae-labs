// ДЗ 12 (фінал) — Personal Research Agent, що покращує власну стратегію.
//
// Зшивка: запит → план → паралельний збір (база знань + зовнішнє джерело) →
// чернетка → критики → HITL → звіт. Ядро самополіпшення — internal/improve
// (адаптовано з solution/, як просить умова), гальма — internal/sandbox.
//
//	go run . console                      # інтерактивно, з паузою на людину
//	go run . -eval                        # метрика поточної стратегії
//	go run . -improve 5                   # пʼять експериментів самополіпшення
//	go run . -improve 5 -report-only      # L1: що БУЛО Б, нічого не змінюючи
//	go run . -generalize                  # прогін на питаннях, яких не було
//
// Без провайдера все працює на правилах і каже про це першим рядком: це
// доводить гальма, проводку й метрику, але НЕ те, що LLM покращує стратегію.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/cmd/launcher"
	"google.golang.org/adk/v2/cmd/launcher/full"
	"google.golang.org/adk/v2/model"

	"github.com/dimetron/ai-eng-course/labs/internal/modelcfg"
	"github.com/dimetron/ai-eng-course/labs/week6/Day12_Self_Improving_Agents_Demo_Day/labs12/internal/improve"
	"github.com/dimetron/ai-eng-course/labs/week6/Day12_Self_Improving_Agents_Demo_Day/labs12/internal/ledgerkb"
	"github.com/dimetron/ai-eng-course/labs/week6/Day12_Self_Improving_Agents_Demo_Day/labs12/internal/offline"
	"github.com/dimetron/ai-eng-course/labs/week6/Day12_Self_Improving_Agents_Demo_Day/labs12/internal/sandbox"
)

// selfImproveBudget — принцип (б): фіксований бюджет ітерацій.
const selfImproveBudget = 5

// epsilon — приріст, який кандидат мусить перевищити. Без нього шум у метриці
// виглядає як прогрес.
const epsilon = 0.02

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "помилка:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("labs12", flag.ContinueOnError)
	fs.SetOutput(out)
	root := fs.String("root", ".", "робоча директорія: тут лежать strategy.md і experiments/")
	nImprove := fs.Int("improve", 0, "скільки експериментів самополіпшення провести")
	reportOnly := fs.Bool("report-only", false, "L1: міряти, що БУЛО Б, і не змінювати strategy.md")
	doEval := fs.Bool("eval", false, "виміряти поточну стратегію на dev і heldout")
	generalize := fs.Bool("generalize", false, "прогін на питаннях, яких пропонент не бачив")
	offlineFlag := fs.Bool("offline", false, "не шукати провайдера")
	query := fs.String("q", "", "один дослідницький запит (неінтерактивно)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	llm, label := pickModel(ctx, *offlineFlag)
	fmt.Fprintln(out, label)

	box, err := openSandbox(*root)
	if err != nil {
		return err
	}
	strategy, err := box.ReadStrategy()
	if err != nil {
		return fmt.Errorf("створіть %s у %s: %w", sandbox.StrategyFile, box.Root(), err)
	}
	fmt.Fprintf(out, "стратегія: %s (%d байт)\n", box.StrategyPath(), len(strategy))

	kb := ledgerkb.New()
	switch {
	case *nImprove > 0:
		return cmdImprove(ctx, out, box, kb, llm, *nImprove, *reportOnly)
	case *doEval:
		return cmdEval(ctx, out, kb, llm, string(strategy))
	case *generalize:
		return cmdGeneralize(ctx, out, kb, llm, string(strategy))
	case *query != "":
		return cmdOnce(ctx, out, kb, llm, string(strategy), *query)
	}

	// Решта аргументів — режим лаунчера ADK (console / web webui api).
	p := Pipeline{KB: kb, Model: llm, Strategy: string(strategy),
		Log: func(s string) { fmt.Fprintln(out, "  ["+s+"]") }}
	ag, err := p.Agent()
	if err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) == 0 {
		rest = []string{"console"}
	}
	l := full.NewLauncher()
	if err := l.Execute(ctx, &launcher.Config{AgentLoader: agent.NewSingleLoader(ag)}, rest); err != nil {
		return fmt.Errorf("лаунчер: %w\n\n%s", err, l.CommandLineSyntax())
	}
	return nil
}

// openSandbox declares the change surface and makes sure both entries exist.
func openSandbox(root string) (*sandbox.Sandbox, error) {
	if err := os.MkdirAll(filepath.Join(root, sandbox.ExperimentsDir), 0o755); err != nil {
		return nil, fmt.Errorf("каталог experiments/: %w", err)
	}
	return sandbox.New(root, sandbox.StrategyFile, sandbox.ExperimentsDir)
}

// cmdOnce answers one query through the full pipeline, auto-approving so the
// command does not sit waiting for a person.
func cmdOnce(ctx context.Context, out io.Writer, kb *ledgerkb.KB, llm model.LLM, strategy, query string) error {
	p := Pipeline{KB: kb, Model: llm, Strategy: strategy, AutoApprove: true,
		Log: func(s string) { fmt.Fprintln(out, "  ["+s+"]") }}
	ag, err := p.Agent()
	if err != nil {
		return err
	}
	text, tokens, err := runOnce(ctx, ag, query)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "\n%s\n\nтокени: %d\n", text, tokens)
	return nil
}

// cmdEval measures the current strategy. It is the number the whole capstone
// turns on, so it is printed with its raw counts.
func cmdEval(ctx context.Context, out io.Writer, kb *ledgerkb.KB, llm model.LLM, strategy string) error {
	sc := scorerFor(kb, llm)
	m, err := sc.Score(ctx, strategy)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "\ndev     : %.4f (%d з %d фактів, %d питань)\n", m.Dev, m.Found, m.Total, len(sc.Dev))
	fmt.Fprintf(out, "heldout : %.4f (%d питань, keep/discard НЕ вирішується на ньому)\n", m.Heldout, len(sc.Heldout))
	fmt.Fprintf(out, "токени  : %d\n", m.Tokens)
	if len(sc.Missing) > 0 {
		fmt.Fprintf(out, "не знайдено: %s\n", strings.Join(sc.Missing, ", "))
	}
	return nil
}

// cmdGeneralize runs the final strategy against questions the proposer never
// saw. Growth on dev alone is not evidence; this is the check that matters.
func cmdGeneralize(ctx context.Context, out io.Writer, kb *ledgerkb.KB, llm model.LLM, strategy string) error {
	cases := ledgerkb.Heldout()
	fmt.Fprintf(out, "\nгенералізація: %d питань, яких не було в dev\n", len(cases))
	found, total := 0, 0
	for _, c := range cases {
		ans, _, err := answerOne(ctx, llm, kb, strategy, c.Question)
		if err != nil {
			return fmt.Errorf("%s: %w", c.ID, err)
		}
		f, t := ledgerkb.Score(ans, c.Facts)
		found += f
		total += t
		miss := ledgerkb.Missing(ans, c.Facts)
		state := "✓"
		if len(miss) > 0 {
			state = "не знайдено: " + strings.Join(miss, ", ")
		}
		fmt.Fprintf(out, "  %-4s %d/%d  %s\n", c.ID, f, t, state)
	}
	if total > 0 {
		fmt.Fprintf(out, "разом: %.4f (%d з %d)\n", float64(found)/float64(total), found, total)
	}
	return nil
}

// cmdImprove is the self-improvement loop.
func cmdImprove(ctx context.Context, out io.Writer, box *sandbox.Sandbox, kb *ledgerkb.KB, llm model.LLM, n int, reportOnly bool) error {
	sc := scorerFor(kb, llm)
	logPath := filepath.Join(box.Root(), "experiments.log")

	var proposer improve.Proposer = &OfflineProposer{}
	if !isOfflineModel(llm) {
		proposer = &LiveProposer{Model: llm, Sandbox: box}
	}

	eng := &improve.Engine{
		Budget:     improve.Budget{Experiments: n, Epsilon: epsilon},
		Surface:    box,
		Proposer:   proposer,
		Scorer:     sc,
		Leaks:      sc.LeakCheck,
		Log:        improve.AppendJSONL(logPath),
		ReportOnly: reportOnly,
	}
	base, err := eng.Start(ctx)
	if err != nil {
		return err
	}
	mode := "справжній прогін (strategy.md змінюється при keep)"
	if reportOnly {
		mode = "report-only (L1): нічого не змінюється"
	}
	fmt.Fprintf(out, "\n%s · бюджет %d · ε=%.2f\n", mode, n, epsilon)
	fmt.Fprintf(out, "базова метрика: dev %.4f (%d з %d), heldout %.4f\n\n",
		base.Dev, base.Found, base.Total, base.Heldout)

	for {
		eng.SetMissing(sc.Missing)
		entry, stop, err := eng.Step(ctx)
		if err != nil {
			if errors.Is(err, improve.ErrBudget) || stop == improve.StopProposer {
				fmt.Fprintf(out, "зупинка: %s (%v)\n", stop, err)
				break
			}
			return err
		}
		if entry.N == 0 {
			fmt.Fprintf(out, "зупинка: %s\n", stop)
			break
		}
		fmt.Fprintf(out, "  #%d %-8s %.4f → %.4f  heldout %.4f → %.4f  %s\n",
			entry.N, entry.Verdict, entry.PrevMetric, entry.NewMetric,
			entry.HeldoutPrev, entry.HeldoutNew, entry.DiffSummary)
		fmt.Fprintf(out, "      підстава: %s\n", entry.Reason)
		if entry.DiscardedRel != "" {
			fmt.Fprintf(out, "      кандидат збережено: %s\n", entry.DiscardedRel)
		}
		if stop != improve.StopRunning {
			fmt.Fprintf(out, "зупинка: %s\n", stop)
			break
		}
	}

	fin := eng.Baseline()
	fmt.Fprintf(out, "\nметрика    : dev %.4f → %.4f · heldout %.4f → %.4f\n",
		base.Dev, fin.Dev, base.Heldout, fin.Heldout)
	fmt.Fprintf(out, "токени     : %d\n", eng.Tokens())
	fmt.Fprintf(out, "журнал     : %s\n", logPath)
	if reportOnly {
		fmt.Fprintf(out, "strategy.md не змінено (report-only)\n")
	}
	return nil
}

// scorerFor builds the metric over the given model.
func scorerFor(kb *ledgerkb.KB, llm model.LLM) *Scorer {
	return &Scorer{KB: kb, Model: llm, Dev: ledgerkb.Dev(), Heldout: ledgerkb.Heldout()}
}

// pickModel resolves the provider, or returns the rule-based stand-in and says
// plainly what that does and does not prove.
func pickModel(ctx context.Context, forceOffline bool) (model.LLM, string) {
	stand := func(reason string) (model.LLM, string) {
		return offline.Worker{}, fmt.Sprintf(
			"модель: %s — %s; прогін доводить гальма, проводку й метрику, "+
				"але НЕ те, що LLM покращує власну стратегію", offline.Name, reason)
	}
	if forceOffline {
		return stand("прапорець -offline")
	}
	_ = modelcfg.LoadEnv(".")
	llm, choice, err := modelcfg.Load(ctx)
	if err != nil {
		return stand(firstLine(err))
	}
	return llm, "модель: " + choice.Reason
}

// isOfflineModel reports whether the rule-based stand-in is in play.
func isOfflineModel(m model.LLM) bool {
	return m == nil || m.Name() == offline.Name
}

func firstLine(err error) string {
	msg, _, _ := strings.Cut(err.Error(), "\n")
	return msg
}
