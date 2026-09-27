// Джерело курсів: НБУ наживо + фікстура для офлайну, з кешем між ними.
//
// НБУ обрано свідомо: публічний ендпоінт БЕЗ ключа, тобто «живий» шлях цієї
// лаби запускається в будь-кого з першої спроби. Це реальний сервіс із
// реальними режимами відмови, а не іграшка.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Provider віддає курси валют відносно гривні — це вісь, у якій НБУ публікує
// свою таблицю. Інтерфейс тут для того, щоб тести й офлайн-режим працювали без
// мережі, а прод — проти справжнього банку.
type Provider interface {
	// Name — мітка походження, яка потрапляє в Source.Name.
	Name() string
	// Kind — вид джерела для enum Source.Kind.
	Kind() SourceKind
	// RatesToUAH повертає «скільки гривень коштує одиниця валюти» і дату.
	RatesToUAH(ctx context.Context) (map[string]float64, string, error)
	// RateOn повертає курс однієї валюти до гривні на конкретну дату.
	// Використовується для History; повертає ErrUnknownCurrency, якщо на цю
	// дату котирування немає (вихідні, свята).
	RateOn(ctx context.Context, code string, day time.Time) (float64, error)
}

const nbuBaseURL = "https://bank.gov.ua/NBUStatService/v1/statdirectory/exchange"

// NBUProvider читає публічний довідник курсів Національного банку України.
type NBUProvider struct {
	// BaseURL перевизначається в тестах на httptest-сервер.
	BaseURL string
	// HTTPClient за замовчуванням має таймаут. http.DefaultClient тут
	// використовувати не можна: у нього таймауту немає взагалі, і один
	// завислий апстрім тече горутиною назавжди.
	HTTPClient *http.Client

	// mu захищає кеш: інструмент викликається з горутин ADK.
	mu    sync.Mutex
	cache map[string]cacheEntry
	// CacheTTL — скільки жити запису. Курс НБУ міняється раз на добу, тож
	// сесія з трьох питань не має робити трьох запитів до банку.
	CacheTTL time.Duration
	// served показує, чи останню відповідь віддано з кешу — саме це значення
	// їде в Source.Kind і робить походження числа чесним.
	served SourceKind
}

type cacheEntry struct {
	rates map[string]float64
	date  string
	at    time.Time
}

// Name реалізує Provider.
func (p *NBUProvider) Name() string { return "nbu" }

// Kind повідомляє, звідки взято ОСТАННЮ відповідь.
//
// Не константа: якщо провайдер віддав число з кешу, а Source.Kind каже "api",
// відповідь бреше саме там, де користувач має право знати правду — у полі
// походження.
func (p *NBUProvider) Kind() SourceKind {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.served == "" {
		return SourceAPI
	}
	return p.served
}

func (p *NBUProvider) client() *http.Client {
	if p.HTTPClient != nil {
		return p.HTTPClient
	}
	return &http.Client{Timeout: 10 * time.Second}
}

func (p *NBUProvider) baseURL() string {
	if p.BaseURL != "" {
		return p.BaseURL
	}
	return nbuBaseURL
}

func (p *NBUProvider) ttl() time.Duration {
	if p.CacheTTL <= 0 {
		return time.Minute
	}
	return p.CacheTTL
}

// nbuRow — один рядок відповіді НБУ.
type nbuRow struct {
	Rate         float64 `json:"rate"`
	CC           string  `json:"cc"`
	ExchangeDate string  `json:"exchangedate"` // DD.MM.YYYY
}

// RatesToUAH реалізує Provider.
func (p *NBUProvider) RatesToUAH(ctx context.Context) (map[string]float64, string, error) {
	rows, cached, err := p.fetch(ctx, p.baseURL()+"?json")
	if err != nil {
		return nil, "", err
	}
	if len(rows) == 0 {
		return nil, "", fmt.Errorf("%w: empty rate directory", ErrUpstream)
	}

	p.mu.Lock()
	p.served = SourceAPI
	if cached {
		p.served = SourceCache
	}
	p.mu.Unlock()

	out := make(map[string]float64, len(rows))
	for _, r := range rows {
		out[strings.ToUpper(r.CC)] = r.Rate
	}
	return out, isoDate(rows[0].ExchangeDate), nil
}

// RateOn реалізує Provider — курс однієї валюти на конкретну дату.
func (p *NBUProvider) RateOn(ctx context.Context, code string, day time.Time) (float64, error) {
	if code == "UAH" {
		return 1, nil
	}
	url := fmt.Sprintf("%s?valcode=%s&date=%s&json", p.baseURL(), code, day.Format("20060102"))
	rows, _, err := p.fetch(ctx, url)
	if err != nil {
		return 0, err
	}
	// Порожня відповідь — це не помилка апстріму, а вихідний або свято:
	// НБУ в такі дні курсу не публікує. Повертаємо ErrUnknownCurrency, і
	// викликач пропускає цей день, а не вигадує число.
	if len(rows) == 0 || rows[0].Rate <= 0 {
		return 0, fmt.Errorf("%w: no NBU quote for %s on %s", ErrUnknownCurrency, code, day.Format("2006-01-02"))
	}
	return rows[0].Rate, nil
}

// fetch виконує запит із кешем, повертаючи ще й те, чи відповідь із кешу.
func (p *NBUProvider) fetch(ctx context.Context, url string) (rows []nbuRow, fromCache bool, err error) {
	p.mu.Lock()
	if p.cache == nil {
		p.cache = map[string]cacheEntry{}
	}
	if e, ok := p.cache[url]; ok && time.Since(e.at) < p.ttl() {
		cached := make([]nbuRow, 0, len(e.rates))
		for cc, rate := range e.rates {
			cached = append(cached, nbuRow{CC: cc, Rate: rate, ExchangeDate: e.date})
		}
		// Мапа не має порядку, а RatesToUAH бере дату з rows[0] — без
		// сортування дата стала б випадковою між прогонами.
		sort.Slice(cached, func(i, j int) bool { return cached[i].CC < cached[j].CC })
		p.mu.Unlock()
		return cached, true, nil
	}
	p.mu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, false, fmt.Errorf("%w: build request: %v", ErrUpstream, err)
	}
	resp, err := p.client().Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("%w: unexpected status %s", ErrUpstream, resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		return nil, false, fmt.Errorf("%w: decode: %v", ErrUpstream, err)
	}

	if len(rows) > 0 {
		p.mu.Lock()
		cached := make(map[string]float64, len(rows))
		for _, r := range rows {
			cached[strings.ToUpper(r.CC)] = r.Rate
		}
		p.cache[url] = cacheEntry{rates: cached, date: rows[0].ExchangeDate, at: time.Now()}
		p.mu.Unlock()
	}
	return rows, false, nil
}

// isoDate конвертує DD.MM.YYYY у YYYY-MM-DD, лишаючи нерозпізнане як є —
// вигадувати дату гірше, ніж показати її у форматі джерела.
func isoDate(ddmmyyyy string) string {
	t, err := time.Parse("02.01.2006", ddmmyyyy)
	if err != nil {
		return ddmmyyyy
	}
	return t.Format("2006-01-02")
}

// FixtureProvider — офлайн-джерело для тестів і демо без мережі.
type FixtureProvider struct {
	Rates   map[string]float64
	Date    string
	History map[string]map[string]float64 // code → YYYY-MM-DD → rate
	Err     error
	// KindOverride підміняє походження, щоб змоделювати провайдера, який
	// віддає значення поза enum. Потрібно для fault injection: без цього
	// сценарій «невідомий enum» неможливо прогнати наживо, бо всі наші
	// провайдери за побудовою повертають валідні значення — а от чужий
	// MCP-сервер за тиждень може повернути будь-що.
	KindOverride SourceKind
}

// Name реалізує Provider.
func (p *FixtureProvider) Name() string { return "fixture" }

// Kind реалізує Provider.
func (p *FixtureProvider) Kind() SourceKind {
	if p.KindOverride != "" {
		return p.KindOverride
	}
	return SourceMock
}

// RatesToUAH реалізує Provider.
func (p *FixtureProvider) RatesToUAH(context.Context) (map[string]float64, string, error) {
	if p.Err != nil {
		return nil, "", p.Err
	}
	return p.Rates, p.Date, nil
}

// RateOn реалізує Provider.
func (p *FixtureProvider) RateOn(_ context.Context, code string, day time.Time) (float64, error) {
	if p.Err != nil {
		return 0, p.Err
	}
	if code == "UAH" {
		return 1, nil
	}
	if byDay, ok := p.History[code]; ok {
		if rate, ok := byDay[day.Format("2006-01-02")]; ok {
			return rate, nil
		}
	}
	return 0, fmt.Errorf("%w: no fixture quote for %s on %s", ErrUnknownCurrency, code, day.Format("2006-01-02"))
}

// Quote збирає повний RateQuote: поточний курс + історія.
//
// Крос-курс рахується через гривню, бо саме в цій осі НБУ публікує таблицю.
// UAH має курс 1.0 до себе, тож UAH→EUR і EUR→UAH працюють без окремого рядка
// в джерелі.
//
// Вхід сюди приходить УЖЕ валідований (RateInput.Validate): функція, яка і
// валідує, і працює, рано чи пізно викликається в обхід валідації.
func Quote(ctx context.Context, p Provider, in RateInput) (RateQuote, error) {
	rates, asOf, err := p.RatesToUAH(ctx)
	if err != nil {
		return RateQuote{}, fmt.Errorf("%w: %v", ErrUpstream, err)
	}

	baseUAH, err := rateToUAH(in.Base, rates)
	if err != nil {
		return RateQuote{}, err
	}
	targetUAH, err := rateToUAH(in.Target, rates)
	if err != nil {
		return RateQuote{}, err
	}

	src := Source{Kind: p.Kind(), Name: p.Name(), FetchedAt: nowRFC3339()}
	quote := RateQuote{
		Pair:   Pair{Base: in.Base, Target: in.Target},
		Rate:   baseUAH / targetUAH,
		AsOf:   asOf,
		Source: src,
		// Порожній масив, а не nil. У Go це майже те саме, у JSON — ні:
		// nil-слайс серіалізується в `null`, і споживач, який робить
		// `for _, p := range q.history`, отримує TypeError замість нуля
		// ітерацій. Контракт має бути зручним для того, хто його читає.
		History: []HistoricalPoint{},
	}

	if in.HistoryDays > 0 {
		quote.History = history(ctx, p, in, src)
		if len(quote.History) == 0 {
			// Порожня історія при запитаній історії — це факт, який треба
			// сказати, а не приховати. Модель, яка бачить порожній масив без
			// пояснення, вирішує, що даних немає взагалі.
			quote.Note = fmt.Sprintf(
				"history was requested for %d day(s) but no quotes were published in that window (weekends and holidays have no NBU rate)",
				in.HistoryDays)
		}
	}

	// Вихід валідується перед віддачею: межа двостороння.
	if err := quote.Validate(); err != nil {
		return RateQuote{}, err
	}
	return quote, nil
}

// history збирає точки історії, мовчки пропускаючи дні без котирувань.
//
// Пропуск тут правильний, а не лінивий: у вихідні НБУ курсу не публікує, і
// «заповнити» їх учорашнім числом означало б вигадати котирування, якого не
// було. Кожна точка несе власний Source — див. коментар до HistoricalPoint.
func history(ctx context.Context, p Provider, in RateInput, src Source) []HistoricalPoint {
	var points []HistoricalPoint
	today := time.Now().UTC()
	for i := in.HistoryDays; i >= 1; i-- {
		day := today.AddDate(0, 0, -i)

		baseRate, err := p.RateOn(ctx, in.Base, day)
		if err != nil {
			continue
		}
		targetRate, err := p.RateOn(ctx, in.Target, day)
		if err != nil || targetRate <= 0 {
			continue
		}
		points = append(points, HistoricalPoint{
			Date:   day.Format("2006-01-02"),
			Rate:   baseRate / targetRate,
			Source: Source{Kind: p.Kind(), Name: src.Name, FetchedAt: nowRFC3339()},
		})
	}
	return points
}

// rateToUAH резолвить вартість однієї валюти в гривні.
func rateToUAH(code string, rates map[string]float64) (float64, error) {
	if code == "UAH" {
		return 1, nil
	}
	rate, ok := rates[code]
	if !ok {
		known := make([]string, 0, len(rates))
		for k := range rates {
			known = append(known, k)
		}
		if hints := suggestCodes(code, known); len(hints) > 0 {
			return 0, fmt.Errorf("%w: %s is not published by this provider; did you mean %s?",
				ErrUnknownCurrency, code, strings.Join(hints, " or "))
		}
		return 0, fmt.Errorf("%w: %s is not published by this provider", ErrUnknownCurrency, code)
	}
	if rate <= 0 {
		// Нуль або від'ємне дало б +Inf чи від'ємну ціну. Падаємо гучно, а не
		// віддаємо моделі безглузде число, яке вона впевнено покаже як факт.
		return 0, fmt.Errorf("%w: %s has non-positive rate %v", ErrUpstream, code, rate)
	}
	return rate, nil
}

// errUpstreamFixture — готова помилка апстріму для fault-injection тестів.
var errUpstreamFixture = errors.New("simulated upstream outage")
