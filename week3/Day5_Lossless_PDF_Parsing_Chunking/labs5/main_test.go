// Скелет тестів для ДЗ 5.
//
// Homework.md, пункт 7, вимагає табличні тести на кожен вузол графа через
// agent.StrictContextMock — і `go test ./...` зеленим. Цей файл дає готову
// форму, щоб ви не витрачали перші 40 хвилин на «як узагалі підсунути
// agent.Context у функцію вузла».
//
// ЯК ЦИМ КОРИСТУВАТИСЬ
//
//	go test ./...            — зараз зелено: тести перевіряють ЗАГЛУШКИ
//	                           (один гігантський чанк — це поточна поведінка).
//	                           Це навмисно: зелений старт означає, що
//	                           середовище живе, і далі ви ламаєте тести
//	                           свідомо, а не воюєте з інструментами.
//
// Ваш шлях: реалізуєте TODO у pipeline.go → тести нижче ПАДАЮТЬ, бо вони
// описують стару поведінку → переписуєте їх під нову. Місця, які треба
// переписати, позначені TODO(студент). Тест «таблиця не ріжеться»
// (TestChunk_TableStaysWhole) уже описує ЦІЛЬОВУ поведінку і має падати, доки
// chunk — заглушка: це контракт ДЗ, а не заготовка. Він єдиний тут із t.Skip —
// зніміть skip, коли візьметесь за chunking.
//
// Тестам не потрібні ні Python, ні docling: PDF-шлях перевіряється фейковим
// конвертером (fakeConverter). Справжній docling-mcp перевіряє opt-in тест
// internal/docling (DOCLING_E2E=1), а його реальний вихід для тестового PDF
// збережено в testdata/ledgerworks_soc2.docling.md.
//
// Чому StrictContextMock, а не nil: вузли приймають agent.Context. Передати
// туди nil можна лише доти, доки ви його не використовуєте; щойно у вашому
// коді з'явиться ctx.InvocationID() або емісія події, nil дасть panic у
// найгіршому місці — у проді. StrictContextMock панікує ГУЧНО і одразу на
// будь-якому методі, про який ви не подумали, тож помилка знаходиться в тесті,
// а не на демо.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/adk/v2/agent"

	"github.com/dimetron/ai-eng-course/labs/internal/labrun"
	"github.com/dimetron/ai-eng-course/labs/week3/Day5_Lossless_PDF_Parsing_Chunking/labs5/internal/chunkeval"
	"github.com/dimetron/ai-eng-course/labs/week3/Day5_Lossless_PDF_Parsing_Chunking/labs5/internal/kgraph"
	"github.com/dimetron/ai-eng-course/labs/week3/Day5_Lossless_PDF_Parsing_Chunking/labs5/internal/vecindex"
)

// nodeCtx — суворий фейк agent.Context. Вбудований StrictContextMock означає:
// будь-який метод, який ваш вузол викличе, а ми його тут не передбачили,
// впаде з panic, а не поверне тихо нульове значення.
type nodeCtx struct {
	agent.StrictContextMock
}

func newNodeCtx() *nodeCtx {
	return &nodeCtx{StrictContextMock: agent.NewStrictContextMock(context.Background())}
}

// fakeConverter підміняє docling-mcp: повертає заданий Markdown або помилку.
type fakeConverter struct {
	markdown string
	err      error
}

func (f fakeConverter) ToMarkdown(context.Context, string) (string, error) {
	return f.markdown, f.err
}

const (
	fixtureMD     = "testdata/ledgerworks_soc2.md"
	fixturePDF    = "testdata/ledgerworks_soc2.pdf"
	fixtureDocMD  = "testdata/ledgerworks_soc2.docling.md"
	fixtureTables = 3
)

// --- Вузол load --------------------------------------------------------------

func TestLoad(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	docx := filepath.Join(dir, "doc.docx")
	if err := os.WriteFile(docx, []byte("PK\x03\x04"), 0o600); err != nil {
		t.Fatalf("підготовка фікстури: %v", err)
	}

	tests := []struct {
		name     string
		path     string
		wantKind string
		wantErr  bool
	}{
		{name: "markdown", path: fixtureMD, wantKind: "markdown"},
		{name: "pdf за magic bytes", path: fixturePDF, wantKind: "pdf"},
		{name: "пробіли навколо шляху", path: "  " + fixtureMD + "\n", wantKind: "markdown"},
		{name: "файла немає", path: filepath.Join(dir, "ghost.md"), wantErr: true},
		{name: "порожній шлях", path: "", wantErr: true},
		{name: "непідтримуваний формат", path: docx, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := load(newNodeCtx(), tt.path)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("load(%q) = %+v, err = nil; очікували помилку", tt.path, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("load(%q) error = %v", tt.path, err)
			}
			if got.Kind != tt.wantKind {
				t.Errorf("load(%q).Kind = %q, очікували %q", tt.path, got.Kind, tt.wantKind)
			}
		})
	}
}

// --- Вузол parse -------------------------------------------------------------

func TestParse(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		conv     fakeConverter
		src      Source
		contains string
		wantErr  string
	}{
		{name: "markdown читається як є", src: Source{Path: fixtureMD, Kind: "markdown"}, contains: "| F-101"},
		{name: "pdf іде в конвертер", conv: fakeConverter{markdown: "| a |\n|---|\n| 1 |"}, src: Source{Path: fixturePDF, Kind: "pdf"}, contains: "| 1 |"},
		{name: "помилка конвертера", conv: fakeConverter{err: errors.New("docling down")}, src: Source{Path: fixturePDF, Kind: "pdf"}, wantErr: "docling down"},
		{name: "порожній результат", conv: fakeConverter{markdown: "  \n"}, src: Source{Path: fixturePDF, Kind: "pdf"}, wantErr: "порожній"},
		{name: "markdown-файла немає", src: Source{Path: "testdata/ghost.md", Kind: "markdown"}, wantErr: "ghost.md"},
		{name: "невідомий тип", src: Source{Path: fixtureMD, Kind: "docx"}, wantErr: "невідомий тип"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := newParse(tt.conv)(newNodeCtx(), tt.src)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("parse() err = %v, очікували %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parse() error = %v", err)
			}
			if got.ID != "ledgerworks_soc2" {
				t.Errorf("Document.ID = %q, очікували ім'я файла без розширення", got.ID)
			}
			if !strings.Contains(got.Markdown, tt.contains) {
				t.Errorf("Markdown не містить %q", tt.contains)
			}
		})
	}
}

// TestDoclingFixtureKeepsTables фіксує, що дає docling на тестовому PDF: усі
// три таблиці — цілими, із заголовками. Зверніть увагу на пастку: docling
// сплющує рівні заголовків — і "1 Scope", і "1.1 Vendors in scope" стають
// "##". Ієрархію для Parent-Child доведеться відновлювати з нумерації розділів.
func TestDoclingFixtureKeepsTables(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(fixtureDocMD)
	if err != nil {
		t.Fatal(err)
	}
	md := string(data)
	if got := strings.Count(md, "\n|--"); got != fixtureTables {
		t.Errorf("таблиць у виході docling = %d, очікували %d", got, fixtureTables)
	}
	if strings.Contains(md, "\n### ") {
		t.Log("docling повернув ### — рівні заголовків збережено; перевірте свою логіку ієрархії")
	}
}

// --- Вузол chunk -------------------------------------------------------------

// TestChunk_Hierarchy перевіряє Parent-Child ієрархію (замінив тест заглушки).
//
// Таблична форма: кожен кейс — окремий документ і очікувана форма нарізки.
// Головне, що тут фіксується: підрозділ (Depth > 1) НЕ створює нового parent,
// а лише подовжує SectionPath — так само влаштована фікстура ДЗ 6.
func TestChunk_Hierarchy(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		md           string
		wantParents  int
		wantChildren int
		wantPath     string
	}{
		{
			name:        "розділ з абзацом",
			md:          "# 1 Розділ\n\nабзац\n",
			wantParents: 1, wantChildren: 1, wantPath: "1 Розділ",
		},
		{
			name:        "підрозділ подовжує шлях, але не створює parent",
			md:          "# 1 Розділ\n\n## 1.1 Підрозділ\n\nабзац\n",
			wantParents: 1, wantChildren: 1, wantPath: "1 Розділ / 1.1 Підрозділ",
		},
		{
			name:        "новий розділ скидає глибші рівні",
			md:          "# 1 Перший\n\n## 1.1 Під\n\nа\n\n# 2 Другий\n\nб\n",
			wantParents: 2, wantChildren: 2, wantPath: "2 Другий",
		},
		{
			name:        "блок до першого заголовка дістає неявного parent",
			md:          "абзац без заголовка\n",
			wantParents: 1, wantChildren: 1, wantPath: "d1",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := chunk(newNodeCtx(), newDocument("d1", "", tc.md))
			if err != nil {
				t.Fatalf("chunk() error = %v", err)
			}
			var parents, children int
			var lastPath string
			for _, c := range got.Chunks {
				if c.ParentID == "" {
					parents++
					if c.Level != 0 {
						t.Errorf("parent %s має Level %d, очікували 0", c.ID, c.Level)
					}
					continue
				}
				children++
				lastPath = c.SectionPath
				if c.Level != 2 {
					t.Errorf("child %s має Level %d, очікували 2", c.ID, c.Level)
				}
				if c.DocumentID == "" {
					t.Errorf("child %s без DocumentID", c.ID)
				}
			}
			if parents != tc.wantParents || children != tc.wantChildren {
				t.Fatalf("parents/children = %d/%d, очікували %d/%d",
					parents, children, tc.wantParents, tc.wantChildren)
			}
			if lastPath != tc.wantPath {
				t.Errorf("SectionPath останнього child = %q, очікували %q", lastPath, tc.wantPath)
			}
		})
	}
}

// TestChunk_LongTextSplitsBySentences — довгий абзац ріжеться, але по реченнях.
func TestChunk_LongTextSplitsBySentences(t *testing.T) {
	t.Parallel()
	long := "# 1 Розділ\n\n" + strings.Repeat("Це речення має рівно кілька слів усередині. ", 60) + "\n"
	got, err := chunk(newNodeCtx(), newDocument("d1", "", long))
	if err != nil {
		t.Fatalf("chunk() error = %v", err)
	}
	var children int
	for _, c := range got.Chunks {
		if c.ParentID == "" {
			continue
		}
		children++
		if n := chunkeval.Tokens(c.Text); n > maxChildTokens {
			t.Errorf("child %s має %d токенів, стеля %d", c.ID, n, maxChildTokens)
		}
		if !strings.HasSuffix(strings.TrimSpace(c.Text), ".") {
			t.Errorf("child %s обірвано не на межі речення: %q", c.ID, c.Text)
		}
	}
	if children < 2 {
		t.Fatalf("довгий текст дав %d children, очікували кілька", children)
	}
}

func TestChunk_EmptyDocumentIsError(t *testing.T) {
	t.Parallel()
	if _, err := chunk(newNodeCtx(), Document{}); err == nil {
		t.Fatal("chunk(порожній) = nil error; порожній документ має бути помилкою")
	}
}

// TestChunk_TableStaysWhole — КОНТРАКТ ДЗ, а не заготовка.
//
// Головна вимога тижня: жодна таблиця не може бути розрізана між двома
// чанками. Тест перевіряє це напряму на реальному виході docling: кожна
// таблиця — рівно один чанк Kind == "table", з усіма рядками й заголовком.
//
// TODO(студент): приберіть t.Skip, коли візьметесь за вузол chunk.
func TestChunk_TableStaysWhole(t *testing.T) {

	data, err := os.ReadFile(fixtureDocMD)
	if err != nil {
		t.Fatal(err)
	}
	got, err := chunk(newNodeCtx(), newDocument("ledgerworks_soc2", fixtureDocMD, string(data)))
	if err != nil {
		t.Fatalf("chunk() error = %v", err)
	}

	var tables []Chunk
	for _, c := range got.Chunks {
		if c.Kind == "table" {
			tables = append(tables, c)
		}
	}
	if len(tables) != fixtureTables {
		t.Fatalf("чанків типу table = %d, очікували %d — таблицю розрізано або втрачено", len(tables), fixtureTables)
	}
	wantRows := map[string][]string{
		"Vendor":     {"Acme Bank JSC", "Northwind Cloud", "Globex Payments"},
		"Finding ID": {"F-101", "F-102", "F-103", "F-104", "F-105", "Severity"},
		"Merchant":   {"A-114", "A-207", "A-331", "NBU 2026 requirement"},
	}
	for header, rows := range wantRows {
		var whole bool
		for _, c := range tables {
			if !strings.HasPrefix(firstTableRow(c.Text), "| "+header) {
				continue
			}
			whole = true
			for _, row := range rows {
				if !strings.Contains(c.Text, row) {
					whole = false
					t.Errorf("у таблиці %q немає %q — рядок або заголовок втрачено:\n%s", header, row, c.Text)
				}
			}
			if c.ParentID == "" {
				t.Errorf("таблиця %q без ParentID — зв'язок із розділом втрачено", header)
			}
		}
		if !whole {
			t.Errorf("таблицю з заголовком %q не знайдено цілою", header)
		}
	}
}

// firstTableRow — перший рядок таблиці (рядок заголовків), щоб "Vendor" у
// таблиці знахідок не сплутати з таблицею вендорів.
func firstTableRow(text string) string {
	for line := range strings.Lines(text) {
		if line = strings.TrimSpace(line); strings.HasPrefix(line, "|") {
			return line
		}
	}
	return ""
}

// --- Вузли extract_entities / index_vector / index_graph ---------------------

// TestStubNodesPassThrough фіксує заглушки: вузли повертають batch без змін.
//
// TODO(студент): замініть на табличні тести ваших вузлів — наприклад,
// extract_entities знаходить Vendor "Acme Bank JSC" із SourceChunkID;
// index_vector дає VectorsAdded == кількість children і st.Vector.Len() те
// саме; index_graph не створює дублікатів (NodesAdded ≤ len(Entities)).
func TestStubNodesPassThrough(t *testing.T) {
	t.Parallel()
	in := Batch{Chunks: []Chunk{{ID: "c1", Text: "Acme Bank JSC"}}}
	st := NewStores()
	nodes := map[string]func(agent.Context, Batch) (Batch, error){
		"extract_entities": extractEntities,
		"index_vector":     newIndexVector(st.Vector),
		"index_graph":      newIndexGraph(st.Graph),
	}
	for name, node := range nodes {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := node(newNodeCtx(), in)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if len(got.Chunks) != len(in.Chunks) {
				t.Errorf("%s загубив чанки: %d → %d", name, len(in.Chunks), len(got.Chunks))
			}
		})
	}
}

// --- Вузол report ------------------------------------------------------------

// TODO(студент): за Homework п.5 report має друкувати статистику за рівнями,
// розміри (min/avg/max) і `Tables: K/K preserved`. Розширте таблицю кейсів,
// коли реалізуєте це.
func TestReport(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		chunks   []Chunk
		contains string
	}{
		{name: "порожній вхід", chunks: nil, contains: "0"},
		{name: "один чанк", chunks: []Chunk{{ID: "c1"}}, contains: "1"},
		{name: "три чанки", chunks: []Chunk{{ID: "c1"}, {ID: "c2"}, {ID: "c3"}}, contains: "3"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := report(newNodeCtx(), Batch{Chunks: tt.chunks})
			if err != nil {
				t.Fatalf("report() error = %v", err)
			}
			if !strings.Contains(got, tt.contains) {
				t.Errorf("report() = %q; очікували підрядок %q", got, tt.contains)
			}
		})
	}
}

// --- Граф цілком ------------------------------------------------------------

// TestGraph_EarlyWin проганяє весь граф так само, як `go run . console`, але
// без launcher-а: ранній win із Homework, відтворюваний у CI.
func TestGraph_EarlyWin(t *testing.T) {
	t.Parallel()
	a, err := newGraph(fakeConverter{}, NewStores())
	if err != nil {
		t.Fatalf("newGraph: %v", err)
	}
	res, err := labrun.Run(context.Background(), a, fixtureMD)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, want := range []string{"Parents:", "Children:", "Tables: 3/3 preserved"} {
		if !strings.Contains(res.Final, want) {
			t.Errorf("у звіті немає %q:\n%s", want, res.Final)
		}
	}
	// 7 вузлів після Start — 7 подій: жоден вузол не випав із ланцюжка.
	if got := len(res.Events); got != 7 {
		t.Errorf("подій = %d, очікували 7 (load … report)", got)
	}
}

// --- Контракт із ДЗ 6 --------------------------------------------------------

// TestChunkJSONContract: labs6 читає ваші чанки як JSON. Імена полів — це
// контракт між ДЗ 5 і ДЗ 6; якщо тест упав, ви перейменували поле.
func TestChunkJSONContract(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(Chunk{ID: "c", DocumentID: "d", ParentID: "p", Level: 2, Kind: "table", SectionPath: "s", Text: "t"})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"id"`, `"document_id"`, `"parent_id"`, `"level"`, `"kind"`, `"section_path"`, `"text"`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("JSON чанка без поля %s: %s", key, raw)
		}
	}
}

// --- Вузли extract_entities та index_* ---------------------------------------

// fixtureBatch проганяє фікстуру через chunk — спільна основа для тестів нижче.
func fixtureBatch(t *testing.T) Batch {
	t.Helper()
	data, err := os.ReadFile(fixtureDocMD)
	if err != nil {
		t.Fatal(err)
	}
	b, err := chunk(newNodeCtx(), newDocument("ledgerworks_soc2", fixtureDocMD, string(data)))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestExtractEntities перевіряє, що сутності беруться з таблиць і несуть provenance.
func TestExtractEntities(t *testing.T) {
	t.Parallel()
	got, err := extractEntities(newNodeCtx(), fixtureBatch(t))
	if err != nil {
		t.Fatalf("extractEntities: %v", err)
	}
	if len(got.Entities) == 0 {
		t.Fatal("жодної сутності — таблиці не розібрано")
	}

	byType := map[string]int{}
	for _, e := range got.Entities {
		byType[e.Type]++
		if e.SourceChunkID == "" {
			t.Errorf("сутність %s без SourceChunkID — provenance втрачено", e.ID)
		}
		if !strings.HasPrefix(e.ID, e.Type+":") {
			t.Errorf("ID %q не починається з типу %q", e.ID, e.Type)
		}
	}
	for _, want := range []string{"vendor", "finding", "merchant"} {
		if byType[want] == 0 {
			t.Errorf("немає жодної сутності типу %q; є: %v", want, byType)
		}
	}
}

// TestEntityIDIsStable — та сама назва з різним регістром дає той самий ID.
//
// Без цього «Acme Bank JSC» і «acme bank jsc» стали б двома вузлами графа, і
// multi-hop запит ДЗ 6 знайшов би половину зв'язків.
func TestEntityIDIsStable(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ in, want string }{
		{"Acme Bank JSC", "vendor:acme-bank-jsc"},
		{"acme bank jsc", "vendor:acme-bank-jsc"},
		{"  Acme   Bank  JSC ", "vendor:acme-bank-jsc"},
	} {
		if got := entityID("vendor", tc.in); got != tc.want {
			t.Errorf("entityID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestIndexVectorSkipsParents — у пошук ідуть лише children.
func TestIndexVectorSkipsParents(t *testing.T) {
	t.Parallel()
	idx := vecindex.New()
	b, err := newIndexVector(idx)(newNodeCtx(), fixtureBatch(t))
	if err != nil {
		t.Fatalf("index_vector: %v", err)
	}
	var children int
	for _, c := range b.Chunks {
		if c.ParentID != "" {
			children++
		}
	}
	if idx.Len() != children {
		t.Fatalf("в індексі %d записів, children %d — parents потрапили в пошук", idx.Len(), children)
	}
	if b.VectorsAdded != children {
		t.Errorf("VectorsAdded = %d, очікували %d", b.VectorsAdded, children)
	}
}

// TestIndexGraphMergesDuplicates — повтор сутності зливає provenance,
// а не створює другий вузол.
func TestIndexGraphMergesDuplicates(t *testing.T) {
	t.Parallel()
	b := fixtureBatch(t)
	b, err := extractEntities(newNodeCtx(), b)
	if err != nil {
		t.Fatal(err)
	}
	g := kgraph.New()
	b, err = newIndexGraph(g)(newNodeCtx(), b)
	if err != nil {
		t.Fatalf("index_graph: %v", err)
	}

	unique := map[string]bool{}
	for _, e := range b.Entities {
		unique[e.ID] = true
	}
	nodes, edges := g.Stats()
	if nodes != len(unique) {
		t.Fatalf("вузлів %d, унікальних сутностей %d — дублікати не злилися", nodes, len(unique))
	}
	if b.NodesAdded != len(unique) {
		t.Errorf("NodesAdded = %d, очікували %d (рахуємо обʼєкти, не згадки)", b.NodesAdded, len(unique))
	}
	if edges == 0 {
		t.Error("жодного ребра — звʼязки з рядків таблиці не побудовано")
	}

	// Vendor → Finding: саме цей перехід потрібен multi-hop запиту ДЗ 6.
	acme := entityID("vendor", "Acme Bank JSC")
	if findings := g.Neighbors(acme, "has_finding"); len(findings) == 0 {
		t.Errorf("у %s немає сусідів has_finding — граф не зв'язує рядок таблиці", acme)
	}
}
