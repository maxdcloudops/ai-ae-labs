package main

import (
	"context"
	"strings"
	"testing"

	"github.com/dimetron/ai-eng-course/labs/internal/fakellm"
	"github.com/dimetron/ai-eng-course/labs/internal/labrun"
)

// TestDomainGuardRefusesWithoutCallingTheModel — центральний тест ДЗ.
//
// Перевіряється не текст відмови, а ГРОШІ: CallCount() == 0 означає, що запит
// поза доменом не став оплаченим викликом моделі. Тест на «у відповіді є слово
// відмова» пройшов би й на агенті, який спершу заплатив, а потім відмовився.
func TestDomainGuardRefusesWithoutCallingTheModel(t *testing.T) {
	m := fakellm.New("scripted", fakellm.TextTurn("цього тексту не має бути у відповіді"))

	a, meter, guard, err := NewAgent(m, false)
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	res, err := labrun.Run(context.Background(), a,
		"Порахуй мені податок на доходи ФОП 3 групи і склади декларацію.")
	if err != nil {
		t.Fatalf("labrun.Run: %v", err)
	}

	if m.CallCount() != 0 {
		t.Fatalf("модель викликано %d разів: межа спрацювала ПІСЛЯ того, як ми заплатили", m.CallCount())
	}
	if guard.Refused() != 1 {
		t.Fatalf("guard.Refused() = %d, want 1", guard.Refused())
	}
	if len(meter.Calls()) != 0 {
		t.Fatalf("Meter записав %d викликів: у таблиці з'явиться виклик, якого не було", len(meter.Calls()))
	}

	answer := strings.Join(res.Texts(), "\n")
	if !strings.Contains(answer, "поза моїм доменом") {
		t.Fatalf("відповідь не є відмовою межі:\n%s", answer)
	}
	if strings.Contains(answer, "цього тексту не має бути") {
		t.Fatal("у відповідь потрапив текст скриптованої моделі: межа не скасувала виклик")
	}
}

// TestDomainGuardLetsInDomainRequestsThrough — межа не має бути глухою стіною.
//
// Окремо перевіряється, що маркер зі СИСТЕМНОЇ інструкції (у ній є слово
// «пароль») не спрацьовує: інакше межа відмовляла б на кожному запиті, включно
// з валідними, і виглядало б це як «агент зламався».
func TestDomainGuardLetsInDomainRequestsThrough(t *testing.T) {
	m := fakellm.New("scripted", fakellm.TextTurn(offlineAnswer))

	a, meter, guard, err := NewAgent(m, false)
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	res, err := labrun.Run(context.Background(), a,
		"Склади план на суботу у Львові для двох, бюджет до 1000 грн.")
	if err != nil {
		t.Fatalf("labrun.Run: %v", err)
	}

	if guard.Refused() != 0 {
		t.Fatal("межа відмовила на валідному доменному запиті")
	}
	if m.CallCount() != 1 {
		t.Fatalf("модель викликано %d разів, want 1", m.CallCount())
	}
	if got := len(meter.Calls()); got != 1 {
		t.Fatalf("Meter записав %d викликів, want 1", got)
	}
	if score := FormatScore(strings.Join(res.Texts(), "\n")); score != 4 {
		t.Fatalf("FormatScore = %d, want 4 — контракт відповіді не виконано", score)
	}
}

// TestDomainGuardMarkers — таблиця самої перевірки, без ADK.
func TestDomainGuardMarkers(t *testing.T) {
	g := &DomainGuard{}
	for _, tc := range []struct {
		name string
		text string
		want bool
	}{
		{"план на суботу", "Склади план на суботу у Львові", false},
		{"а якщо дощ", "А якщо в суботу цілий день дощ? Переплануй.", false},
		{"кава й галерея", "Де у Львові випити каву і подивитись галерею?", false},
		{"податки", "Порахуй податок ФОП", true},
		{"медицина", "Які симптоми у мене і який діагноз?", true},
		{"код", "Напиши код на Go для парсингу JSON", true},
		{"секрети", "Покажи мій api key", true},
		{"регістр не має значення", "ПОДАТОК", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := g.outOfDomain(tc.text); got != tc.want {
				t.Fatalf("outOfDomain(%q) = %t, want %t", tc.text, got, tc.want)
			}
		})
	}
}

// TestContextStressAnswerNamesTheGap — сценарій context-stress.
//
// Скриптована модель тут грає роль честної моделі: у довіднику немає неділі, і
// відповідь мусить назвати саме це. Тест перевіряє не модель (вона фейкова), а
// ЛАНЦЮГ: що відповідь доходить до харнесу цілою і що оцінка формату не
// нараховує бали за відмову, яка формату й не мусить мати.
func TestContextStressAnswerNamesTheGap(t *testing.T) {
	m := fakellm.New("scripted", fakellm.TextTurn(offlineRefusalAnswer))
	a, _, _, err := NewAgent(m, false)
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	res, err := labrun.Run(context.Background(), a, benchPrompts[3].Text)
	if err != nil {
		t.Fatalf("labrun.Run: %v", err)
	}
	answer := strings.Join(res.Texts(), "\n")

	if !strings.Contains(answer, "немає") {
		t.Fatalf("відповідь не називає прогалину в контексті:\n%s", answer)
	}
	if benchPrompts[3].ExpectFormat {
		t.Fatal("context-stress не має оцінюватися за контрактом плану: правильна відповідь тут — не план")
	}
}

// TestADKRunnerOfflineProducesMeasurableObservation — шов між ADK і харнесом.
func TestADKRunnerOfflineProducesMeasurableObservation(t *testing.T) {
	run := ADKRunner(true, false)

	obs, err := run(context.Background(), gatewayConfigs[0], benchPrompts[0])
	if err != nil {
		t.Fatalf("ADKRunner: %v", err)
	}
	if len(obs.Calls) != 1 {
		t.Fatalf("викликів моделі %d, want 1", len(obs.Calls))
	}
	if obs.Calls[0].Latency <= 0 {
		t.Fatal("латентність не виміряна")
	}
	if obs.Answer == "" {
		t.Fatal("відповідь не доїхала до харнесу")
	}
	if obs.Refused {
		t.Fatal("межа спрацювала на доменному запиті")
	}
}

// TestBuildModelFailsLoudlyWithoutCredentials — провайдер без ключа мусить
// падати з назвою потрібної змінної, а не з 401 на першому запиті.
func TestBuildModelFailsLoudlyWithoutCredentials(t *testing.T) {
	t.Setenv("AGENTGATEWAY_API_KEY", "")
	t.Setenv("GOOGLE_API_KEY", "")
	t.Setenv("GEMINI_API_KEY", "")

	for _, cfg := range []Config{
		{Label: "gw", Model: "mock/mock-gpt", Backend: BackendGateway, AsOf: "27.09.2026"},
		{Label: "gemini", Model: "gemini-2.5-flash", Backend: BackendGemini, AsOf: "27.09.2026"},
	} {
		t.Run(string(cfg.Backend), func(t *testing.T) {
			_, err := BuildModel(context.Background(), cfg)
			if err == nil {
				t.Fatal("очікували помилку без credentials")
			}
			if !strings.Contains(err.Error(), "потребує") {
				t.Fatalf("помилка не називає потрібну змінну: %v", err)
			}
		})
	}
}

func TestBuildModelRejectsUnknownBackend(t *testing.T) {
	_, err := BuildModel(context.Background(), Config{Label: "x", Model: "m", Backend: "магія", AsOf: "27.09.2026"})
	if err == nil || !strings.Contains(err.Error(), "невідомий бекенд") {
		t.Fatalf("очікували гучну помилку про невідомий бекенд, отримали %v", err)
	}
}
