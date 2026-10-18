package main

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/dimetron/ai-eng-course/labs/week4/Day8_Fault_Tolerant_Tool_Harness/labs8/internal/ledger"
)

// TestClassifyError — ПУНКТ 1 умови: клас визначається через errors.Is/As,
// ніколи через текст помилки.
//
// Кейс «обгорнута семантична» тут найважливіший: помилка, загорнута в
// fmt.Errorf, лишається семантичною. Перевірка по рядку це зламала б —
// саме тому умова й забороняє err.Error().
func TestClassifyError(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
		want Class
	}{
		{"скасування згори", context.Canceled, ClassFatal},
		{"дедлайн — транзієнтний", context.DeadlineExceeded, ClassRetryTransient},
		{"семантична як тип", &ledger.SemanticError{Field: "merchant_id", Reason: "порожній"}, ClassSurfaceToModel},
		{"семантична як сентинел", ledger.ErrSemantic, ClassSurfaceToModel},
		{"транзієнтна", ledger.ErrTransient, ClassRetryTransient},
		{"обгорнута семантична", fmt.Errorf("open case: %w", &ledger.SemanticError{Field: "txn", Reason: "no"}), ClassSurfaceToModel},
		{"обгорнута транзієнтна", fmt.Errorf("step 2: %w", ledger.ErrTransient), ClassRetryTransient},
		{"невідома", errors.New("щось пішло не так"), ClassFatal},
		{"nil", nil, ClassFatal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got, _ := classifyError(tc.err); got != tc.want {
				t.Fatalf("classifyError(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}

// TestSemanticErrorBeatsTransient — порядок перевірок має значення.
//
// Помилка може задовольняти обидві перевірки одночасно. Якщо транзієнтна
// виграє, harness почне повторювати неправильний аргумент — і повторить його
// стільки разів, скільки дозволить рушій, жодного разу не наблизившись до
// успіху.
func TestSemanticErrorBeatsTransient(t *testing.T) {
	t.Parallel()
	both := fmt.Errorf("%w: %w", ledger.ErrTransient, &ledger.SemanticError{Field: "amount", Reason: "відʼємна"})
	if got, _ := classifyError(both); got != ClassSurfaceToModel {
		t.Fatalf("клас = %d, want ClassSurfaceToModel: неправильний аргумент не стає правильним від повтору", got)
	}
}

// TestObserveTurnsSemanticIntoObservation — ПУНКТ 2: семантична помилка НЕ
// повертається як error, вона стає спостереженням для моделі.
func TestObserveTurnsSemanticIntoObservation(t *testing.T) {
	t.Parallel()
	var b budget
	obs, err := b.observe(&ledger.SemanticError{Field: "merchant_id", Reason: "порожній"})
	if err != nil {
		t.Fatalf("семантична помилка прокинулась як error: %v", err)
	}
	if obs.Status != "invalid_argument" {
		t.Errorf("Status = %q, want invalid_argument", obs.Status)
	}
	if obs.Field != "merchant_id" {
		t.Errorf("Field = %q — модель не знатиме, що саме виправляти", obs.Field)
	}
	if obs.Attempt != 1 {
		t.Errorf("Attempt = %d, want 1", obs.Attempt)
	}
}

// TestSelfCorrectionBudgetIsExhaustedLoudly — вичерпання бюджету має бути
// явною помилкою, а не тихою зупинкою.
func TestSelfCorrectionBudgetIsExhaustedLoudly(t *testing.T) {
	t.Parallel()
	var b budget
	semantic := &ledger.SemanticError{Field: "txn", Reason: "формат"}

	for i := 1; i <= maxSelfCorrections; i++ {
		obs, err := b.observe(semantic)
		if err != nil {
			t.Fatalf("спроба %d мала дати Observation, а дала помилку: %v", i, err)
		}
		if obs.Attempt != i {
			t.Errorf("спроба %d: Attempt = %d", i, obs.Attempt)
		}
	}

	_, err := b.observe(semantic)
	if err == nil {
		t.Fatal("після вичерпання бюджету harness мовчки продовжив")
	}
	if !errors.Is(err, ErrSelfCorrectionExhausted) {
		t.Fatalf("errors.Is(err, ErrSelfCorrectionExhausted) = false; err = %v", err)
	}
}

// TestObserveSurfacesTransientAsUnavailable — після вичерпаних повторів
// модель має побачити «джерело недоступне», а не вигадати відповідь.
func TestObserveSurfacesTransientAsUnavailable(t *testing.T) {
	t.Parallel()
	var b budget
	obs, err := b.observe(fmt.Errorf("ledger: %w", ledger.ErrTransient))
	if err != nil {
		t.Fatalf("транзієнтна після повторів мала стати Observation: %v", err)
	}
	if obs.Status != "unavailable" {
		t.Errorf("Status = %q, want unavailable", obs.Status)
	}
	if b.used != 0 {
		t.Errorf("транзієнтна витратила бюджет самокорекції (used=%d) — це не помилка моделі", b.used)
	}
}

// TestObserveStopsOnCancel — скасування не перетворюється на спостереження.
func TestObserveStopsOnCancel(t *testing.T) {
	t.Parallel()
	var b budget
	if _, err := b.observe(context.Canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("скасування мало зупинити ран, а не стати Observation: %v", err)
	}
}
