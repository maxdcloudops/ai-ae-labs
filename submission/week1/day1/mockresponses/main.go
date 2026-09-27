// mockresponses — крихітний бекенд, сумісний з OpenAI **Responses API**.
//
// Навіщо він узагалі існує. ADK Go v2.4.0 у пакеті openaimodel говорить
// ВИКЛЮЧНО Responses API (`POST /v1/responses`, див. client.Responses.New).
// А mock-llm із demo/1_ai-gateway реалізує лише старий `/v1/chat/completions`.
// Тому ланцюг «ADK-агент → agentgateway → mock-llm» рветься на 400, і це не
// баг гейтвея: гейтвей чесно проксює шлях, якого в бекенді немає.
//
// Це і є урок цього файлу, і він коштує більше, ніж сам файл: «OpenAI-сумісний»
// — це не одна сумісність, а мінімум дві несумісні між собою (chat completions
// і responses). Перед тим як вибирати провайдера за фразою «OpenAI-compatible»,
// перевірте, ЯКИЙ саме API він реалізує — інакше ви дізнаєтесь це з 400-ки.
//
// Що НЕ можна читати з прогону через цей бекенд: якість відповіді (текст
// написаний тут, а не моделлю). Що можна: латентність ланцюга, кількість
// токенів, арифметику вартості, поведінку ретраїв і те, що доменна межа
// спрацьовує до виклику.
//
// Запуск:
//
//	go run ./mockresponses            # :8098
//	PORT=9000 go run ./mockresponses
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

// planAnswer — відповідь, яка ВИКОНУЄ контракт інструкції агента.
//
// Виконує навмисно: так колонка «формат 0–4» у бенчмарку показує, що оцінювач
// працює. Пара до неї — sloppyAnswer нижче.
const planAnswer = `Ранок: Високий замок — панорама міста, 1.5 години, вхід вільний.
День: обід у Старому місті + Аптека-музей, ~2 години, 350–500 грн.
Вечір: Львівська опера або концерт у філармонії, за наявності квитків.
Бюджет: 700–1100 грн на двох.

Застереження: прогноз погоди й афішу цей бекенд не перевіряв — він фейковий.`

// sloppyAnswer — відповідь, яка контракт ІГНОРУЄ.
//
// Потрібна, щоб колонка «формат» у таблиці мала розкид: стовпчик, у якому всі
// значення однакові, не вимірює нічого. Модель `mock-sloppy` існує рівно для
// цього рядка таблиці.
const sloppyAnswer = `Sure! Lviv is a lovely city, you should definitely walk around the old town and try some coffee.`

// responseBody — мінімальний валідний об'єкт Responses API.
//
// Поля названі точно так, як їх чекає openai-go/v3: `output` — масив item-ів,
// у message-item — масив `content` з `output_text`. Скоротити не вийде:
// клієнт, який не знайшов жодного output_text, віддає агенту порожню
// відповідь, і це виглядає як «модель промовчала».
type responseBody struct {
	ID        string       `json:"id"`
	Object    string       `json:"object"`
	CreatedAt int64        `json:"created_at"`
	Model     string       `json:"model"`
	Status    string       `json:"status"`
	Output    []outputItem `json:"output"`
	Usage     usage        `json:"usage"`
	// Поля нижче openai-go читає, і без них декодер лишає нулі в місцях, де
	// потім важко зрозуміти, чому агент «не побачив» відповіді.
	ParallelToolCalls bool  `json:"parallel_tool_calls"`
	Tools             []any `json:"tools"`
}

type outputItem struct {
	Type    string        `json:"type"`
	ID      string        `json:"id"`
	Status  string        `json:"status"`
	Role    string        `json:"role"`
	Content []contentPart `json:"content"`
}

type contentPart struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	Annotations []any  `json:"annotations"`
}

type usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// requestBody — те, що нам потрібно з запиту: ім'я моделі й розмір входу.
//
// `input` у Responses API поліморфний (рядок АБО масив item-ів), тому читаємо
// його як json.RawMessage і рахуємо слова по сирому тексту. Для мока цього
// достатньо: нам потрібен правдоподібний розмір входу, а не справжній BPE.
type requestBody struct {
	Model        string          `json:"model"`
	Input        json.RawMessage `json:"input"`
	Instructions string          `json:"instructions"`
}

func handleResponses(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req requestBody
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}

	// mock-fail завжди 500 — це справжня 5xx для тесту ретраїв з backoff.
	// Fault injection має бути справжньою: помилка, підроблена в клієнті, не
	// перевіряє ні транспорт, ні те, що гейтвей запише її в access-log.
	if strings.Contains(req.Model, "fail") {
		writeError(w, http.StatusInternalServerError, "simulated upstream failure for model "+req.Model)
		return
	}

	answer := planAnswer
	if strings.Contains(req.Model, "sloppy") {
		answer = sloppyAnswer
	}

	inTokens := countWords(string(req.Input)) + countWords(req.Instructions)
	if inTokens == 0 {
		inTokens = 1 // нуль вхідних токенів означав би нульову вартість входу
	}
	outTokens := countWords(answer)

	body := responseBody{
		ID:        fmt.Sprintf("resp_mock_%d", time.Now().UnixNano()),
		Object:    "response",
		CreatedAt: time.Now().Unix(),
		Model:     req.Model,
		Status:    "completed",
		Output: []outputItem{{
			Type:    "message",
			ID:      fmt.Sprintf("msg_mock_%d", time.Now().UnixNano()),
			Status:  "completed",
			Role:    "assistant",
			Content: []contentPart{{Type: "output_text", Text: answer, Annotations: []any{}}},
		}},
		Usage: usage{InputTokens: inTokens, OutputTokens: outTokens, TotalTokens: inTokens + outTokens},
		Tools: []any{},
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(body); err != nil {
		log.Printf("encode response: %v", err)
	}
}

// countWords — груба оцінка кількості токенів.
//
// Це НЕ токенайзер, і видавати його за токенайзер не можна. Але для мока
// важлива одна властивість, яка тут є: довший вхід дає більше токенів, а
// значить і більшу вартість. Саме її перевіряє бенчмарк.
func countWords(s string) int { return len(strings.Fields(s)) }

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"message": msg, "type": "invalid_request_error"},
	})
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8098"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/responses", handleResponses)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	// Явна 501 замість неявної 404: той, хто прийде сюди зі старим клієнтом,
	// має прочитати ПРИЧИНУ, а не гадати над «page not found».
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotImplemented,
			"this mock implements the OpenAI Responses API (/v1/responses) only — ADK Go v2 openaimodel uses it")
	})

	log.Printf("mockresponses (OpenAI Responses API) listening on :%s", port)
	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Fatal(srv.ListenAndServe())
}
