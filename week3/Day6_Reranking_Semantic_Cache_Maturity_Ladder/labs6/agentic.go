package main

import (
	"fmt"
	"sort"
	"strings"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/session"

	"github.com/dimetron/ai-eng-course/labs/week3/Day6_Reranking_Semantic_Cache_Maturity_Ladder/labs6/internal/embed"
)

// Цей файл — агентна половина ДЗ 6: планувальник, три гілки retrieval,
// збірка й формат. Базовий лінійний шлях лишився в retrieval.go; тут —
// топологія з розгалуженням.

// Plan — рішення планувальника: що це за запит і скільки кроків йому дозволено.
type Plan struct {
	Query  string `json:"query"`
	Class  string `json:"class"`  // single-hop | multi-hop | metadata-filter | global
	Budget int    `json:"budget"` // стеля retrieval-кроків
}

// Класи запитів. Від них залежить бюджет: multi-hop чесно дорожчий, бо йому
// треба пройти по зв'язках, а не знайти один фрагмент.
const (
	classSingleHop = "single-hop"
	classMultiHop  = "multi-hop"
	classMetadata  = "metadata-filter"
	classGlobal    = "global"
)

// budgetFor — стеля retrieval-кроків на клас запиту (Homework п.1: 3–5).
//
// Бюджет — це запобіжник, а не оптимізація. Агент сам вирішує, скільки
// шукати, і без стелі планування може не зупинитися. Та сама дисципліна, що
// maxHistoryDays у ДЗ 2: обмеження задає контракт, а не сподівання.
// Числа підібрані під ТОПОЛОГІЮ, а не взяті зі стелі: гілок три, кожна коштує
// крок, а graph_traversal на зв'язках робить другий hop. Тобто 4 — це
// нормальний прогін, 3 — коли traversal мовчить, 5 — стеля для multi-hop.
// Бюджет, менший за кількість гілок, означав би, що граф не може виконатися
// жодного разу, — і спіймав це тест, а не читання коду.
func budgetFor(class string) int {
	switch class {
	case classMultiHop:
		return 5
	case classGlobal, classMetadata:
		return 4
	default:
		return 4
	}
}

// multiHopMarkers — слова, за якими запит виглядає як багатокроковий.
//
// Груба евристика, і вона тут доречна: класифікатор помиляється в бік
// більшого бюджету, а не меншого. Зайвий крок коштує мілісекунди; пропущений
// hop коштує половини відповіді.
var multiHopMarkers = []string{"і хто", "а хто", "хто підпис", "та хто", "who signed", "pov'яза", "пов'яза", "звʼяза"}

// metadataMarkers — ознаки запиту-фільтра: конкретні коди, тарифи, періоди.
var metadataMarkers = []string{"t-1", "t-2", "q3", "q4", "soc2", "high", "medium", "low", "нбу", "nbu"}

// classify — планувальник. Емітує маршрут і повертає план для всіх гілок.
//
// Повертає Plan (а не nil), бо далі йде fan-out: кожна гілка має отримати
// запит. Якби вузол повертав nil, наступники побачили б порожній вхід.
func (r *retriever) classify(ctx agent.Context, probe CacheProbe, emit func(*session.Event) error) (Plan, error) {
	q := strings.TrimSpace(probe.Query)
	if q == "" {
		return Plan{}, fmt.Errorf("порожній запит")
	}
	lower := strings.ToLower(q)

	class := classSingleHop
	switch {
	case containsAny(lower, multiHopMarkers):
		class = classMultiHop
	case containsAny(lower, metadataMarkers):
		class = classMetadata
	case len(tokens(lower)) <= 3:
		class = classGlobal
	}

	// Подія несе ЛИШЕ маршрут. Якщо покласти в неї ще й Output, вузол видасть
	// два значення — і ADK відхилить прогін: «only one event per execution
	// can carry output». Значення йде через return, щоб гілки отримали
	// типізований Plan, а не рядок.
	ev := session.NewEvent(ctx, ctx.InvocationID())
	ev.Routes = []string{class}
	if err := emit(ev); err != nil {
		return Plan{}, err
	}
	return Plan{Query: q, Class: class, Budget: budgetFor(class)}, nil
}

func containsAny(s string, markers []string) bool {
	for _, m := range markers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// RetrievalResult — що знайшла одна гілка. Branch потрібен, щоб у звіті було
// видно, яка саме гілка дала доказ, — інакше provenance неповна.
type RetrievalResult struct {
	Branch     string      `json:"branch"`
	Class      string      `json:"class"`
	Query      string      `json:"query"`
	Candidates []Candidate `json:"candidates"`
	Steps      int         `json:"steps"`
}

// vectorSearch — семантична гілка: косинус між ембедингом запиту й чанка.
//
// Саме вона відповідає на «про що це», і саме вона не вміє в точні фільтри:
// «тариф T-2» і «тариф T-1» для неї майже однакові. Тому поруч живе
// graph_filter.
func (r *retriever) vectorSearch(ctx agent.Context, p Plan) (RetrievalResult, error) {
	out := RetrievalResult{Branch: "vector", Class: p.Class, Query: p.Query, Steps: 1}
	texts, chunks := r.searchable()
	if len(texts) == 0 {
		return out, nil
	}
	scored, err := embed.Rerank(ctx, r.emb, p.Query, texts)
	if err != nil {
		// Падіння ембеддера не має валити весь граф: інші гілки ще можуть
		// дати докази, а порожня гілка краще за зупинений запит.
		return out, nil
	}
	for _, s := range scored {
		if s.Index < 0 || s.Index >= len(chunks) {
			continue
		}
		c := chunks[s.Index]
		out.Candidates = append(out.Candidates, Candidate{
			ChunkID: c.ID, DocumentID: c.DocumentID, SectionPath: c.SectionPath,
			Text: c.Text, Score: s.Score,
		})
	}
	out.Candidates = trim(out.Candidates, topK)
	return out, nil
}

// graphFilter — структурний фільтр: точні коди й значення з таблиць.
//
// Працює по чанках Kind == "table", бо саме там живуть T-2, Severity=High і
// періоди. Збіг тут — підрядок, а не схожість: «T-2» або є в рядку, або нема.
func (r *retriever) graphFilter(_ agent.Context, p Plan) (RetrievalResult, error) {
	out := RetrievalResult{Branch: "graph_filter", Class: p.Class, Query: p.Query, Steps: 1}
	terms := filterTerms(p.Query)
	if len(terms) == 0 {
		return out, nil
	}
	for _, c := range r.chunks {
		if c.Kind != "table" {
			continue
		}
		words := tokenSet(c.Text)
		hits := 0
		for _, t := range terms {
			if words[t] {
				hits++
			}
		}
		if hits == 0 {
			continue
		}
		out.Candidates = append(out.Candidates, Candidate{
			ChunkID: c.ID, DocumentID: c.DocumentID, SectionPath: c.SectionPath,
			Text: c.Text, Score: float64(hits) / float64(len(terms)),
		})
	}
	sortByScore(out.Candidates)
	out.Candidates = trim(out.Candidates, topK)
	return out, nil
}

// filterTerms дістає з запиту те, що схоже на точне значення: коди з дефісом
// або цифрою, періоди, рівні. Звичайні слова сюди не йдуть — ними займається
// vector-гілка.
func filterTerms(query string) []string {
	var out []string
	for _, t := range tokens(query) {
		if strings.ContainsAny(t, "-0123456789") || containsAny(t, metadataMarkers) {
			out = append(out, t)
		}
	}
	return out
}

// graphTraversal — перехід по зв'язках: знайти згадку, потім сусідній чанк
// у тому самому розділі.
//
// Це спрощений traversal: сусідство за SectionPath замість справжнього графа
// сутностей. На корпусі ДЗ 5 цього достатньо — підписанти лежать у розділі
// «3.2 Contract signers» поруч із тарифами з «3.1», — і в README це сказано
// прямо як обмеження.
func (r *retriever) graphTraversal(_ agent.Context, p Plan) (RetrievalResult, error) {
	out := RetrievalResult{Branch: "graph_traversal", Class: p.Class, Query: p.Query, Steps: 1}
	if p.Class != classMultiHop && p.Class != classMetadata {
		return out, nil
	}

	// Крок 1: чанки, де згадані точні значення запиту.
	seeds := map[string]bool{}
	roots := map[string]bool{}
	for _, t := range filterTerms(p.Query) {
		for _, c := range r.chunks {
			if tokenSet(c.Text)[t] {
				seeds[c.ID] = true
				roots[sectionRoot(c.SectionPath)] = true
			}
		}
	}
	if len(seeds) == 0 {
		return out, nil
	}
	out.Steps = 2

	// Крок 2: усе з того самого кореневого розділу, чого ще не бачили. Саме
	// тут і з'являється друга половина відповіді на запит Оксани.
	for _, c := range r.chunks {
		if c.Kind == "heading" || seeds[c.ID] {
			continue
		}
		if !roots[sectionRoot(c.SectionPath)] {
			continue
		}
		out.Candidates = append(out.Candidates, Candidate{
			ChunkID: c.ID, DocumentID: c.DocumentID, SectionPath: c.SectionPath,
			Text: c.Text, Score: 0.5,
		})
	}
	if out.Steps > p.Budget {
		out.Steps = p.Budget
	}
	out.Candidates = trim(out.Candidates, topK)
	return out, nil
}

// sectionRoot повертає верхній рівень шляху: «3 Tariffs / 3.1 …» → «3 Tariffs».
// tokenSet — слова тексту як множина.
//
// Порівняння по ТОКЕНАХ, а не по підрядку, і це не дрібниця: підрядок «t-2»
// знаходиться всередині «ct-2025-014», тож договір потрапляв у seeds і
// випадав із обходу — відповідь на запит Оксани лишалася без підписанта.
// Помилка тиха: гілка просто повертала порожньо, і ніщо не падало.
func tokenSet(text string) map[string]bool {
	out := map[string]bool{}
	for _, t := range tokens(text) {
		out[t] = true
	}
	return out
}

func sectionRoot(path string) string {
	if i := strings.Index(path, " / "); i > 0 {
		return path[:i]
	}
	return path
}

// searchable віддає тексти й чанки, придатні для пошуку: усе, крім заголовків.
func (r *retriever) searchable() ([]string, []corpusChunk) {
	var texts []string
	var out []corpusChunk
	for _, c := range r.chunks {
		if c.Kind == "heading" {
			continue
		}
		texts = append(texts, c.Text)
		out = append(out, corpusChunk{ID: c.ID, DocumentID: c.DocumentID, SectionPath: c.SectionPath, Text: c.Text})
	}
	return texts, out
}

// corpusChunk — локальна проєкція corpus.Chunk, щоб не тягнути імпорт у кожен
// підпис.
type corpusChunk struct {
	ID, DocumentID, SectionPath, Text string
}

func sortByScore(c []Candidate) {
	sort.SliceStable(c, func(i, j int) bool { return c[i].Score > c[j].Score })
}

func trim(c []Candidate, n int) []Candidate {
	if len(c) > n {
		return c[:n]
	}
	return c
}

// --- Збірка, re-ranking і формат -------------------------------------------

// mergeAndRerank збирає гілки з JoinNode і переранжовує спільним шаром.
//
// Вхід — map[string]any, ключ = ім'я вузла-попередника (так влаштований
// JoinNode). Дедуплікація за ChunkID обов'язкова: один і той самий чанк
// легко приходить із двох гілок, і без злиття він двічі займе місце в топі,
// витіснивши інший доказ.
//
// Re-ranking — ДРУГИЙ прохід: гілки віддають швидко й грубо (top-K=5 кожна),
// а тут повільно й точно залишається top-N=3. Дешево відсіяти багато, дорого
// перевірити мало.
func (r *retriever) mergeAndRerank(ctx agent.Context, in map[string]any) (SearchResult, error) {
	var (
		query   string
		class   string
		steps   int
		merged  []Candidate
		byChunk = map[string]int{}
	)
	// branchOf пам'ятає, яка гілка знайшла чанк: після re-ranking саме за цим
	// забезпечується представництво кожного hop.
	branchOf := map[string]string{}
	for _, name := range sortedKeys(in) {
		res, ok := in[name].(RetrievalResult)
		if !ok {
			continue
		}
		if query == "" {
			query, class = res.Query, res.Class
		}
		steps += res.Steps
		for _, c := range res.Candidates {
			if i, seen := byChunk[c.ChunkID]; seen {
				// Той самий чанк із двох гілок — сильніший сигнал, а не
				// привід показати його двічі.
				if c.Score > merged[i].Score {
					merged[i].Score = c.Score
				}
				continue
			}
			byChunk[c.ChunkID] = len(merged)
			branchOf[c.ChunkID] = res.Branch
			merged = append(merged, c)
		}
	}

	out := SearchResult{Query: query, Candidates: merged}
	if len(merged) == 0 {
		return out, nil
	}

	before := make([]string, len(merged))
	for i, c := range merged {
		before[i] = c.ChunkID
	}

	texts := make([]string, len(merged))
	for i, c := range merged {
		texts[i] = c.Text
	}
	if scored, err := embed.Rerank(ctx, r.emb, query, texts); err == nil {
		reordered := make([]Candidate, 0, len(scored))
		for _, s := range scored {
			if s.Index >= 0 && s.Index < len(merged) {
				c := merged[s.Index]
				c.Score = s.Score
				reordered = append(reordered, c)
			}
		}
		if len(reordered) > 0 {
			out.Candidates = reordered
		}
	}
	sortByScore(out.Candidates)
	out.Candidates = keepDiverse(out.Candidates, branchOf, topNFor(class))

	r.lastRerank = rerankTrace{Query: query, Class: class, Steps: steps, Before: before, After: idsOf(out.Candidates)}
	return out, nil
}

// topN — скільки кандидатів переживає re-ranking і йде у відповідь.
const topN = 3

// topNFor — multi-hop потребує більше місць у відповіді.
//
// Відповідь на «хто такі X і хто підписав їхні договори» складається з ДВОХ
// фактів із різних розділів. Три місця на таку відповідь — це гарантія, що
// один із фактів не поміститься.
func topNFor(class string) int {
	if class == classMultiHop {
		return 5
	}
	return topN
}

// keepDiverse лишає top-N, але гарантує представництво кожної гілки.
//
// Чисте ранжування за score на multi-hop відкидає другий hop: гілка
// graph_traversal дає нижчі оцінки (вона знаходить сусідів, а не прямі збіги),
// і її докази не доживають до топу. Тоді відповідь виглядає впевненою й
// містить рівно половину того, про що питали.
//
// Тому спершу беремо найкращого з КОЖНОЇ гілки, потім добираємо за score.
func keepDiverse(sorted []Candidate, branchOf map[string]string, n int) []Candidate {
	if len(sorted) <= n {
		return sorted
	}
	var out []Candidate
	taken := map[string]bool{}
	seenBranch := map[string]bool{}
	for _, c := range sorted {
		b := branchOf[c.ChunkID]
		if seenBranch[b] {
			continue
		}
		seenBranch[b] = true
		taken[c.ChunkID] = true
		out = append(out, c)
		if len(out) == n {
			return out
		}
	}
	for _, c := range sorted {
		if taken[c.ChunkID] {
			continue
		}
		out = append(out, c)
		if len(out) == n {
			break
		}
	}
	sortByScore(out)
	return out
}

// rerankTrace зберігає порядок «до/після» останнього прогону — саме його
// вимагає README (5 запитів із таблицею змін).
type rerankTrace struct {
	Query  string   `json:"query"`
	Class  string   `json:"class"`
	Steps  int      `json:"steps"`
	Before []string `json:"before"`
	After  []string `json:"after"`
}

func idsOf(c []Candidate) []string {
	out := make([]string, len(c))
	for i := range c {
		out[i] = c[i].ChunkID
	}
	return out
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// format складає відповідь із кількох кандидатів із цитатами.
//
// Якщо доказів немає — так і каже. Це термінальний стан бюджету з Homework
// п.1: агент, який на порожній retrieval відповідає зв'язним текстом, гірший
// за агента, який мовчить, бо його помилку ніхто не помітить.
func (r *retriever) format(ctx agent.Context, in SearchResult) (string, error) {
	if len(in.Candidates) == 0 {
		return "Недостатньо доказів: у корпусі немає фрагментів, які відповідають на цей запит.", nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[cache_hit=false] Знайдено %d фрагментів:\n", len(in.Candidates))
	for i, c := range in.Candidates {
		fmt.Fprintf(&b, "\n%d. [%s · %s · %s] score=%.3f\n%s\n",
			i+1, c.ChunkID, c.DocumentID, c.SectionPath, c.Score, strings.TrimSpace(c.Text))
	}
	answer := b.String()
	r.rememberAnswer(ctx, in.Query, answer)
	return answer, nil
}

// --- Семантичний кеш як вузол ------------------------------------------------

// tenantID та corpusVersion — межі ключа кеша.
//
// Поки константи, бо корпус один і клієнт один. Але ключ уже містить обидва
// поля: додати мультитенантність пізніше — це передати параметр, а не
// переписати кеш і не виявити одного дня, що один клієнт бачить відповіді
// іншого.
const (
	tenantID      = "ledgerworks"
	corpusVersion = "week3-day5"
)

// cacheCheck — перший вузол графа. Емітує маршрут hit або miss.
//
// Кешуємо ФІНАЛЬНУ відповідь, а не проміжні RetrievalResult. Причина проста:
// проміжні результати доведеться перевалідовувати проти корпусу (а раптом
// чанк змінився), і економія зникає. Фінальна відповідь самодостатня — у ній
// уже є цитати, за якими її можна перевірити.
// CacheProbe — вихід cache_check.
//
// Структура, а не рядок, і це не естетика: вузол, що повертає string, launcher
// показує як готову відповідь агента, і користувач бачить відлуння власного
// запиту замість результату. Типізований вихід у текст не перетворюється.
type CacheProbe struct {
	Query  string `json:"query"`
	Hit    bool   `json:"hit"`
	Answer string `json:"answer,omitempty"`
}

func (r *retriever) cacheCheck(ctx agent.Context, query string, emit func(*session.Event) error) (CacheProbe, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return CacheProbe{}, fmt.Errorf("порожній запит")
	}

	probe := CacheProbe{Query: q}
	route := "miss"
	if vecs, err := r.emb.Embed(ctx, []string{q}); err == nil && len(vecs) == 1 {
		if hit, score, ok := r.cache.getSemantic(cacheKey(tenantID, corpusVersion), q, vecs[0]); ok {
			route, probe.Hit, probe.Answer = "hit", true, hit
			r.lastCache = cacheTrace{Query: q, Hit: true, Score: score}
		} else {
			r.lastCache = cacheTrace{Query: q, Hit: false, Score: score}
		}
	}

	// Тільки маршрут — див. коментар у classify.
	ev := session.NewEvent(ctx, ctx.InvocationID())
	ev.Routes = []string{route}
	if err := emit(ev); err != nil {
		return CacheProbe{}, err
	}
	return probe, nil
}

// cacheTrace — що сталося з кешем на останньому запиті (для README).
type cacheTrace struct {
	Query string  `json:"query"`
	Hit   bool    `json:"hit"`
	Score float64 `json:"score"`
}

// serveFromCache — термінальний вузол гілки hit.
func (r *retriever) serveFromCache(_ agent.Context, p CacheProbe) (string, error) {
	return p.Answer, nil
}

// rememberAnswer кладе відповідь у семантичний кеш після повного прогону.
func (r *retriever) rememberAnswer(ctx agent.Context, query, answer string) {
	if vecs, err := r.emb.Embed(ctx, []string{query}); err == nil && len(vecs) == 1 {
		r.cache.putSemantic(cacheKey(tenantID, corpusVersion), query, vecs[0], strings.Replace(answer, "[cache_hit=false]", "[cache_hit=true]", 1))
	}
}
