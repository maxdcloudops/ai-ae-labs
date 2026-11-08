package main

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/dimetron/ai-eng-course/labs/week6/Day12_Self_Improving_Agents_Demo_Day/labs12/internal/improve"
	"github.com/dimetron/ai-eng-course/labs/week6/Day12_Self_Improving_Agents_Demo_Day/labs12/internal/ledgerkb"
	"github.com/dimetron/ai-eng-course/labs/week6/Day12_Self_Improving_Agents_Demo_Day/labs12/internal/sandbox"
)

// webDoc is one page of the fixture "web" source.
//
// A fixture and not a real search: AGENTS.md §5.3 requires `go test ./...` to
// pass with no network, and a capstone whose metric depends on what Google
// returned today is a capstone whose numbers cannot be reproduced.
type webDoc struct {
	URL   string
	Title string
	Body  string
}

// webCorpus is the external source. It deliberately OVERLAPS the knowledge
// base on some facts and contradicts it on none: the point of two branches is
// corroboration, not volume.
func webCorpus() []webDoc {
	return []webDoc{
		{"https://example.org/psd2-sca", "PSD2 SCA exemptions",
			"Under PSD2, strong customer authentication may be exempted for low-value transactions below 30 EUR, with cumulative limits. Acquirers must track exemption rates."},
		{"https://example.org/card-scheme-chargebacks", "Card scheme chargeback windows",
			"Issuers may raise a chargeback for reason code 4837 within 120 days of the transaction date. Merchant evidence is due within 7 calendar days of notification."},
		{"https://example.org/pci-dss-4", "PCI DSS 4.0 for service providers",
			"A level 1 service provider undergoes an annual on-site assessment and quarterly ASV scans. Tokenization reduces the cardholder data environment."},
		{"https://example.org/sev-classification", "Incident severity practice",
			"A SEV1 incident is customer-visible and revenue-affecting. Common practice pages the on-call within 5 minutes and posts updates every 30 minutes."},
	}
}

// searchWeb is the external branch's tool.
func searchWeb(_ agent.Context, in struct {
	Query string `json:"query" jsonschema:"Пошуковий запит"`
}) (struct {
	Hits []map[string]any `json:"hits"`
}, error) {
	type out = struct {
		Hits []map[string]any `json:"hits"`
	}
	terms := lowerTerms(in.Query)
	type scored struct {
		doc webDoc
		n   int
	}
	var all []scored
	for _, d := range webCorpus() {
		hay := strings.ToLower(d.Title + " " + d.Body)
		n := 0
		for _, t := range terms {
			if strings.Contains(hay, t) {
				n++
			}
		}
		if n > 0 {
			all = append(all, scored{d, n})
		}
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].n > all[j].n })
	hits := make([]map[string]any, 0, 3)
	for i, s := range all {
		if i == 3 {
			break
		}
		hits = append(hits, map[string]any{"url": s.doc.URL, "title": s.doc.Title, "snippet": s.doc.Body})
	}
	return out{Hits: hits}, nil
}

func lowerTerms(q string) []string {
	var out []string
	for _, w := range strings.FieldsFunc(strings.ToLower(q), func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9')
	}) {
		if len(w) > 3 {
			out = append(out, w)
		}
	}
	return out
}

// worker builds the research agent: the strategy IS its instruction.
//
// That is the whole mechanism of principle (d). The strategy file is not
// documentation about the agent — it is the agent's prompt, so editing the file
// changes behaviour, and the metric notices.
func worker(llm model.LLM, kb *ledgerkb.KB, strategy string) (agent.Agent, error) {
	search, err := kb.Tool(ledgerkb.ToolSearch)
	if err != nil {
		return nil, fmt.Errorf("kb_search: %w", err)
	}
	read, err := kb.Tool(ledgerkb.ToolRead)
	if err != nil {
		return nil, fmt.Errorf("kb_read: %w", err)
	}
	web, err := functiontool.New(functiontool.Config{
		Name:        "web_search",
		Description: "Шукає у зовнішніх публічних джерелах (фікстура, не мережа).",
	}, searchWeb)
	if err != nil {
		return nil, fmt.Errorf("web_search: %w", err)
	}
	return llmagent.New(llmagent.Config{
		Name:        "researcher",
		Model:       llm,
		Description: "Відповідає на дослідницькі питання за базою знань і зовнішніми джерелами.",
		Instruction: strategy,
		Tools:       []tool.Tool{search, read, web},
	})
}

// Scorer is principle (c): one objective, comparable metric.
//
// Score itself is ledgerkb.Score — a pure function over the answer text and the
// golden facts, with no model call. What this type adds is running the agent
// once per eval question and averaging. Dev decides keep/discard; Heldout is
// reported and never decides.
type Scorer struct {
	KB      *ledgerkb.KB
	Model   model.LLM
	Dev     []ledgerkb.Case
	Heldout []ledgerkb.Case
	// Missing collects the facts the last scored strategy failed to find, so
	// the proposer gets a target instead of "do better".
	Missing []string
}

// Score measures one candidate strategy.
func (s *Scorer) Score(ctx context.Context, candidate string) (improve.Measurement, error) {
	dev, found, total, missing, tokens, err := s.run(ctx, candidate, s.Dev)
	if err != nil {
		return improve.Measurement{}, err
	}
	held, _, _, _, heldTokens, err := s.run(ctx, candidate, s.Heldout)
	if err != nil {
		return improve.Measurement{}, err
	}
	s.Missing = missing
	return improve.Measurement{
		Dev: dev, Heldout: held, Found: found, Total: total, Tokens: tokens + heldTokens,
	}, nil
}

// run answers every case of a set and aggregates.
//
// The metric is facts found over facts expected across the whole set, not the
// mean of per-question ratios: a question with three facts should weigh three
// times a question with one, and averaging ratios silently makes every
// question equal.
func (s *Scorer) run(ctx context.Context, strategy string, cases []ledgerkb.Case) (score float64, found, total int, missing []string, tokens int, err error) {
	seen := map[string]bool{}
	for _, c := range cases {
		ans, used, err := answerOne(ctx, s.Model, s.KB, strategy, c.Question)
		if err != nil {
			return 0, 0, 0, nil, 0, fmt.Errorf("питання %s: %w", c.ID, err)
		}
		tokens += used
		f, tt := ledgerkb.Score(ans, c.Facts)
		found += f
		total += tt
		for _, name := range ledgerkb.Missing(ans, c.Facts) {
			if !seen[name] {
				seen[name] = true
				missing = append(missing, name)
			}
		}
	}
	if total == 0 {
		return 0, 0, 0, missing, tokens, nil
	}
	return float64(found) / float64(total), found, total, missing, tokens, nil
}

// LeakCheck is the other half of principle (c): a candidate strategy that
// quotes the expected answers would score 1.0 and prove nothing.
func (s *Scorer) LeakCheck(text string) []string {
	all := append(append([]ledgerkb.Case{}, s.Dev...), s.Heldout...)
	return ledgerkb.Leaks(text, all)
}

// --- пропонент --------------------------------------------------------------

// OfflineProposer is the rule-based proposer: a fixed ladder of strategy edits.
//
// It is NOT a language model, and the run says so. It exists because the
// offline worker reacts to exactly three phrases in its instruction
// ("kb_read", "every relevant document", "every figure" — see
// internal/offline), so a deterministic ladder that adds them one at a time
// produces a REAL measurable improvement curve: the metric moves because the
// agent genuinely behaves differently, not because a script said it would.
//
// What it cannot show is a model inventing a better strategy. Only a live run
// shows that, and the README says so.
type OfflineProposer struct {
	// offered remembers every candidate already handed out, so the loop never
	// pays an experiment for a measurement it already has.
	offered map[string]bool
}

// rungs are the ladder of process edits, in order. Each is a line a competent
// reviewer might add to a research strategy.
//
// The first three are levers the offline worker actually reacts to (see
// internal/offline). The rest are sensible process instructions it ignores —
// and that is on purpose: most proposals do not help, and a log that only ever
// records keeps is a log of a loop nobody learned anything from.
var rungs = []struct {
	Line    string
	Summary string
}{
	{"Для кожного питання читай найкращий документ інструментом kb_read, а не відповідай зі сніпетів.",
		"kb_read: сніпет обрізає речення з цифрою"},
	{"Цитуй кожну цифру (every figure) з прочитаного документа, а не лише перше речення.",
		"«every figure»: відповідь мусить містити всі цифри, а не підсумок"},
	{"Читай кожен релевантний документ (every relevant document), не лише перший.",
		"«every relevant document»: одна відповідь часто спирається на три документи"},
	{"Якщо після двох пошуків факту немає — скажи, що його немає, і не вигадуй.",
		"явна відмова замість вигадки"},
	{"Перед відповіддю перевір, що кожна цифра має джерело; якщо ні — зроби ще один kb_read.",
		"самоперевірка цитувань перед відповіддю"},
	{"Якщо релевантних документів більше трьох — звужуй запит, а не читай усі.",
		"звуження запиту замість читання всього"},
}

// Propose returns the next candidate: the rungs not yet in the strategy, taking
// one more of them when the narrow step has already been tried.
//
// Why it ever widens — this is the finding of the offline run, and it is a
// property of the pattern rather than of this lab. A greedy keep/discard loop
// cannot climb a plateau that needs TWO changes at once: adding kb_read alone
// reads the whole document but still answers with its first sentence, so the
// metric does not move, the candidate is reverted, and the next narrow step
// starts from exactly the same place. Offering the pair is the cheapest way out
// of that trap — and it is why the course reference adds both levers in one
// line instead of two.
//
// Narrow first, though: a narrow step that works tells you WHICH lever worked.
func (p *OfflineProposer) Propose(_ context.Context, current string, _ improve.Measurement, missing []string) (improve.Proposal, error) {
	type rung struct{ Line, Summary string }
	var absent []rung
	for _, r := range rungs {
		if !strings.Contains(current, r.Line) {
			absent = append(absent, rung{r.Line, r.Summary})
		}
	}
	if len(absent) == 0 {
		return improve.Proposal{}, fmt.Errorf("правилові пропозиції вичерпано: усі %d важелів уже в стратегії", len(rungs))
	}
	if p.offered == nil {
		p.offered = map[string]bool{}
	}

	// Widen until a candidate appears that has not been offered before.
	// Offering the same text twice would buy an identical measurement with a
	// whole experiment of the budget.
	for width := 1; width <= len(absent); width++ {
		take := absent[:width]
		var lines, summaries []string
		for _, r := range take {
			lines = append(lines, r.Line)
			summaries = append(summaries, r.Summary)
		}
		content := strings.TrimRight(current, "\n") + "\n" + strings.Join(lines, "\n") + "\n"
		if p.offered[content] {
			continue
		}
		p.offered[content] = true
		summary := "додано: " + strings.Join(summaries, "; ")
		if len(missing) > 0 {
			summary += fmt.Sprintf(" (не знайдено: %s)", strings.Join(firstN(missing, 3), ", "))
		}
		return improve.Proposal{Content: content, Summary: summary}, nil
	}
	return improve.Proposal{}, fmt.Errorf("правилові пропозиції вичерпано: усі комбінації з %d важелів уже пропонувались", len(absent))
}

func firstN(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// LiveProposer asks a model for the next candidate strategy.
//
// The model does not write the file. It returns text, and the engine writes it
// through the sandbox — so principle (a) holds even if the model asks for
// something else entirely.
type LiveProposer struct {
	Model model.LLM
	// Sandbox is used only to tell the model what it is allowed to change.
	Sandbox *sandbox.Sandbox
}

const proposerInstruction = `Ти редагуєш ДОСЛІДНИЦЬКУ СТРАТЕГІЮ агента — один markdown-файл,
який є його системною інструкцією. Твоя мета — підняти частку знайдених фактів
на eval-наборі.

Правила, і вони жорсткі:
- Відповідай ЛИШЕ новим повним вмістом файлу, без пояснень і без огорожі.
- Зміни МАЛО: один-два рядки за раз. Велика переписка не дає зрозуміти, що саме
  допомогло.
- НЕ вписуй у стратегію самі відповіді з питань (цифри, назви, дати). Це витік
  eval-набору: метрика зросте, а агент не покращиться. Такий кандидат буде
  відхилено автоматично.
- Пиши про ПРОЦЕС: які інструменти викликати, скільки документів читати, що
  обовʼязково цитувати, коли зупинитись і спитати людину.`

// Propose asks the model for a candidate.
func (p *LiveProposer) Propose(ctx context.Context, current string, m improve.Measurement, missing []string) (improve.Proposal, error) {
	a, err := llmagent.New(llmagent.Config{
		Name:        "strategy_proposer",
		Model:       p.Model,
		Description: "Пропонує наступну версію дослідницької стратегії.",
		Instruction: proposerInstruction,
	})
	if err != nil {
		return improve.Proposal{}, err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Поточна метрика: %.4f (%d з %d фактів).\n", m.Dev, m.Found, m.Total)
	if len(missing) > 0 {
		fmt.Fprintf(&b, "Не знайдено фактів: %s.\n", strings.Join(firstN(missing, 8), ", "))
	}
	fmt.Fprintf(&b, "Дозволено змінювати лише %s.\n\nПоточний вміст:\n%s", p.Sandbox.StrategyRel(), current)

	text, _, err := runOnce(ctx, a, b.String())
	if err != nil {
		return improve.Proposal{}, err
	}
	content := strings.TrimSpace(stripFence(text))
	if content == "" {
		return improve.Proposal{}, fmt.Errorf("модель віддала порожню стратегію")
	}
	return improve.Proposal{Content: content + "\n", Summary: "кандидат від моделі"}, nil
}

// stripFence removes a markdown fence if the model added one despite the
// instruction.
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
