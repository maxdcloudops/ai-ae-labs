// Цикл «валідація → ремонт → повторна валідація» з лімітом спроб.
//
// ++ Advanced до ДЗ 2. Тут же проходить головна межа дня, і вона важливіша за
// сам цикл:
//
//	РЕМОНТУВАТИ можна лише те, що не може змінити зміст.
//	ВСЕ ІНШЕ — це відмова з поясненням, а не здогад.
//
// "usd" → "USD" — ремонт: це та сама валюта, записана інакше.
// "US1" → "USD" — НЕ ремонт: це здогад. Може, мали на увазі USD, а може, це
// взагалі інший тікер. Інструмент, який «здогадався» про валюту в запиті про
// гроші, помиляється рідко й дорого — і саме такі помилки не ловляться
// тестами, бо система при цьому не падає.
//
// Схема доводить форму; дозвіл на ризиковану дію перевіряється окремо.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// RepairPolicy — налаштування циклу ремонту.
type RepairPolicy struct {
	// MaxAttempts — скільки разів пробувати провалідувати аргументи, рахуючи
	// першу спробу. 1 означає «жодного ремонту»: провалідували — і все.
	MaxAttempts int
	// AllowNormalization вмикає безпечні нормалізації (регістр, пробіли,
	// відомі синоніми). Вимикається, щоб показати різницю в тестах і в демо.
	AllowNormalization bool
}

// DefaultRepairPolicy — дві спроби: валідація, безпечний ремонт, ще валідація.
//
// Чому саме дві. Одна — і найдешевша одруківка моделі коштує зайвого раунду з
// нею. Багато — і цикл починає «підбирати» аргументи, а підбір у домені грошей
// це не стійкість, а замаскована вигадка.
func DefaultRepairPolicy() RepairPolicy {
	return RepairPolicy{MaxAttempts: 2, AllowNormalization: true}
}

// RepairStep — що сталося на одній ітерації циклу. Для звіту й для графіка.
type RepairStep struct {
	Attempt int    `json:"attempt"`
	Action  string `json:"action"`
	Err     string `json:"error,omitempty"`
}

// RepairResult — результат циклу.
type RepairResult struct {
	Input    RateInput    `json:"input"`
	Steps    []RepairStep `json:"steps"`
	Repaired bool         `json:"repaired"`
	Err      error        `json:"-"`
	ErrText  string       `json:"error,omitempty"`
}

// currencyAliases — синоніми, які можна розкрити БЕЗ здогадів.
//
// Кожен рядок тут — це не «схоже написання», а однозначна назва тієї самої
// валюти українською чи англійською. Межа проста й перевіряється очима: якщо
// для розкриття потрібно вгадувати — рядка тут бути не повинно.
//
// "долар" навмисно немає: доларів багато (USD, CAD, AUD, SGD), і вибір одного
// з них — це здогад, а не нормалізація.
var currencyAliases = map[string]string{
	"гривня": "UAH", "гривні": "UAH", "грн": "UAH", "hryvnia": "UAH",
	"євро": "EUR", "euro": "EUR", "eur": "EUR",
	"долар сша": "USD", "доллар сша": "USD", "us dollar": "USD", "usd": "USD",
	"фунт стерлінгів": "GBP", "british pound": "GBP",
	"злотий": "PLN", "zloty": "PLN",
	"швейцарський франк": "CHF", "swiss franc": "CHF",
}

// TryRepair — цикл «валідація → ремонт → повторна валідація».
//
// Повертає нормалізований вхід або помилку, яку модель побачить як
// спостереження. Кожна ітерація записується в Steps: без цього сліду
// неможливо відповісти на питання «інструмент виправив аргумент чи користувач
// із самого початку попросив інше?», а в домені грошей це питання ставлять.
func TryRepair(raw RateInput, p RepairPolicy) RepairResult {
	attempts := max(p.MaxAttempts, 1)
	res := RepairResult{}
	current := raw

	for attempt := 1; attempt <= attempts; attempt++ {
		valid, err := current.Validate()
		if err == nil {
			res.Input = valid
			res.Steps = append(res.Steps, RepairStep{Attempt: attempt, Action: "validated"})
			return res
		}

		// Остання спроба — далі ремонтувати нікуди.
		if attempt == attempts {
			res.Steps = append(res.Steps, RepairStep{
				Attempt: attempt, Action: "rejected (attempts exhausted)", Err: err.Error(),
			})
			res.Err = err
			res.ErrText = err.Error()
			return res
		}

		if !p.AllowNormalization {
			res.Steps = append(res.Steps, RepairStep{
				Attempt: attempt, Action: "rejected (normalization disabled)", Err: err.Error(),
			})
			res.Err = err
			res.ErrText = err.Error()
			return res
		}

		repaired, action, ok := safeRepair(current)
		if !ok {
			// Ось та сама межа: помилку можна ПОЯСНИТИ, але не можна
			// виправити, не вигадавши чогось за користувача.
			wrapped := fmt.Errorf("%w: %v", ErrUnrepairable, err)
			res.Steps = append(res.Steps, RepairStep{
				Attempt: attempt, Action: "rejected (no safe repair)", Err: err.Error(),
			})
			res.Err = wrapped
			res.ErrText = wrapped.Error()
			return res
		}

		res.Steps = append(res.Steps, RepairStep{Attempt: attempt, Action: action, Err: err.Error()})
		current = repaired
		res.Repaired = true
	}

	return res
}

// safeRepair застосовує лише ті зміни, які не можуть змінити зміст.
//
// Повертає (виправлений вхід, опис дії, чи вдалося). false означає «безпечного
// ремонту немає» — і це нормальний, очікуваний результат, а не збій.
func safeRepair(in RateInput) (RateInput, string, bool) {
	out := in
	var actions []string

	if fixed, act, ok := repairCode(in.Base); ok {
		out.Base = fixed
		actions = append(actions, "base: "+act)
	}
	if fixed, act, ok := repairCode(in.Target); ok {
		out.Target = fixed
		actions = append(actions, "target: "+act)
	}

	// Діапазон історії підрізаємо до стелі, а не відхиляємо: 30 днів замість
	// 14 — це не помилка сенсу, це надмірна вимога, і найкорисніша відповідь
	// тут — віддати максимум, який дозволено, і сказати про це.
	if in.HistoryDays > maxHistoryDays {
		out.HistoryDays = maxHistoryDays
		actions = append(actions, fmt.Sprintf("history_days: clamped %d → %d", in.HistoryDays, maxHistoryDays))
	}
	if in.HistoryDays < 0 {
		out.HistoryDays = 0
		actions = append(actions, fmt.Sprintf("history_days: clamped %d → 0", in.HistoryDays))
	}

	if len(actions) == 0 {
		return in, "", false
	}
	return out, "repaired (" + strings.Join(actions, "; ") + ")", true
}

// repairCode виправляє один код, якщо це можна зробити безпечно.
func repairCode(code string) (string, string, bool) {
	trimmed := strings.TrimSpace(code)
	if trimmed == "" {
		return "", "", false // порожнє поле не має безпечного заповнення
	}

	// 1. Регістр і пробіли — завжди безпечно.
	upper := strings.ToUpper(trimmed)
	if _, err := NormalizeCode(upper); err == nil {
		if upper != code {
			return upper, fmt.Sprintf("normalized %q → %q", code, upper),
				true
		}
		return code, "", false
	}

	// 2. Однозначний синонім — безпечно: це назва тієї самої валюти.
	if alias, ok := currencyAliases[strings.ToLower(trimmed)]; ok {
		return alias, fmt.Sprintf("resolved alias %q → %q", code, alias), true
	}

	// 3. Усе інше — здогад. Не ремонтуємо.
	return "", "", false
}

// DecodeArgs розбирає сирі аргументи tool-call так, як їх надсилає модель.
//
// Окрема функція, бо на цьому кроці ламається інше: сюди приходить JSON, і
// помилка типу («history_days»: «сім») виглядає зовсім не так, як помилка
// домену. Модель має побачити різницю: перше виправляється зміною типу, друге
// — зміною значення.
//
// DisallowUnknownFields увімкнено навмисно: поле, якого немає в контракті, —
// це або одруківка моделі, або схема, яку вона взяла з іншого інструмента.
// Мовчки його проігнорувати означає виконати НЕ той виклик, про який просили.
func DecodeArgs(raw []byte) (RateInput, error) {
	var in RateInput
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return RateInput{}, fmt.Errorf("tool arguments are not valid for this contract: %w", err)
	}
	return in, nil
}

// IsPermanent повідомляє, чи помилка така, що повторний виклик із тими самими
// аргументами нічого не змінить.
//
// Потрібно для fail path: на ErrUpstream має сенс сказати «джерело недоступне,
// спробуйте пізніше», а на ErrUnknownCurrency — «такої валюти немає», і це
// «пізніше» не полагодить.
func IsPermanent(err error) bool {
	return errors.Is(err, ErrInvalidCode) ||
		errors.Is(err, ErrUnknownCurrency) ||
		errors.Is(err, ErrInvalidRange) ||
		errors.Is(err, ErrUnrepairable)
}
