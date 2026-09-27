package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/dimetron/ai-eng-course/labs/internal/fakellm"
	"github.com/dimetron/ai-eng-course/labs/internal/labrun"
)

// TestRepairOnlyDoesWhatCannotChangeMeaning — головна межа ДЗ 2.
//
// Таблиця читається як політика: ліворуч те, що інструмент виправляє МОВЧКИ
// безпечно, праворуч — те, на чому він мусить зупинитися й спитати. Рядок
// «долар» тут найважливіший: доларів багато (USD, CAD, AUD, SGD), тож
// «виправити» його на USD означало б вибрати валюту за користувача.
func TestRepairOnlyDoesWhatCannotChangeMeaning(t *testing.T) {
	for _, tc := range []struct {
		name       string
		in         RateInput
		wantOK     bool
		wantBase   string
		wantAction string
	}{
		{"канонічний вхід проходить без ремонту", RateInput{Base: "USD", Target: "UAH"}, true, "USD", "validated"},
		{"регістр — нормалізація, не ремонт", RateInput{Base: "usd", Target: "uah"}, true, "USD", "validated"},
		{"пробіли", RateInput{Base: "  eur ", Target: "UAH"}, true, "EUR", "validated"},
		{"однозначний синонім — безпечний ремонт", RateInput{Base: "євро", Target: "UAH"}, true, "EUR", "repaired"},
		{"діапазон підрізається", RateInput{Base: "USD", Target: "UAH", HistoryDays: 999}, true, "USD", "repaired"},

		{"«долар» неоднозначний — НЕ ремонтуємо", RateInput{Base: "долар", Target: "UAH"}, false, "", ""},
		{"одруківка — НЕ ремонтуємо", RateInput{Base: "US1", Target: "UAH"}, false, "", ""},
		{"задовгий код — НЕ ремонтуємо", RateInput{Base: "USDD", Target: "UAH"}, false, "", ""},
		{"порожнє поле — НЕ ремонтуємо", RateInput{Base: "", Target: "UAH"}, false, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := TryRepair(tc.in, DefaultRepairPolicy())

			if !tc.wantOK {
				if res.Err == nil {
					t.Fatalf("небезпечний ремонт відбувся: отримали %+v", res.Input)
				}
				return
			}
			if res.Err != nil {
				t.Fatalf("безпечний випадок відхилено: %v", res.Err)
			}
			if res.Input.Base != tc.wantBase {
				t.Fatalf("base = %q, want %q", res.Input.Base, tc.wantBase)
			}
			last := res.Steps[len(res.Steps)-1]
			if tc.wantAction == "validated" && last.Action != "validated" {
				t.Fatalf("останній крок = %q, очікували validated (ремонт не мав знадобитись)", last.Action)
			}
			if tc.wantAction == "repaired" && !res.Repaired {
				t.Fatalf("очікували ремонт, а його не було: %+v", res.Steps)
			}
		})
	}
}

// TestRepairRespectsAttemptLimit — ліміт спроб конфігурований і дотримується.
func TestRepairRespectsAttemptLimit(t *testing.T) {
	in := RateInput{Base: "євро", Target: "UAH"} // потребує рівно одного ремонту

	if res := TryRepair(in, RepairPolicy{MaxAttempts: 1, AllowNormalization: true}); res.Err == nil {
		t.Fatal("MaxAttempts=1 мусить означати «жодного ремонту», а ремонт відбувся")
	}
	if res := TryRepair(in, RepairPolicy{MaxAttempts: 2, AllowNormalization: true}); res.Err != nil {
		t.Fatalf("MaxAttempts=2 мусить встигнути один ремонт: %v", res.Err)
	}
}

// TestRepairCanBeDisabledEntirely — вимикач нормалізації.
func TestRepairCanBeDisabledEntirely(t *testing.T) {
	res := TryRepair(RateInput{Base: "євро", Target: "UAH"},
		RepairPolicy{MaxAttempts: 5, AllowNormalization: false})
	if res.Err == nil {
		t.Fatal("ремонт вимкнено, а він усе одно відбувся")
	}
	if !strings.Contains(res.Steps[0].Action, "normalization disabled") {
		t.Fatalf("причина відмови не записана в слід: %+v", res.Steps)
	}
}

// TestRepairLeavesAnAuditTrail — слід ремонту обов'язковий.
//
// Без нього неможливо відповісти на питання «інструмент виправив аргумент чи
// користувач із самого початку просив інше?». У домені грошей це питання
// ставлять, і «не знаю» — незадовільна відповідь.
func TestRepairLeavesAnAuditTrail(t *testing.T) {
	res := TryRepair(RateInput{Base: "євро", Target: "UAH", HistoryDays: 100}, DefaultRepairPolicy())
	if res.Err != nil {
		t.Fatalf("TryRepair: %v", res.Err)
	}
	if len(res.Steps) < 2 {
		t.Fatalf("очікували щонайменше два кроки (ремонт + повторна валідація), маємо %+v", res.Steps)
	}
	trail := summarize(res.Steps)
	for _, want := range []string{"євро", "EUR", "100", "14"} {
		if !strings.Contains(trail, want) {
			t.Fatalf("слід ремонту не згадує %q: %q", want, trail)
		}
	}
}

// TestDecodeArgsSeparatesLevelsOfFailure — три різні рівні помилки.
//
// Плутати їх дорого: помилка рівня домену, подана як «bad request», відправляє
// модель виправляти тип замість значення, і наступна спроба буде такою самою.
func TestDecodeArgsSeparatesLevelsOfFailure(t *testing.T) {
	for _, tc := range []struct {
		name, raw, wantIn string
	}{
		{"обірваний JSON", `{"base": "USD", "target":`, "unexpected EOF"},
		{"не той тип", `{"base": 840, "target": "UAH"}`, "cannot unmarshal number"},
		{"невідоме поле", `{"base":"USD","target":"UAH","currency":"EUR"}`, `unknown field "currency"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DecodeArgs([]byte(tc.raw))
			if err == nil {
				t.Fatal("зіпсовані аргументи пройшли декодування")
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Fatalf("помилка %q не містить %q", err, tc.wantIn)
			}
		})
	}
}

// TestDecodeArgsAcceptsOptionalField — history_days опційне.
func TestDecodeArgsAcceptsOptionalField(t *testing.T) {
	in, err := DecodeArgs([]byte(`{"base":"USD","target":"UAH"}`))
	if err != nil {
		t.Fatalf("виклик без опційного поля відхилено: %v", err)
	}
	if in.HistoryDays != 0 {
		t.Fatalf("HistoryDays = %d, want 0", in.HistoryDays)
	}
}

// --- agent.go: інструмент усередині агента ---

// TestToolRejectsBadCallAndModelSelfCorrects — діалог №2 з README, як тест.
//
// Перевіряється ЛАНЦЮГ, а не модель: перший tool-call відхилено з конкретною
// помилкою, другий пройшов, і число реальне. Репліки моделі скриптовані —
// саме тому тест детермінований і не потребує ключа.
func TestToolRejectsBadCallAndModelSelfCorrects(t *testing.T) {
	m := fakellm.New("scripted",
		fakellm.CallTurn(toolName, map[string]any{"base": "USDD", "target": "UAH"}),
		fakellm.CallTurn(toolName, map[string]any{"base": "USD", "target": "UAH"}),
		fakellm.TextTurn("Курс USD/UAH наведено вище."),
	)
	a, log, err := NewLoggedAgent(m, fixture())
	if err != nil {
		t.Fatalf("NewLoggedAgent: %v", err)
	}

	if _, err := labrun.Run(context.Background(), a, "Скільки коштує USDD?"); err != nil {
		t.Fatalf("labrun.Run: %v", err)
	}

	calls := log.Calls()
	if len(calls) != 2 {
		t.Fatalf("очікували 2 виклики інструмента (поганий + виправлений), маємо %d: %+v", len(calls), calls)
	}
	if calls[0].Err == "" {
		t.Fatal("перший виклик із неіснуючим кодом мав бути відхилений")
	}
	if !strings.Contains(calls[0].Err, "USDD") {
		t.Fatalf("помилка не називає значення, яке треба виправити: %q", calls[0].Err)
	}
	if calls[1].Err != "" {
		t.Fatalf("виправлений виклик теж відхилено: %q", calls[1].Err)
	}
	if calls[1].Quote == nil || calls[1].Quote.Rate <= 0 {
		t.Fatal("виправлений виклик не повернув курс")
	}
}

// TestToolNeverInventsANumberWhenUpstreamIsDown — fail path у зборі.
func TestToolNeverInventsANumberWhenUpstreamIsDown(t *testing.T) {
	m := fakellm.New("scripted",
		fakellm.CallTurn(toolName, map[string]any{"base": "USD", "target": "UAH"}),
		fakellm.TextTurn("Не вдалося отримати курс: джерело недоступне."),
	)
	a, log, err := NewLoggedAgent(m, &FixtureProvider{Err: errUpstreamFixture})
	if err != nil {
		t.Fatalf("NewLoggedAgent: %v", err)
	}

	res, err := labrun.Run(context.Background(), a, "Курс долара?")
	if err != nil {
		t.Fatalf("labrun.Run: %v", err)
	}

	calls := log.Calls()
	if len(calls) != 1 || calls[0].Err == "" {
		t.Fatalf("очікували один відхилений виклик, маємо %+v", calls)
	}
	if calls[0].Quote != nil {
		t.Fatal("мертвий апстрім повернув котирування")
	}
	answer := strings.Join(res.Texts(), " ")
	if !strings.Contains(answer, "не вдалося") && !strings.Contains(answer, "Не вдалося") {
		t.Fatalf("агент не сказав чесно про збій: %q", answer)
	}
}

// TestRepairIsVisibleInTheAnswer — тихий ремонт заборонений.
func TestRepairIsVisibleInTheAnswer(t *testing.T) {
	m := fakellm.New("scripted",
		fakellm.CallTurn(toolName, map[string]any{"base": "євро", "target": "UAH"}),
		fakellm.TextTurn("ok"),
	)
	a, log, err := NewLoggedAgent(m, fixture())
	if err != nil {
		t.Fatalf("NewLoggedAgent: %v", err)
	}
	if _, err := labrun.Run(context.Background(), a, "Курс євро?"); err != nil {
		t.Fatalf("labrun.Run: %v", err)
	}

	calls := log.Calls()
	if len(calls) != 1 || calls[0].Quote == nil {
		t.Fatalf("очікували один успішний виклик: %+v", calls)
	}
	if calls[0].Quote.Pair.Base != "EUR" {
		t.Fatalf("ремонт не застосувався: base = %q", calls[0].Quote.Pair.Base)
	}
	if !strings.Contains(calls[0].Quote.Note, "normalized") {
		t.Fatalf("ремонт відбувся тихо — у Note про нього нічого: %q", calls[0].Quote.Note)
	}
}

// TestBothToolVariantsBuild — інструмент будується в обох режимах схеми.
func TestBothToolVariantsBuild(t *testing.T) {
	if _, err := NewRateTool(fixture(), DefaultRepairPolicy()); err != nil {
		t.Fatalf("автогенерована схема: %v", err)
	}
	if _, err := NewStrictRateTool(fixture(), DefaultRepairPolicy()); err != nil {
		t.Fatalf("явна InputSchema: %v", err)
	}
}

// TestToolDescriptionTellsTheModelWhenToCall — опис вирішує, чи буде виклик.
//
// Тест на текст виглядає дивно рівно доти, доки агент не почне відповідати
// курсом із пам'яті. Модель не читає код — вона читає цей рядок.
func TestToolDescriptionTellsTheModelWhenToCall(t *testing.T) {
	for _, want := range []string{"Never state a rate from memory", "ask them to", "ISO 4217"} {
		if !strings.Contains(toolDescription, want) {
			t.Fatalf("опис інструмента не містить %q — модель не знатиме, коли викликати й коли перепитати", want)
		}
	}
}

// TestUnrepairableIsPermanent — текст для користувача залежить від цього.
func TestUnrepairableIsPermanent(t *testing.T) {
	res := TryRepair(RateInput{Base: "долар", Target: "UAH"}, DefaultRepairPolicy())
	if res.Err == nil {
		t.Fatal("неоднозначний синонім виправлено")
	}
	if !errors.Is(res.Err, ErrUnrepairable) {
		t.Fatalf("очікували ErrUnrepairable: %v", res.Err)
	}
	if !IsPermanent(res.Err) {
		t.Fatal("неремонтопридатні аргументи позначено як тимчасові: користувачу запропонують «спробувати пізніше»")
	}
}
