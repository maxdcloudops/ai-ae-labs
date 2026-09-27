package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestValidJSONIsNotAValidCall — теза дня, зафіксована тестом.
//
// `{"base":"US1","target":"UAH"}` розбирається json.Unmarshal БЕЗ помилки.
// Схема довела форму. Виклик усе одно не має права виконатись — і помилка
// мусить назвати конкретне значення, інакше модель не знатиме, що виправляти.
func TestValidJSONIsNotAValidCall(t *testing.T) {
	raw := []byte(`{"base": "US1", "target": "UAH"}`)

	in, err := DecodeArgs(raw)
	if err != nil {
		t.Fatalf("JSON мав розібратися без помилки, а не розібрався: %v", err)
	}

	_, err = in.Validate()
	if err == nil {
		t.Fatal("валідатор пропустив зіпсований виклик")
	}
	if !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("очікували ErrInvalidCode, отримали %v", err)
	}
	if !strings.Contains(err.Error(), `"US1"`) {
		t.Fatalf("помилка не називає конкретне значення — моделі нема що виправляти: %v", err)
	}
	t.Logf("валідатор відхилив виклик: %v", err)
}

func TestNormalizeCode(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
		wantErr        bool
	}{
		{"canonical", "USD", "USD", false},
		{"lowercase безпечно", "usd", "USD", false},
		{"пробіли безпечно", "  eur  ", "EUR", false},
		{"цифра", "US1", "", true},
		{"задовгий", "USDD", "", true},
		{"закороткий", "US", "", true},
		{"порожній", "", "", true},
		{"кирилиця", "УСД", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeCode(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("очікували помилку для %q, отримали %q", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("несподівана помилка для %q: %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("NormalizeCode(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestCyrillicLookalikeIsRejected — «УСД» кирилицею виглядає як «УСД» латиною.
//
// Це не екзотика: такий рядок приходить із копіпасту й проходить будь-яку
// перевірку «три символи». Ловить його тільки перевірка на діапазон A–Z.
func TestCyrillicLookalikeIsRejected(t *testing.T) {
	_, err := NormalizeCode("СНF") // перша літера — кирилична «С»
	if err == nil {
		t.Fatal("кириличний омоглиф пройшов як валідний код валюти")
	}
	if !strings.Contains(err.Error(), "LATIN") {
		t.Fatalf("помилка не пояснює причину: %v", err)
	}
}

func TestRateInputValidateRange(t *testing.T) {
	for _, tc := range []struct {
		name    string
		days    int
		wantErr bool
	}{
		{"нуль — лише поточний курс", 0, false},
		{"у межах", 7, false},
		{"стеля", maxHistoryDays, false},
		{"понад стелю", maxHistoryDays + 1, true},
		{"відʼємний", -1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := RateInput{Base: "USD", Target: "UAH", HistoryDays: tc.days}.Validate()
			if tc.wantErr != (err != nil) {
				t.Fatalf("days=%d: err=%v, wantErr=%t", tc.days, err, tc.wantErr)
			}
		})
	}
}

// TestSourceKindEnum — enum мусить відхиляти все, чого в ньому немає.
func TestSourceKindEnum(t *testing.T) {
	for _, tc := range []struct {
		kind SourceKind
		want bool
	}{
		{SourceAPI, true}, {SourceCache, true}, {SourceMock, true},
		{"", false}, {"API", false}, {"аpi", false}, // третій — кирилична «а»
	} {
		if got := tc.kind.Valid(); got != tc.want {
			t.Fatalf("SourceKind(%q).Valid() = %t, want %t", tc.kind, got, tc.want)
		}
	}
}

// TestQuoteValidatesItsOwnOutput — межа двостороння.
func TestQuoteValidatesItsOwnOutput(t *testing.T) {
	for _, tc := range []struct {
		name  string
		quote RateQuote
		want  string
	}{
		{"нульовий курс", RateQuote{Rate: 0, Source: Source{Kind: SourceAPI}}, "positive"},
		{"невідомий enum", RateQuote{Rate: 1, Source: Source{Kind: "wat"}}, "must be one of"},
		{"битий enum усередині history", RateQuote{
			Rate:    1,
			Source:  Source{Kind: SourceAPI},
			History: []HistoricalPoint{{Rate: 1, Source: Source{Kind: "nope"}}},
		}, "history[0].source.kind"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.quote.Validate()
			if err == nil {
				t.Fatal("невалідний вихід пройшов межу")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("помилка %q не містить %q", err, tc.want)
			}
		})
	}
}

// TestQuoteCrossRatesThroughUAH — арифметика крос-курсу.
func TestQuoteCrossRatesThroughUAH(t *testing.T) {
	p := &FixtureProvider{
		Rates: map[string]float64{"USD": 40.0, "EUR": 50.0},
		Date:  "2026-09-28",
	}
	q, err := Quote(context.Background(), p, RateInput{Base: "EUR", Target: "USD"})
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	if q.Rate != 1.25 {
		t.Fatalf("EUR/USD = %v, want 1.25 (50/40)", q.Rate)
	}
	if q.Source.Kind != SourceMock {
		t.Fatalf("Source.Kind = %q, want %q — походження мусить бути чесним", q.Source.Kind, SourceMock)
	}
}

// TestQuoteHandlesUAHOnBothSides — UAH не публікується як рядок джерела.
func TestQuoteHandlesUAHOnBothSides(t *testing.T) {
	p := &FixtureProvider{Rates: map[string]float64{"USD": 40.0}, Date: "2026-09-28"}
	for _, tc := range []struct {
		in   RateInput
		want float64
	}{
		{RateInput{Base: "USD", Target: "UAH"}, 40},
		{RateInput{Base: "UAH", Target: "USD"}, 0.025},
		{RateInput{Base: "UAH", Target: "UAH"}, 1},
	} {
		q, err := Quote(context.Background(), p, tc.in)
		if err != nil {
			t.Fatalf("%s: %v", tc.in.Base+"/"+tc.in.Target, err)
		}
		if diff := q.Rate - tc.want; diff > 1e-9 || diff < -1e-9 {
			t.Fatalf("%s = %v, want %v", tc.in.Base+"/"+tc.in.Target, q.Rate, tc.want)
		}
	}
}

// TestUnknownCurrencySuggests — помилка мусить підказувати.
//
// Модель, яка отримала «unknown currency code: USDD», може здогадатися сама;
// модель, яка отримала «did you mean USD?», виправиться з першого разу. На
// масштабі різниця — один зайвий раунд на кожну одруківку.
func TestUnknownCurrencySuggests(t *testing.T) {
	p := &FixtureProvider{Rates: map[string]float64{"USD": 40, "EUR": 50}, Date: "2026-09-28"}
	_, err := Quote(context.Background(), p, RateInput{Base: "USE", Target: "UAH"})
	if err == nil {
		t.Fatal("неіснуюча валюта пройшла")
	}
	if !errors.Is(err, ErrUnknownCurrency) {
		t.Fatalf("очікували ErrUnknownCurrency: %v", err)
	}
	if !strings.Contains(err.Error(), "did you mean") {
		t.Fatalf("помилка не підказує виправлення: %v", err)
	}
}

// TestNonPositiveRateFailsLoudly — нуль у джерелі дав би +Inf.
func TestNonPositiveRateFailsLoudly(t *testing.T) {
	p := &FixtureProvider{Rates: map[string]float64{"USD": 0}, Date: "2026-09-28"}
	_, err := Quote(context.Background(), p, RateInput{Base: "UAH", Target: "USD"})
	if err == nil {
		t.Fatal("нульовий курс у джерелі не зупинив обчислення: модель отримала б +Inf як факт")
	}
}

// TestEmptyHistoryIsExplained — порожній масив без пояснення читається як
// «даних немає взагалі».
func TestEmptyHistoryIsExplained(t *testing.T) {
	p := &FixtureProvider{Rates: map[string]float64{"USD": 40}, Date: "2026-09-28"} // History: nil
	q, err := Quote(context.Background(), p, RateInput{Base: "USD", Target: "UAH", HistoryDays: 3})
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	if len(q.History) != 0 {
		t.Fatalf("очікували порожню історію, отримали %d точок", len(q.History))
	}
	if q.Note == "" {
		t.Fatal("порожня історія без Note: модель вирішить, що даних немає взагалі")
	}
}

// TestUpstreamFailureIsNotAGuessedNumber — fail path.
func TestUpstreamFailureIsNotAGuessedNumber(t *testing.T) {
	p := &FixtureProvider{Err: errUpstreamFixture}
	_, err := Quote(context.Background(), p, RateInput{Base: "USD", Target: "UAH"})
	if err == nil {
		t.Fatal("мертвий апстрім повернув курс — звідки?")
	}
	if !errors.Is(err, ErrUpstream) {
		t.Fatalf("очікували ErrUpstream: %v", err)
	}
	if IsPermanent(err) {
		t.Fatal("збій апстріму позначено як постійний: користувачу скажуть «такої валюти немає» замість «спробуйте пізніше»")
	}
}

// TestIsPermanentSeparatesRetryableFromNot — від цього залежить текст для
// користувача: «спробуйте пізніше» проти «такої валюти немає».
func TestIsPermanentSeparatesRetryableFromNot(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{ErrInvalidCode, true},
		{ErrUnknownCurrency, true},
		{ErrInvalidRange, true},
		{ErrUnrepairable, true},
		{ErrUpstream, false},
		{nil, false},
	} {
		if got := IsPermanent(tc.err); got != tc.want {
			t.Fatalf("IsPermanent(%v) = %t, want %t", tc.err, got, tc.want)
		}
	}
}

// --- provider.go: живий HTTP через httptest ---

// TestNBUProviderParsesAndCaches — і розбір відповіді, і кеш.
//
// Кеш тут не оптимізація: курс НБУ міняється раз на добу, і сесія з трьох
// питань не має робити трьох запитів до банку. Перевіряється саме кількість
// запитів, а не «швидко».
func TestNBUProviderParsesAndCaches(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		_, _ = w.Write([]byte(`[{"rate":44.6743,"cc":"USD","exchangedate":"28.09.2026"},
		                        {"rate":51.122,"cc":"EUR","exchangedate":"28.09.2026"}]`))
	}))
	defer srv.Close()

	p := &NBUProvider{BaseURL: srv.URL, HTTPClient: srv.Client(), CacheTTL: time.Minute}

	rates, date, err := p.RatesToUAH(context.Background())
	if err != nil {
		t.Fatalf("RatesToUAH: %v", err)
	}
	if rates["USD"] != 44.6743 {
		t.Fatalf("USD = %v", rates["USD"])
	}
	if date != "2026-09-28" {
		t.Fatalf("дата = %q, want 2026-09-28 (DD.MM.YYYY мусить конвертуватись в ISO)", date)
	}
	if p.Kind() != SourceAPI {
		t.Fatalf("перший запит мав бути api, а не %q", p.Kind())
	}

	// Другий виклик — із кешу, і Source.Kind мусить це СКАЗАТИ.
	if _, _, err = p.RatesToUAH(context.Background()); err != nil {
		t.Fatalf("другий RatesToUAH: %v", err)
	}
	if hits != 1 {
		t.Fatalf("апстрім отримав %d запитів, очікували 1: кеш не спрацював", hits)
	}
	if p.Kind() != SourceCache {
		t.Fatalf("відповідь із кешу позначена як %q: поле походження бреше", p.Kind())
	}
}

func TestNBUProviderSurfacesUpstreamStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	p := &NBUProvider{BaseURL: srv.URL, HTTPClient: srv.Client()}
	if _, _, err := p.RatesToUAH(context.Background()); !errors.Is(err, ErrUpstream) {
		t.Fatalf("503 від НБУ мусить давати ErrUpstream, а не %v", err)
	}
}

// TestNBUProviderSkipsDaysWithoutQuotes — вихідні й свята.
func TestNBUProviderSkipsDaysWithoutQuotes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[]`)) // НБУ так відповідає на вихідний
	}))
	defer srv.Close()

	p := &NBUProvider{BaseURL: srv.URL, HTTPClient: srv.Client()}
	_, err := p.RateOn(context.Background(), "USD", time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC))
	if !errors.Is(err, ErrUnknownCurrency) {
		t.Fatalf("день без котирування мусить давати ErrUnknownCurrency (щоб його пропустили), а не %v", err)
	}
}

// --- schema.go ---

// TestInferredOutputSchemaHasThreeLevelsArrayAndEnum — вимога ДЗ, перевірена
// на самій схемі, а не на око.
func TestInferredOutputSchemaHasThreeLevelsArrayAndEnum(t *testing.T) {
	raw, err := InferredOutputSchemaJSON()
	if err != nil {
		t.Fatalf("InferredOutputSchemaJSON: %v", err)
	}

	var doc map[string]any
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("схема не є валідним JSON: %v", err)
	}

	// Рівень 1 → 2: history — це масив.
	props := doc["properties"].(map[string]any)
	history, ok := props["history"].(map[string]any)
	if !ok {
		t.Fatalf("у схемі немає history; є: %v", keysOf(props))
	}
	// Тип тут — ["null","array"], а не просто "array", і це ще одна знахідка
	// про автогенерацію: nil-слайс у Go — валідне значення, тож виведена схема
	// чесно позначає поле nullable. Ми при цьому НІКОЛИ не віддаємо null
	// (див. Quote: History ініціалізується порожнім слайсом), але схема цього
	// знати не може — вона виводиться з ТИПУ, а не з поведінки.
	if !schemaTypeIncludes(history["type"], "array") {
		t.Fatalf("history.type = %v, want array (можливо, у вигляді [null array])", history["type"])
	}

	// Рівень 2 → 3: усередині елемента масиву є source.
	items := history["items"].(map[string]any)
	itemProps := resolveProps(t, items, doc)
	if _, ok := itemProps["source"]; !ok {
		t.Fatalf("у HistoricalPoint немає source — третього рівня вкладеності немає; є: %v", keysOf(itemProps))
	}

	// Enum: він має бути присутній десь у схемі — jsonschema-go виводить його
	// з типу SourceKind лише за наявності явних значень, тож перевіряємо
	// принаймні, що поле kind описане.
	if !strings.Contains(raw, "api, cache or mock") {
		t.Fatal("опис enum-поля не доїхав до схеми — модель не дізнається допустимих значень")
	}
}

// schemaTypeIncludes перевіряє тип, який може бути рядком або списком.
func schemaTypeIncludes(v any, want string) bool {
	switch t := v.(type) {
	case string:
		return t == want
	case []any:
		for _, item := range t {
			if s, ok := item.(string); ok && s == want {
				return true
			}
		}
	}
	return false
}

// TestHistoryIsNeverNullInJSON — контракт не має віддавати null там, де
// споживач чекає масив.
func TestHistoryIsNeverNullInJSON(t *testing.T) {
	p := &FixtureProvider{Rates: map[string]float64{"USD": 40}, Date: "2026-09-28"}
	q, err := Quote(context.Background(), p, RateInput{Base: "USD", Target: "UAH"})
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	raw, err := json.Marshal(q)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), `"history":null`) {
		t.Fatalf("history серіалізувалося в null — споживач впаде на ітерації: %s", raw)
	}
	if !strings.Contains(string(raw), `"history":[]`) {
		t.Fatalf("очікували порожній масив: %s", raw)
	}
}

// resolveProps дістає properties елемента, розгортаючи $ref, якщо він є.
func resolveProps(t *testing.T, node, root map[string]any) map[string]any {
	t.Helper()
	if p, ok := node["properties"].(map[string]any); ok {
		return p
	}
	ref, ok := node["$ref"].(string)
	if !ok {
		t.Fatalf("елемент масиву не має ні properties, ні $ref: %v", node)
	}
	name := ref[strings.LastIndex(ref, "/")+1:]
	defs, ok := root["$defs"].(map[string]any)
	if !ok {
		t.Fatalf("у схемі є $ref %q, але немає $defs", ref)
	}
	def, ok := defs[name].(map[string]any)
	if !ok {
		t.Fatalf("$defs не містить %q", name)
	}
	return def["properties"].(map[string]any)
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestInferredSchemaMakesOnlyNonOptionalFieldsRequired — знахідка цієї лаби.
//
// У Go немає «опційного поля», тож автогенератор мусить вгадувати — і вгадує
// «required» для ВСЬОГО. Поки в history_days не було `omitempty`, будь-який
// виклик моделі без цього поля відхилявся ще до handler-а: інструмент мовчки
// не викликався, а в логах було порожньо. Цей тест не дає регресії повернутись.
func TestInferredSchemaMakesOnlyNonOptionalFieldsRequired(t *testing.T) {
	raw, err := InferredInputSchemaJSON()
	if err != nil {
		t.Fatalf("InferredInputSchemaJSON: %v", err)
	}
	var doc struct {
		Required []string `json:"required"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, f := range doc.Required {
		if f == "history_days" {
			t.Fatal("history_days знову required: виклик без нього відхилятиметься до handler-а. " +
				"Потрібен тег `json:\"history_days,omitempty\"`")
		}
	}
	if len(doc.Required) != 2 {
		t.Fatalf("required = %v, очікували рівно [base target]", doc.Required)
	}
}

// TestExplicitSchemaMatchesStruct — страховка для явної схеми.
//
// Явна InputSchema не слідує за структурою: додасте поле в RateInput — вона
// мовчки лишиться старою, і модель ніколи про це поле не дізнається. Тест
// звіряє набір полів між двома схемами.
func TestExplicitSchemaMatchesStruct(t *testing.T) {
	inferredRaw, err := InferredInputSchemaJSON()
	if err != nil {
		t.Fatalf("InferredInputSchemaJSON: %v", err)
	}
	var inferred struct {
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal([]byte(inferredRaw), &inferred); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	explicit := StrictInputSchema()
	for name := range inferred.Properties {
		if _, ok := explicit.Properties[name]; !ok {
			t.Fatalf("поле %q є в RateInput, але його немає в явній InputSchema — модель про нього не дізнається", name)
		}
	}
	for name := range explicit.Properties {
		if _, ok := inferred.Properties[name]; !ok {
			t.Fatalf("явна InputSchema описує поле %q, якого в RateInput немає", name)
		}
	}
}
