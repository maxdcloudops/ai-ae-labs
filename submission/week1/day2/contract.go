// ДЗ 2 — Strict Schema Enforcer: контракт інструмента курсу валют.
//
// Цей файл не знає ні про ADK, ні про модель, ні про мережу. У ньому живе
// тільки контракт і його валідація — і саме тому його можна покрити тестами
// без ключа, без агента й без інтернету.
//
// Головна теза дня в одному рядку: **валідний JSON ≠ валідний виклик**.
// `{"base":"US1","target":"UAH"}` розбирається `json.Unmarshal` без жодної
// скарги. Схема доводить ФОРМУ. Чи має це значення в домені — доводить
// валідація, і вона окремо.
package main

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Типізовані помилки межі.
//
// Текст цих помилок їде назад у модель як спостереження, і саме за ним вона
// вирішує, виправити аргументи чи здатися. Тому кожна з них називає КОНКРЕТНЕ
// значення, а не «bad request»: «unknown currency code: XYZ» дає моделі те, що
// виправляти, а «invalid arguments» — не дає нічого, і наступна спроба буде
// такою самою.
var (
	// ErrInvalidCode — код не відповідає формі ISO 4217 (три латинські літери).
	ErrInvalidCode = errors.New("invalid currency code")
	// ErrUnknownCurrency — форма правильна, але такої валюти в джерелі немає.
	ErrUnknownCurrency = errors.New("unknown currency code")
	// ErrInvalidRange — запит історії поза допустимим діапазоном.
	ErrInvalidRange = errors.New("invalid history range")
	// ErrUpstream — джерело курсів недоступне або відповіло сміттям.
	ErrUpstream = errors.New("rate provider unavailable")
	// ErrUnrepairable — аргументи неможливо виправити безпечно.
	ErrUnrepairable = errors.New("arguments cannot be repaired safely")
)

// maxHistoryDays — стеля глибини історії.
//
// Не магічне число, а рішення про гроші й про ліміти: кожен день історії — це
// окремий HTTP-запит до НБУ. Модель, яка попросить 3650 днів, зробила б 3650
// запитів, і зупинити її має контракт, а не сподівання.
const maxHistoryDays = 14

// RateInput — вхідний контракт інструмента.
//
// Тег `jsonschema` тут — це ОПИС поля, а не список обмежень. Це найчастіша
// помилка зі схемами ADK Go: `jsonschema:"required,enum=UAH"` дає інструмент,
// опис поля якого — літеральний рядок "required,enum=UAH", а порожній
// `jsonschema:""` взагалі валить збірку схеми. Обмеження живуть або в явній
// functiontool.Config.InputSchema (див. agent.go), або в доменній валідації
// нижче — і у нас вони в обох місцях, свідомо.
type RateInput struct {
	Base        string `json:"base" jsonschema:"ISO 4217 code of the base currency, three letters, e.g. USD"`
	Target      string `json:"target" jsonschema:"ISO 4217 code of the target currency, three letters, e.g. UAH"`
	HistoryDays int    `json:"history_days,omitempty" jsonschema:"How many recent business days of history to include, 0 to 14; 0 means current rate only"`
}

// SourceKind — enum походження числа.
//
// Enum, а не вільний рядок, тому що це поле читає не людина, а код: за ним
// вирішується, чи можна показати число користувачеві як офіційний курс.
// Рядок "аpi" (з кириличною «а») пройшов би як валідний JSON і зламався б
// мовчки через тиждень.
type SourceKind string

const (
	// SourceAPI — число щойно отримане з живого API.
	SourceAPI SourceKind = "api"
	// SourceCache — число з кешу процесу, не старше TTL.
	SourceCache SourceKind = "cache"
	// SourceMock — фікстура: офлайн-режим і тести.
	SourceMock SourceKind = "mock"
)

// SourceKinds — усі допустимі значення enum.
//
// Один список, з якого будується і валідація, і JSON Schema, і повідомлення про
// помилку. Три копії того самого переліку в трьох місцях розходяться завжди —
// питання лише в тому, через скільки тижнів.
var SourceKinds = []SourceKind{SourceAPI, SourceCache, SourceMock}

// Valid повідомляє, чи значення належить до enum.
func (k SourceKind) Valid() bool {
	for _, v := range SourceKinds {
		if k == v {
			return true
		}
	}
	return false
}

// Source — рівень 3 вкладеності: походження одного числа.
type Source struct {
	Kind      SourceKind `json:"kind" jsonschema:"Where this number came from: api, cache or mock"`
	Name      string     `json:"name" jsonschema:"Provider identifier, e.g. nbu"`
	FetchedAt string     `json:"fetched_at" jsonschema:"RFC3339 timestamp of when this number was obtained"`
}

// HistoricalPoint — рівень 2: один день історії.
//
// Кожна точка несе ВЛАСНИЙ Source, а не успадковує загальний. Це не
// надлишковість: сьогоднішній курс приходить із живого API, а вчорашній може
// приїхати з кешу. Один Source на всю відповідь брехав би про половину чисел.
type HistoricalPoint struct {
	Date   string  `json:"date" jsonschema:"Rate date, YYYY-MM-DD"`
	Rate   float64 `json:"rate" jsonschema:"How many units of target one unit of base buys on that date"`
	Source Source  `json:"source" jsonschema:"Provenance of this particular data point"`
}

// Pair — рівень 2: нормалізована валютна пара.
//
// Окремий тип, а не два рядки в корені, рівно тому, що пара — це одне поняття:
// її нормалізують, валідують і логують разом.
type Pair struct {
	Base   string `json:"base" jsonschema:"Normalized ISO 4217 base currency code"`
	Target string `json:"target" jsonschema:"Normalized ISO 4217 target currency code"`
}

// String повертає пару у звичній формі USD/UAH.
func (p Pair) String() string { return p.Base + "/" + p.Target }

// RateQuote — рівень 1: вихідний контракт інструмента.
//
// Три рівні вкладеності, масив і enum, як вимагає завдання:
//
//	RateQuote → History []HistoricalPoint → Source{Kind: "api"|"cache"|"mock"}
//
// Схема генерується автоматично з цих структур — жодного рукописного JSON
// Schema в проєкті немає (крім явної InputSchema в agent.go, яка існує заради
// порівняння двох підходів).
type RateQuote struct {
	Pair    Pair              `json:"pair" jsonschema:"The normalized currency pair this quote is about"`
	Rate    float64           `json:"rate" jsonschema:"Current rate: how many units of target one unit of base buys"`
	AsOf    string            `json:"as_of" jsonschema:"Date of the current rate, YYYY-MM-DD"`
	Source  Source            `json:"source" jsonschema:"Provenance of the current rate"`
	History []HistoricalPoint `json:"history" jsonschema:"Recent daily rates, oldest first; empty when history was not requested"`
	// Note — опційне поле, додане в §Schema-міграція README. Старий виклик
	// працює з новою схемою саме тому, що поле опційне: `omitempty` прибирає
	// його з відповіді, і клієнт, який про нього не знає, нічого не помічає.
	Note string `json:"note,omitempty" jsonschema:"Optional human-readable caveat about this quote"`
}

// Validate перевіряє ВИХІД перед тим, як віддати його моделі.
//
// Так, вихід теж валідується, і це не параноя. Інструмент, який повернув
// RateQuote з Source.Kind="аpi" (кирилична «а») або з від'ємним курсом,
// зламає споживача мовчки й далеко від місця помилки. Межа має бути
// двосторонньою: MCP-сервер робить рівно це — валідує вхід ДО виконання й
// санітизує вихід ПІСЛЯ.
func (q RateQuote) Validate() error {
	if q.Rate <= 0 {
		return fmt.Errorf("%w: rate must be positive, got %v", ErrUpstream, q.Rate)
	}
	if !q.Source.Kind.Valid() {
		return fmt.Errorf("%w: source.kind %q must be one of %s",
			ErrUpstream, q.Source.Kind, kindList())
	}
	for i, p := range q.History {
		if !p.Source.Kind.Valid() {
			return fmt.Errorf("%w: history[%d].source.kind %q must be one of %s",
				ErrUpstream, i, p.Source.Kind, kindList())
		}
		if p.Rate <= 0 {
			return fmt.Errorf("%w: history[%d].rate must be positive, got %v", ErrUpstream, i, p.Rate)
		}
	}
	return nil
}

// kindList друкує допустимі значення enum для повідомлення про помилку.
func kindList() string {
	out := make([]string, 0, len(SourceKinds))
	for _, k := range SourceKinds {
		out = append(out, string(k))
	}
	return strings.Join(out, ", ")
}

// NormalizeCode приводить код до канонічної форми й перевіряє його ФОРМУ.
//
// Верхній регістр і обрізані пробіли — це безпечна нормалізація: вона не може
// змінити зміст. "usd" і "USD" — та сама валюта, і відхиляти перший варіант
// означало б вимагати від моделі вгадати регістр.
//
// А от "US1" чи "доллар" безпечно нормалізувати НЕ можна, і саме тут проходить
// межа між ремонтом і відмовою: див. repair.go.
func NormalizeCode(code string) (string, error) {
	c := strings.ToUpper(strings.TrimSpace(code))
	if c == "" {
		return "", fmt.Errorf("%w: currency code is empty (expected three letters, e.g. USD)", ErrInvalidCode)
	}
	if len([]rune(c)) != 3 {
		return "", fmt.Errorf("%w: %q must be three letters (ISO 4217), got %d characters",
			ErrInvalidCode, code, len([]rune(c)))
	}
	for _, r := range c {
		if r < 'A' || r > 'Z' {
			return "", fmt.Errorf("%w: %q must be three LATIN letters (ISO 4217); %q is not one",
				ErrInvalidCode, code, string(r))
		}
	}
	return c, nil
}

// Validate перевіряє вхід: форму кодів і діапазон історії.
//
// Повертає нормалізований вхід, а не править оригінал на місці: функція, яка
// і валідує, і мутує аргумент, рано чи пізно застосується двічі.
func (in RateInput) Validate() (RateInput, error) {
	base, err := NormalizeCode(in.Base)
	if err != nil {
		return RateInput{}, fmt.Errorf("field \"base\": %w", err)
	}
	target, err := NormalizeCode(in.Target)
	if err != nil {
		return RateInput{}, fmt.Errorf("field \"target\": %w", err)
	}
	if in.HistoryDays < 0 || in.HistoryDays > maxHistoryDays {
		return RateInput{}, fmt.Errorf("field \"history_days\": %w: %d is outside 0..%d",
			ErrInvalidRange, in.HistoryDays, maxHistoryDays)
	}
	return RateInput{Base: base, Target: target, HistoryDays: in.HistoryDays}, nil
}

// suggestCodes шукає схожі коди серед відомих — щоб помилка підказувала.
//
// Модель, яка отримала «unknown currency code: USDD», може здогадатися сама.
// Модель, яка отримала «unknown currency code: USDD; did you mean USD?»,
// виправиться з першого разу. Різниця — один зайвий виклик на кожну одруківку,
// і на масштабі це гроші.
func suggestCodes(code string, known []string) []string {
	var out []string
	for _, k := range known {
		if editDistance(code, k) <= 1 {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	if len(out) > 3 {
		out = out[:3]
	}
	return out
}

// editDistance — відстань Левенштейна, обмежена трилітерними кодами.
func editDistance(a, b string) int {
	ar, br := []rune(a), []rune(b)
	prev := make([]int, len(br)+1)
	cur := make([]int, len(br)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ar); i++ {
		cur[0] = i
		for j := 1; j <= len(br); j++ {
			cost := 1
			if ar[i-1] == br[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, min(cur[j-1]+1, prev[j-1]+cost))
		}
		prev, cur = cur, prev
	}
	return prev[len(br)]
}

// nowRFC3339 — час у форматі контракту. Окремою функцією заради тестів.
var nowRFC3339 = func() string { return time.Now().UTC().Format(time.RFC3339) }
