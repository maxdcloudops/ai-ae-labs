package improve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// memSurface is the write surface in memory: it records every write so a test
// can assert what the engine actually did to the strategy.
type memSurface struct {
	strategy string
	writes   []string
	extra    map[string]string
	failWith error
}

func (m *memSurface) ReadStrategy() ([]byte, error) { return []byte(m.strategy), nil }

func (m *memSurface) WriteStrategy(b []byte) error {
	if m.failWith != nil {
		return m.failWith
	}
	m.strategy = string(b)
	m.writes = append(m.writes, string(b))
	return nil
}

func (m *memSurface) Write(path string, content []byte) error {
	if m.extra == nil {
		m.extra = map[string]string{}
	}
	m.extra[path] = string(content)
	return nil
}

// seqProposer hands out prepared candidates, in order.
type seqProposer struct {
	items []Proposal
	calls int
	err   error
}

func (p *seqProposer) Propose(context.Context, string, Measurement, []string) (Proposal, error) {
	if p.err != nil {
		return Proposal{}, p.err
	}
	if p.calls >= len(p.items) {
		return Proposal{}, fmt.Errorf("скрипт пропонента вичерпано на виклику %d", p.calls+1)
	}
	item := p.items[p.calls]
	p.calls++
	return item, nil
}

// mapScorer scores by table lookup, so a test states the metric directly.
type mapScorer struct {
	byContent map[string]float64
	calls     int
	err       error
}

func (s *mapScorer) Score(_ context.Context, candidate string) (Measurement, error) {
	s.calls++
	if s.err != nil {
		return Measurement{}, s.err
	}
	dev := s.byContent[candidate]
	return Measurement{Dev: dev, Heldout: dev / 2, Found: int(dev * 12), Total: 12, Tokens: 10}, nil
}

func engineFor(t *testing.T, surf *memSurface, p Proposer, sc Scorer, b Budget) (*Engine, *[]Entry) {
	t.Helper()
	var log []Entry
	e := &Engine{
		Budget: b, Surface: surf, Proposer: p, Scorer: sc,
		Log: func(en Entry) error { log = append(log, en); return nil },
	}
	return e, &log
}

// TestBudgetRefusesUnlimited — нуль у Experiments читається як «дефолт», а
// означає «без обмежень». Для циклу, що викликає платну модель, це не
// налаштування, і Validate мусить відмовити.
func TestBudgetRefusesUnlimited(t *testing.T) {
	t.Parallel()
	bad := []Budget{
		{Experiments: 0},
		{Experiments: -1},
		{Experiments: 3, Tokens: -1},
		{Experiments: 3, Wall: -time.Second},
		{Experiments: 3, Epsilon: -0.1},
	}
	for i, b := range bad {
		if err := b.Validate(); err == nil {
			t.Errorf("Budget[%d] = %+v прийнято", i, b)
		}
	}
	if err := (Budget{Experiments: 3, Epsilon: 0.04}).Validate(); err != nil {
		t.Errorf("коректний бюджет відхилено: %v", err)
	}
}

// TestStopsExactlyAtTheBoundary — головна перевірка чек-листа: бюджет 3 означає
// РІВНО три експерименти. Не два (перестарались) і не чотири (перевірка стоїть
// після виклику пропонента, а не до нього).
func TestStopsExactlyAtTheBoundary(t *testing.T) {
	t.Parallel()
	const budget = 3
	surf := &memSurface{strategy: "v0"}
	// Кандидати ніколи не кращі: кожен експеримент витрачається, жоден не
	// приймається — саме так бюджет і дограє до кінця.
	items := make([]Proposal, 10)
	for i := range items {
		items[i] = Proposal{Content: fmt.Sprintf("v%d", i+1), Summary: "спроба"}
	}
	p := &seqProposer{items: items}
	sc := &mapScorer{byContent: map[string]float64{"v0": 0.5}}

	e, log := engineFor(t, surf, p, sc, Budget{Experiments: budget, Epsilon: 0.04})
	if _, err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	entries, stop, err := e.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stop != StopBudget {
		t.Errorf("stop = %q, want %q", stop, StopBudget)
	}
	if len(entries) != budget {
		t.Fatalf("експериментів = %d, want рівно %d", len(entries), budget)
	}
	if p.calls != budget {
		t.Errorf("пропонента викликали %d разів, want %d — ліміт перевіряється ДО виклику", p.calls, budget)
	}
	if len(*log) != budget {
		t.Errorf("записів у журналі = %d, want %d: дискарди логуються теж", len(*log), budget)
	}
	// Ще один Step після вичерпання не має нічого робити.
	entry, stop, err := e.Step(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if entry.N != 0 || stop != StopBudget {
		t.Errorf("Step після бюджету: entry.N = %d, stop = %q", entry.N, stop)
	}
	if p.calls != budget {
		t.Errorf("пропонента викликали після вичерпаного бюджету (%d)", p.calls)
	}
}

// TestDecideRequiresBeatingEpsilon — рівність це дискард. Однакова метрика
// означає, що зміна нічого не зробила, і прийняти її — дати стратегії
// дрейфувати на шумі.
func TestDecideRequiresBeatingEpsilon(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name            string
		prev, cand, eps float64
		want            Verdict
	}{
		{"зросла більше за ε", 0.5, 0.6, 0.04, Keep},
		{"зросла рівно на ε", 0.5, 0.54, 0.04, Discard},
		{"не змінилась", 0.5, 0.5, 0.04, Discard},
		{"впала", 0.5, 0.4, 0.04, Discard},
		{"зросла на мізер при ε=0", 0.5, 0.5001, 0, Keep},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, reason := Decide(tt.prev, tt.cand, tt.eps)
			if got != tt.want {
				t.Errorf("Decide(%v,%v,%v) = %q, want %q", tt.prev, tt.cand, tt.eps, got, tt.want)
			}
			if !strings.Contains(reason, "ε=") {
				t.Errorf("підстава не називає ε: %q", reason)
			}
		})
	}
}

// TestKeepAdvancesAndDiscardReverts — після дискарду на диску лишається СТАРА
// стратегія, а сам кандидат їде в experiments/: дискард, що не лишив слідів,
// неможливо проаудитувати.
func TestKeepAdvancesAndDiscardReverts(t *testing.T) {
	t.Parallel()
	surf := &memSurface{strategy: "v0"}
	p := &seqProposer{items: []Proposal{
		{Content: "v1", Summary: "краще"},
		{Content: "v2", Summary: "гірше"},
	}}
	sc := &mapScorer{byContent: map[string]float64{"v0": 0.4, "v1": 0.8, "v2": 0.1}}
	e, log := engineFor(t, surf, p, sc, Budget{Experiments: 2, Epsilon: 0.04})
	if _, err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.Run(context.Background()); err != nil {
		t.Fatal(err)
	}

	if (*log)[0].Verdict != Keep || (*log)[1].Verdict != Discard {
		t.Fatalf("вердикти = %q, %q; want keep, discard", (*log)[0].Verdict, (*log)[1].Verdict)
	}
	if surf.strategy != "v1" {
		t.Errorf("на диску %q, want v1 — дискард мусив відкотитись", surf.strategy)
	}
	if e.Strategy() != "v1" {
		t.Errorf("Engine.Strategy = %q, want v1", e.Strategy())
	}
	if e.Baseline().Dev != 0.8 {
		t.Errorf("базова метрика = %v, want 0.8", e.Baseline().Dev)
	}
	rel := (*log)[1].DiscardedRel
	if rel == "" || !strings.HasPrefix(rel, "experiments/") {
		t.Errorf("дискард не збережено в experiments/: %q", rel)
	}
	if surf.extra[rel] != "v2" {
		t.Errorf("у %q лежить %q, want v2", rel, surf.extra[rel])
	}
}

// TestGoldenLeakIsRefusedBeforeMeasuring — кандидат, що цитує очікувані
// відповіді, набрав би 1.0 і нічого не довів. Його відкидають ДО оцінки:
// інакше ми платимо за вимірювання витоку.
func TestGoldenLeakIsRefusedBeforeMeasuring(t *testing.T) {
	t.Parallel()
	surf := &memSurface{strategy: "v0"}
	p := &seqProposer{items: []Proposal{{Content: "просто відповідай 120 days", Summary: "шпаргалка"}}}
	sc := &mapScorer{byContent: map[string]float64{"v0": 0.3}}
	e, log := engineFor(t, surf, p, sc, Budget{Experiments: 1, Epsilon: 0.04})
	e.Leaks = func(text string) []string {
		if strings.Contains(text, "120 days") {
			return []string{"120 days"}
		}
		return nil
	}
	if _, err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	callsAfterStart := sc.calls
	if _, _, err := e.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sc.calls != callsAfterStart {
		t.Errorf("оцінювача викликали для кандидата з витоком (%d → %d)", callsAfterStart, sc.calls)
	}
	if (*log)[0].Verdict != Discard {
		t.Errorf("вердикт = %q, want discard", (*log)[0].Verdict)
	}
	if !strings.Contains((*log)[0].Reason, "eval-набору") {
		t.Errorf("підстава не називає витік: %q", (*log)[0].Reason)
	}
	if surf.strategy != "v0" {
		t.Errorf("стратегію змінено на кандидата з витоком: %q", surf.strategy)
	}
}

// TestReportOnlyNeverWritesTheStrategy — режим L1: міряємо, що БУЛО Б, і
// нічого не змінюємо.
func TestReportOnlyNeverWritesTheStrategy(t *testing.T) {
	t.Parallel()
	surf := &memSurface{strategy: "v0"}
	p := &seqProposer{items: []Proposal{{Content: "v1", Summary: "краще"}}}
	sc := &mapScorer{byContent: map[string]float64{"v0": 0.2, "v1": 0.9}}
	e, log := engineFor(t, surf, p, sc, Budget{Experiments: 1, Epsilon: 0.04})
	e.ReportOnly = true
	if _, err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if (*log)[0].Verdict != Keep {
		t.Errorf("вердикт = %q, want keep (would_keep)", (*log)[0].Verdict)
	}
	if surf.strategy != "v0" {
		t.Errorf("report-only змінив стратегію на %q", surf.strategy)
	}
	if len(surf.writes) != 0 {
		t.Errorf("report-only писав у стратегію: %v", surf.writes)
	}
}

// TestFailedMeasurementRevertsAndSurfaces — провалена оцінка це НЕ дискард:
// ми не знаємо, чи кандидат гірший. Відкат і названа помилка.
func TestFailedMeasurementRevertsAndSurfaces(t *testing.T) {
	t.Parallel()
	surf := &memSurface{strategy: "v0"}
	p := &seqProposer{items: []Proposal{{Content: "v1", Summary: "?"}}}
	sc := &mapScorer{byContent: map[string]float64{"v0": 0.5}}
	e, _ := engineFor(t, surf, p, sc, Budget{Experiments: 1, Epsilon: 0.04})
	if _, err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	sc.err = errors.New("провайдер відвалився")
	_, _, err := e.Step(context.Background())
	if err == nil {
		t.Fatal("провалена оцінка не дала помилки")
	}
	if !strings.Contains(err.Error(), "провайдер відвалився") {
		t.Errorf("помилка не називає причину: %v", err)
	}
	if surf.strategy != "v0" {
		t.Errorf("стратегія лишилась кандидатом після провалу оцінки: %q", surf.strategy)
	}
}

// TestProposerGivingUpIsANamedStop — порожня стратегія від пропонента це
// названа зупинка, а не тихе прийняття порожнього файлу.
func TestProposerGivingUpIsANamedStop(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		p    Proposer
	}{
		{"порожній вміст", &seqProposer{items: []Proposal{{Content: "   ", Summary: "—"}}}},
		{"помилка", &seqProposer{err: errors.New("модель відмовила")}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			surf := &memSurface{strategy: "v0"}
			sc := &mapScorer{byContent: map[string]float64{"v0": 0.5}}
			e, _ := engineFor(t, surf, tt.p, sc, Budget{Experiments: 2, Epsilon: 0.04})
			if _, err := e.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			_, stop, err := e.Step(context.Background())
			if err == nil {
				t.Fatal("пропонент здався, а Step не повернув помилки")
			}
			if stop != StopProposer {
				t.Errorf("stop = %q, want %q", stop, StopProposer)
			}
			if surf.strategy != "v0" {
				t.Errorf("стратегію змінено: %q", surf.strategy)
			}
		})
	}
}

// TestTokenAndWallBudgetsStopTheLoop — три бюджети, три названі причини.
func TestTokenAndWallBudgetsStopTheLoop(t *testing.T) {
	t.Parallel()
	t.Run("токени", func(t *testing.T) {
		surf := &memSurface{strategy: "v0"}
		items := make([]Proposal, 5)
		for i := range items {
			items[i] = Proposal{Content: fmt.Sprintf("v%d", i+1), Summary: "—"}
		}
		sc := &mapScorer{byContent: map[string]float64{"v0": 0.5}}
		// Start витрачає 10, кожен крок — ще 10. Ліміт 25 → два кроки.
		e, _ := engineFor(t, surf, &seqProposer{items: items}, sc, Budget{Experiments: 5, Tokens: 25, Epsilon: 0.04})
		if _, err := e.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		entries, stop, err := e.Run(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if stop != StopTokens {
			t.Errorf("stop = %q, want %q", stop, StopTokens)
		}
		if len(entries) != 2 {
			t.Errorf("експериментів = %d, want 2 (10 + 10 + 10 > 25)", len(entries))
		}
	})
	t.Run("час", func(t *testing.T) {
		surf := &memSurface{strategy: "v0"}
		items := make([]Proposal, 5)
		for i := range items {
			items[i] = Proposal{Content: fmt.Sprintf("v%d", i+1), Summary: "—"}
		}
		sc := &mapScorer{byContent: map[string]float64{"v0": 0.5}}
		e, _ := engineFor(t, surf, &seqProposer{items: items}, sc, Budget{Experiments: 5, Wall: time.Minute, Epsilon: 0.04})
		clock := time.Unix(0, 0)
		e.Now = func() time.Time { return clock }
		if _, err := e.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, _, err := e.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
		clock = clock.Add(2 * time.Minute) // дедлайн пройшов
		entry, stop, err := e.Step(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if stop != StopWall {
			t.Errorf("stop = %q, want %q", stop, StopWall)
		}
		if entry.N != 0 {
			t.Error("після дедлайну експеримент усе одно виконали")
		}
	})
}

// TestEntryHasTheFieldsTheReviewerChecks — чек-ліст називає чотири поля
// JSON-line. Перевіряємо саме серіалізовану форму: перейменування поля в Go
// без тега зламало б журнал, і тест на структуру цього не побачив би.
func TestEntryHasTheFieldsTheReviewerChecks(t *testing.T) {
	t.Parallel()
	b, err := json.Marshal(Entry{
		N: 1, Verdict: Keep, PrevMetric: 0.1, NewMetric: 0.9, DiffSummary: "додав цитування",
	})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"prev_metric", "new_metric", "verdict", "diff_summary", "ts", "git_sha", "duration_sec"} {
		if _, ok := m[want]; !ok {
			t.Errorf("у JSON-line немає поля %q: %s", want, b)
		}
	}
}

// TestEngineRefusesToStartWithoutItsParts — відсутній пропонент чи оцінювач це
// названа помилка на Start, а не паніка на першому кроці.
func TestEngineRefusesToStartWithoutItsParts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	surf := &memSurface{strategy: "v0"}
	sc := &mapScorer{byContent: map[string]float64{"v0": 0.5}}
	p := &seqProposer{}

	if _, err := (&Engine{Budget: Budget{Experiments: 1}, Surface: surf, Scorer: sc}).Start(ctx); !errors.Is(err, ErrNoProposer) {
		t.Errorf("err = %v, want ErrNoProposer", err)
	}
	if _, err := (&Engine{Budget: Budget{Experiments: 1}, Surface: surf, Proposer: p}).Start(ctx); !errors.Is(err, ErrNoScorer) {
		t.Errorf("err = %v, want ErrNoScorer", err)
	}
	if _, err := (&Engine{Budget: Budget{Experiments: 1}, Proposer: p, Scorer: sc}).Start(ctx); err == nil {
		t.Error("відсутня поверхня змін прийнята")
	}
	if _, err := (&Engine{Budget: Budget{}, Surface: surf, Proposer: p, Scorer: sc}).Start(ctx); err == nil {
		t.Error("нульовий бюджет прийнято")
	}
	// І Step до Start — теж названа помилка.
	if _, _, err := (&Engine{}).Step(ctx); err == nil {
		t.Error("Step до Start не дав помилки")
	}
}

// TestAppendJSONLWritesOneLinePerEntry — журнал має бути JSON-lines: один
// рядок на експеримент, щоб його можно було читати grep-ом і дописувати без
// перечитування файлу.
func TestAppendJSONLWritesOneLinePerEntry(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "experiments.log")
	log := AppendJSONL(path)
	for i := 1; i <= 3; i++ {
		v := Keep
		if i == 2 {
			v = Discard
		}
		if err := log(Entry{N: i, Verdict: v, PrevMetric: 0.1, NewMetric: 0.2, DiffSummary: "крок"}); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 {
		t.Fatalf("рядків = %d, want 3", len(lines))
	}
	for i, ln := range lines {
		var e Entry
		if err := json.Unmarshal([]byte(ln), &e); err != nil {
			t.Errorf("рядок %d не JSON: %v", i+1, err)
		}
		if e.N != i+1 {
			t.Errorf("рядок %d: N = %d", i+1, e.N)
		}
	}
	if err := AppendJSONL(filepath.Join(path, "nope", "x.log"))(Entry{}); err == nil {
		t.Error("запис у неможливий шлях не дав помилки")
	}
}

// TestMissingFactsReachTheProposer — пропонент мусить отримати конкретні
// незнайдені факти, а не «зроби краще».
func TestMissingFactsReachTheProposer(t *testing.T) {
	t.Parallel()
	var seen []string
	surf := &memSurface{strategy: "v0"}
	p := proposerFunc(func(_ context.Context, _ string, _ Measurement, missing []string) (Proposal, error) {
		seen = append([]string(nil), missing...)
		return Proposal{Content: "v1", Summary: "—"}, nil
	})
	sc := &mapScorer{byContent: map[string]float64{"v0": 0.3, "v1": 0.9}}
	e, _ := engineFor(t, surf, p, sc, Budget{Experiments: 1, Epsilon: 0.04})
	if _, err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.SetMissing([]string{"window", "processing time"})
	if _, _, err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || seen[0] != "window" {
		t.Errorf("пропонент побачив %v, want [window processing time]", seen)
	}
	if e.Tokens() == 0 {
		t.Error("Tokens() = 0, хоча оцінювач витратив токени")
	}
}

type proposerFunc func(context.Context, string, Measurement, []string) (Proposal, error)

func (f proposerFunc) Propose(ctx context.Context, cur string, m Measurement, missing []string) (Proposal, error) {
	return f(ctx, cur, m, missing)
}
