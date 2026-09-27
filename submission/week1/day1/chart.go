// Графік результатів бенчмарку — SVG без залежностей.
//
// Чому власний рендер, а не бібліотека: файл має відкриватися прямо в README на
// GitHub, без запуску коду й без CI-кроку. SVG — єдиний формат, який GitHub
// показує в markdown і який при цьому лишається текстом у diff: рядок графіка
// видно в code review так само, як рядок таблиці.
//
// Палітра свідомо середньотонова: SVG у README рендериться і на світлій, і на
// темній темі GitHub, а медіа-запити в <img> надійно не працюють. Тому фон
// намальований явною плашкою, а не залишений прозорим.
package main

import (
	"fmt"
	"html"
	"strings"
	"time"
)

// Колірні токени графіка. Одне місце — щоб змінити тему одним рядком.
const (
	colBG      = "#f7f7f5"
	colCard    = "#ffffff"
	colInk     = "#1f1f1d"
	colMuted   = "#6b6b66"
	colGrid    = "#e3e3df"
	colHarness = "#3b6ea5" // вимір агента
	colGateway = "#c4703a" // вимір межі системи
	colLatency = "#4a7c59"
	colFormat  = "#8a6bab"
)

// ChartInput — усе, що графік має сказати, щоб його прочитали правильно.
type ChartInput struct {
	Title     string
	Mode      string
	AsOf      string
	Summaries []Summary
	// Note — застереження, без якого числа читаються неправильно.
	Note string
}

// RenderSVG малює три панелі: латентність, вартість (два виміри), формат.
//
// Панель вартості — головна: вона показує одну й ту саму величину, поміряну
// двічі. Саме тому стовпчики «харнес» і «гейтвей» стоять поруч, а не на різних
// графіках: розбіжність має бути видно, а не вимагати перемикання вкладок.
func RenderSVG(in ChartInput) string {
	rows := in.Summaries
	if len(rows) == 0 {
		return `<svg xmlns="http://www.w3.org/2000/svg" width="480" height="60"><text x="12" y="34">Немає даних для графіка</text></svg>`
	}

	const (
		width      = 900
		padX       = 28
		panelGap   = 26
		labelW     = 230
		barH       = 16
		barGap     = 6
		rowH       = barH + barGap + 8
		axisFooter = 30
		modeWidth  = 118 // символів у рядку підзаголовка
	)
	// Висоти рахуються, а не вписуються константами: таблиця на чотири рядки в
	// панелі, розрахованій на три, мовчки вилазить за плашку — і графік
	// виглядає зламаним рівно тоді, коли даних стало більше.
	modeLines := wrap(in.Mode, modeWidth)
	headerH := 60 + len(modeLines)*16 + 20
	panelH := 46 + len(rows)*rowH + 10
	height := headerH + 3*(panelH+panelGap) + axisFooter

	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d" font-family="ui-sans-serif,-apple-system,Segoe UI,Roboto,Helvetica,Arial,sans-serif">`,
		width, height, width, height)
	fmt.Fprintf(&b, `<rect width="%d" height="%d" fill="%s"/>`, width, height, colBG)

	// Заголовок.
	fmt.Fprintf(&b, `<text x="%d" y="34" font-size="20" font-weight="700" fill="%s">%s</text>`,
		padX, colInk, esc(in.Title))
	for i, line := range modeLines {
		fmt.Fprintf(&b, `<text x="%d" y="%d" font-size="12" fill="%s">%s</text>`,
			padX, 56+i*16, colMuted, esc(line))
	}
	fmt.Fprintf(&b, `<text x="%d" y="%d" font-size="12" font-weight="600" fill="%s">%s</text>`,
		padX, 56+len(modeLines)*16+4, colMuted, esc("станом на "+in.AsOf))
	if in.Note != "" {
		fmt.Fprintf(&b, `<text x="%d" y="%d" font-size="11" fill="%s">%s</text>`,
			padX, height-9, colMuted, esc(in.Note))
	}

	plotW := width - padX*2 - labelW - 120

	// Панель 1 — латентність.
	maxLat := 0.0
	for _, r := range rows {
		if v := float64(r.MedianLatency); v > maxLat {
			maxLat = v
		}
	}
	y := headerH
	b.WriteString(panel(padX, y, width-padX*2, panelH,
		"Латентність моделі, медіана (мс)", fmt.Sprintf("шкала 0–%s", ms(time.Duration(maxLat)))))
	for i, r := range rows {
		ty := y + 46 + i*rowH
		b.WriteString(bar(padX+16, ty, labelW, plotW, barH, r.Config,
			frac(float64(r.MedianLatency), maxLat), colLatency, ms(r.MedianLatency)))
	}

	// Панель 2 — вартість, два виміри.
	maxCost := 0.0
	for _, r := range rows {
		maxCost = maxf(maxCost, r.HarnessCostUSD, r.GatewayCostUSD)
	}
	y += panelH + panelGap
	b.WriteString(panel(padX, y, width-padX*2, panelH,
		"Вартість прогону, USD — два виміри однієї величини",
		fmt.Sprintf("шкала 0–%.8f", maxCost)))
	fmt.Fprintf(&b, `<rect x="%d" y="%d" width="10" height="10" fill="%s"/><text x="%d" y="%d" font-size="11" fill="%s">харнес (усередині агента)</text>`,
		padX+330, y+22, colHarness, padX+346, y+31, colMuted)
	fmt.Fprintf(&b, `<rect x="%d" y="%d" width="10" height="10" fill="%s"/><text x="%d" y="%d" font-size="11" fill="%s">гейтвей (на межі системи)</text>`,
		padX+520, y+22, colGateway, padX+536, y+31, colMuted)
	for i, r := range rows {
		ty := y + 46 + i*rowH
		b.WriteString(bar(padX+16, ty, labelW, plotW, barH/2, r.Config,
			frac(r.HarnessCostUSD, maxCost), colHarness, fmt.Sprintf("%.8f", r.HarnessCostUSD)))
		label := "гейтвей не виміряв"
		if r.GatewayCostUSD > 0 {
			label = fmt.Sprintf("%.8f", r.GatewayCostUSD)
		}
		b.WriteString(bar(padX+16, ty+barH/2+1, labelW, plotW, barH/2, "",
			frac(r.GatewayCostUSD, maxCost), colGateway, label))
	}

	// Панель 3 — формат.
	y += panelH + panelGap
	b.WriteString(panel(padX, y, width-padX*2, panelH,
		"Виконання контракту відповіді (0–4: Ранок/День/Вечір/Бюджет)",
		"це структура, а НЕ якість плану"))
	for i, r := range rows {
		ty := y + 46 + i*rowH
		b.WriteString(bar(padX+16, ty, labelW, plotW, barH, r.Config,
			frac(r.FormatAvg, 4), colFormat, fmt.Sprintf("%.2f / 4", r.FormatAvg)))
	}

	b.WriteString(`</svg>`)
	return b.String()
}

// panel малює плашку панелі із заголовком і підписом шкали.
func panel(x, y, w, h int, title, scale string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `<rect x="%d" y="%d" width="%d" height="%d" rx="8" fill="%s" stroke="%s"/>`,
		x, y, w, h, colCard, colGrid)
	fmt.Fprintf(&b, `<text x="%d" y="%d" font-size="13" font-weight="600" fill="%s">%s</text>`,
		x+16, y+24, colInk, esc(title))
	fmt.Fprintf(&b, `<text x="%d" y="%d" font-size="11" fill="%s" text-anchor="end">%s</text>`,
		x+w-16, y+24, colMuted, esc(scale))
	return b.String()
}

// bar малює підпис, стовпчик і значення.
//
// Порожній label означає «другий стовпчик тієї самої групи» — підпис не
// повторюється, інакше очі читають дві різні конфігурації там, де одна.
func bar(x, y, labelW, plotW, h int, label string, f float64, color, value string) string {
	var b strings.Builder
	if label != "" {
		fmt.Fprintf(&b, `<text x="%d" y="%d" font-size="11" fill="%s">%s</text>`,
			x, y+h-3, colInk, esc(trim(label, 36)))
	}
	w := int(f * float64(plotW))
	if w < 1 && f > 0 {
		w = 1 // вимірене, але дуже маленьке значення не має зникати з графіка
	}
	fmt.Fprintf(&b, `<rect x="%d" y="%d" width="%d" height="%d" rx="2" fill="%s"/>`,
		x+labelW, y, w, h, color)
	fmt.Fprintf(&b, `<text x="%d" y="%d" font-size="10" fill="%s">%s</text>`,
		x+labelW+w+6, y+h-3, colMuted, esc(value))
	return b.String()
}

// frac нормалізує значення до [0,1], переживаючи нульовий максимум.
func frac(v, max float64) float64 {
	if max <= 0 || v <= 0 {
		return 0
	}
	if v > max {
		return 1
	}
	return v / max
}

func maxf(vals ...float64) float64 {
	out := 0.0
	for _, v := range vals {
		if v > out {
			out = v
		}
	}
	return out
}

// wrap ріже рядок на слова так, щоб кожен рядок влазив у ширину графіка.
//
// Без цього довгий підзаголовок (а він довгий навмисно — це застереження про
// те, що саме виміряно) виїжджає за праву межу SVG і просто зникає.
func wrap(s string, width int) []string {
	words := strings.Fields(s)
	if len(words) == 0 {
		return nil
	}
	var (
		lines []string
		cur   []rune
	)
	for _, w := range words {
		wr := []rune(w)
		if len(cur) > 0 && len(cur)+1+len(wr) > width {
			lines = append(lines, string(cur))
			cur = nil
		}
		if len(cur) > 0 {
			cur = append(cur, ' ')
		}
		cur = append(cur, wr...)
	}
	if len(cur) > 0 {
		lines = append(lines, string(cur))
	}
	return lines
}

func trim(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// esc екранує текст: підпис конфігурації приходить із JSON, і `&` у ньому
// зробив би SVG невалідним — GitHub тоді показує битий значок замість графіка.
func esc(s string) string { return html.EscapeString(s) }
