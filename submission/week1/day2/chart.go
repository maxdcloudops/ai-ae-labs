// Графік результатів fault injection — SVG без залежностей.
//
// Питання, на яке відповідає цей графік: «що саме робить межа системи з
// невалідним викликом — і на якому рівні вона його ловить». Таблиця відповідає
// на нього теж, але графік показує пропорцію: скільки сценаріїв відхилено, а
// скільки безпечно відремонтовано. Саме ця пропорція і є налаштуванням, яке
// інженер обирає свідомо.
package main

import (
	"fmt"
	"html"
	"strings"
)

const (
	fBG       = "#f7f7f5"
	fCard     = "#ffffff"
	fInk      = "#1f1f1d"
	fMuted    = "#6b6b66"
	fGrid     = "#e3e3df"
	fRejected = "#b4483c" // виклик не пройшов межу
	fRepaired = "#c4913a" // виправлено безпечно
	fAccepted = "#4a7c59" // валідний із першої спроби
	fStage    = "#8a8a84" // нейтральна смуга зведення: рахує ВСІ результати
)

// stageOrder — рівні межі в порядку проходження.
//
// Порядок тут не косметичний: він і є архітектура. Виклик проходить рівні
// зліва направо, і кожен наступний дорожчий за попередній — decode безкоштовний,
// validate коштує процесорного часу, а до провайдера доходить лише те, що
// пройшло обидва.
var stageOrder = []struct {
	key   string
	label string
}{
	{"decode", "1. decode — чи це взагалі наш контракт"},
	{"validate", "2. validate — чи правильна ФОРМА аргументів"},
	{"repair", "3. repair — чи можна виправити БЕЗПЕЧНО"},
	{"lookup", "4. lookup — чи існує це в джерелі"},
}

// RenderFaultsSVG малює результати fault injection.
func RenderFaultsSVG(outcomes []faultOutcome, date string) string {
	if len(outcomes) == 0 {
		return `<svg xmlns="http://www.w3.org/2000/svg" width="460" height="56"><text x="12" y="32">Немає сценаріїв</text></svg>`
	}

	const (
		width   = 900
		padX    = 28
		rowH    = 26
		labelW  = 170
		legendH = 78
	)
	headerH := 96
	panelH := 42 + len(outcomes)*rowH + 14
	summaryH := 40 + len(stageOrder)*26 + 14
	height := headerH + panelH + 24 + summaryH + legendH

	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d" font-family="ui-sans-serif,-apple-system,Segoe UI,Roboto,Helvetica,Arial,sans-serif">`,
		width, height, width, height)
	fmt.Fprintf(&b, `<rect width="%d" height="%d" fill="%s"/>`, width, height, fBG)

	fmt.Fprintf(&b, `<text x="%d" y="34" font-size="20" font-weight="700" fill="%s">%s</text>`,
		padX, fInk, esc("Strict Schema Enforcer — що межа робить із невалідним викликом"))
	fmt.Fprintf(&b, `<text x="%d" y="56" font-size="12" fill="%s">%s</text>`,
		padX, fMuted, esc("Валідний JSON ≠ валідний виклик. Кожен сценарій — реальний прогін через ту саму межу, що й справжні виклики моделі."))
	fmt.Fprintf(&b, `<text x="%d" y="74" font-size="12" font-weight="600" fill="%s">%s</text>`,
		padX, fMuted, esc("станом на "+date))

	// Панель зі сценаріями.
	y := headerH
	fmt.Fprintf(&b, `<rect x="%d" y="%d" width="%d" height="%d" rx="8" fill="%s" stroke="%s"/>`,
		padX, y, width-padX*2, panelH, fCard, fGrid)
	fmt.Fprintf(&b, `<text x="%d" y="%d" font-size="13" font-weight="600" fill="%s">%s</text>`,
		padX+16, y+24, fInk, esc("Сценарії fault injection"))

	for i, o := range outcomes {
		ty := y + 42 + i*rowH
		color, verdict := verdictStyle(o.Verdict)

		fmt.Fprintf(&b, `<text x="%d" y="%d" font-size="11" font-family="ui-monospace,SFMono-Regular,Menlo,monospace" fill="%s">%s</text>`,
			padX+16, ty+13, fInk, esc(o.Scenario.Name))
		// Плашка вердикту фіксованої ширини: очі читають колонку, а не текст.
		fmt.Fprintf(&b, `<rect x="%d" y="%d" width="86" height="17" rx="3" fill="%s"/>`,
			padX+16+labelW, ty, color)
		fmt.Fprintf(&b, `<text x="%d" y="%d" font-size="10" font-weight="600" fill="#ffffff" text-anchor="middle">%s</text>`,
			padX+16+labelW+43, ty+13, esc(verdict))
		fmt.Fprintf(&b, `<text x="%d" y="%d" font-size="11" fill="%s">%s</text>`,
			padX+16+labelW+98, ty+13, fMuted, esc(trimRunes(o.Scenario.Why+" · "+o.Message, 84)))
	}

	// Зведення за рівнями межі.
	y += panelH + 24
	byStage := map[string]int{}
	for _, o := range outcomes {
		byStage[o.Stage]++
	}
	fmt.Fprintf(&b, `<rect x="%d" y="%d" width="%d" height="%d" rx="8" fill="%s" stroke="%s"/>`,
		padX, y, width-padX*2, summaryH, fCard, fGrid)
	fmt.Fprintf(&b, `<text x="%d" y="%d" font-size="13" font-weight="600" fill="%s">%s</text>`,
		padX+16, y+24, fInk, esc("На якому рівні спіймано — і скільки"))

	maxCount := 1
	for _, s := range stageOrder {
		if byStage[s.key] > maxCount {
			maxCount = byStage[s.key]
		}
	}
	plotW := width - padX*2 - 360
	for i, s := range stageOrder {
		ty := y + 40 + i*26
		n := byStage[s.key]
		fmt.Fprintf(&b, `<text x="%d" y="%d" font-size="11" fill="%s">%s</text>`,
			padX+16, ty+13, fInk, esc(s.label))
		w := int(float64(n) / float64(maxCount) * float64(plotW))
		// Нейтральний колір навмисно: ця смуга рахує ВСІ результати на рівні,
		// і зелений тут читався б як «усі прийняті» — рівно навпаки до того,
		// що показує рядок decode.
		fmt.Fprintf(&b, `<rect x="%d" y="%d" width="%d" height="16" rx="2" fill="%s"/>`,
			padX+16+320, ty, w, fStage)
		fmt.Fprintf(&b, `<text x="%d" y="%d" font-size="10" fill="%s">%d</text>`,
			padX+16+320+w+6, ty+13, fMuted, n)
	}

	// Легенда з поясненням, а не просто кольорами.
	y += summaryH + 16
	legend := []struct {
		color, text string
	}{
		{fRejected, "rejected — виклик не пройшов: модель отримує конкретну помилку й виправляє САМА"},
		{fRepaired, "repaired — виправлено безпечно (регістр, синонім, підрізаний діапазон); ремонт видно у відповіді"},
		{fAccepted, "accepted — валідний із першої спроби"},
	}
	for i, l := range legend {
		ty := y + i*18
		fmt.Fprintf(&b, `<rect x="%d" y="%d" width="10" height="10" rx="2" fill="%s"/>`, padX, ty, l.color)
		fmt.Fprintf(&b, `<text x="%d" y="%d" font-size="11" fill="%s">%s</text>`,
			padX+16, ty+9, fMuted, esc(l.text))
	}

	b.WriteString(`</svg>`)
	return b.String()
}

// verdictStyle віддає колір і підпис плашки.
func verdictStyle(v string) (string, string) {
	switch v {
	case "rejected":
		return fRejected, "REJECTED"
	case "repaired":
		return fRepaired, "REPAIRED"
	default:
		return fAccepted, "ACCEPTED"
	}
}

func trimRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// esc екранує текст: повідомлення помилок містять лапки й `&`, і без
// екранування SVG стає невалідним — GitHub тоді показує битий значок.
func esc(s string) string { return html.EscapeString(s) }
