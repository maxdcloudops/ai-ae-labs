package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"google.golang.org/adk/v2/agent"

	"github.com/dimetron/ai-eng-course/labs/week3/Day5_Lossless_PDF_Parsing_Chunking/labs5/internal/chunkeval"
	"github.com/dimetron/ai-eng-course/labs/week3/Day5_Lossless_PDF_Parsing_Chunking/labs5/internal/kgraph"
	"github.com/dimetron/ai-eng-course/labs/week3/Day5_Lossless_PDF_Parsing_Chunking/labs5/internal/mdblocks"
	"github.com/dimetron/ai-eng-course/labs/week3/Day5_Lossless_PDF_Parsing_Chunking/labs5/internal/vecindex"
)

// Source — документ, який прийшов на вхід конвеєра.
type Source struct {
	Path string `json:"path"`
	Kind string `json:"kind"` // "pdf" | "markdown"
}

// Document — документ після lossless-парсингу: увесь зміст у Markdown,
// таблиці — Markdown-таблицями із заголовками стовпчиків. Blocks — той самий
// Markdown, уже розкладений на заголовки / абзаци / списки / таблиці
// (internal/mdblocks). Саме з Blocks ви будуєте чанки.
type Document struct {
	ID       string           `json:"id"`
	Path     string           `json:"path"`
	Markdown string           `json:"markdown"`
	Blocks   []mdblocks.Block `json:"blocks"`
}

// Chunk — одиниця майбутньої бази знань Research Agent.
//
// Цей JSON-формат — контракт із ДЗ 6: labs6 читає саме такі чанки
// (див. labs6/testdata/chunks.json). Додавайте поля, але не перейменовуйте
// наявні — інакше ДЗ 6 не прочитає ваш індекс.
type Chunk struct {
	ID          string `json:"id"`
	DocumentID  string `json:"document_id"`
	ParentID    string `json:"parent_id,omitempty"`    // порожній для батьківських чанків
	Level       int    `json:"level"`                  // 0 = розділ, 1 = підрозділ, 2 = абзац/таблиця
	Kind        string `json:"kind"`                   // "heading" | "text" | "table" | "list"
	SectionPath string `json:"section_path,omitempty"` // напр. "2 Findings / 2.1 Security Findings"
	Text        string `json:"text"`
}

// Entity — сутність, витягнута з чанка (Vendor, Finding, Merchant, Contract…).
// SourceChunkID — provenance: без нього аудитор не знайде первинний фрагмент.
type Entity struct {
	ID            string `json:"id"`
	Type          string `json:"type"`
	Name          string `json:"name"`
	SourceChunkID string `json:"source_chunk_id"`
}

// Batch тече вузлами після chunk: кожен вузол додає свою частину.
type Batch struct {
	Document     Document `json:"document"`
	Chunks       []Chunk  `json:"chunks"`
	Entities     []Entity `json:"entities,omitempty"`
	VectorsAdded int      `json:"vectors_added"`
	NodesAdded   int      `json:"nodes_added"`
}

// MarkdownConverter перетворює PDF на Markdown. У main.go це docling-mcp
// (internal/docling); у тестах — фейк без мережі й без Python.
type MarkdownConverter interface {
	ToMarkdown(ctx context.Context, path string) (string, error)
}

// Stores — сховища, які наповнюють вузли index_*. Вони живуть поза графом:
// вузли лишаються функціями, а після прогону тест (або ваш A1) читає сховища
// напряму.
type Stores struct {
	Vector *vecindex.Index
	Graph  *kgraph.Graph
}

// NewStores повертає порожні in-memory сховища.
func NewStores() Stores {
	return Stores{Vector: vecindex.New(), Graph: kgraph.New()}
}

// load перевіряє вхід: шлях із повідомлення користувача → Source.
// Тип визначається за вмістом (magic bytes %PDF), а не лише за розширенням.
func load(_ agent.Context, path string) (Source, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return Source{}, fmt.Errorf("порожній шлях: введіть шлях до PDF або Markdown")
	}
	f, err := os.Open(path)
	if err != nil {
		return Source{}, fmt.Errorf("не вдалося прочитати %q: %w", path, err)
	}
	defer f.Close()
	head := make([]byte, 5)
	n, _ := f.Read(head)
	if bytes.HasPrefix(head[:n], []byte("%PDF-")) {
		return Source{Path: path, Kind: "pdf"}, nil
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".md", ".markdown", ".txt":
		return Source{Path: path, Kind: "markdown"}, nil
	}
	return Source{}, fmt.Errorf("%q: непідтримуваний формат — очікуємо PDF або Markdown", path)
}

// newParse повертає вузол parse. PDF іде в docling-mcp, Markdown читається як є;
// далі mdblocks розкладає Markdown на блоки.
//
// TODO(студент): перевірте на своєму PDF, що кожна таблиця вийшла цілою
// Markdown-таблицею із заголовками (mdblocks.Tables(doc.Blocks) — швидкий
// лічильник). Сканований PDF без текстового шару потребує OCR — docling уміє,
// але це повільніше; зазначте в README.
func newParse(conv MarkdownConverter) func(agent.Context, Source) (Document, error) {
	return func(ctx agent.Context, src Source) (Document, error) {
		var md string
		switch src.Kind {
		case "pdf":
			out, err := conv.ToMarkdown(ctx, src.Path)
			if err != nil {
				return Document{}, fmt.Errorf("parse %q: %w", src.Path, err)
			}
			md = out
		case "markdown":
			data, err := os.ReadFile(src.Path)
			if err != nil {
				return Document{}, fmt.Errorf("parse %q: %w", src.Path, err)
			}
			md = string(data)
		default:
			return Document{}, fmt.Errorf("parse: невідомий тип %q", src.Kind)
		}
		if strings.TrimSpace(md) == "" {
			return Document{}, fmt.Errorf("parse %q: порожній документ", src.Path)
		}
		return newDocument(documentID(src.Path), src.Path, md), nil
	}
}

// newDocument — єдиний спосіб зібрати Document: Blocks завжди відповідають
// Markdown. У своїх тестах будуйте документи теж через нього.
func newDocument(id, path, md string) Document {
	return Document{ID: id, Path: path, Markdown: md, Blocks: mdblocks.Parse(md)}
}

// documentID — стабільний ідентифікатор документа з імені файла.
func documentID(path string) string {
	return strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
}

// chunk нарізає документ ієрархічно на Parent-Child.
//
// Дві вимоги тягнуть у різні боки. Для ПОШУКУ потрібні дрібні чанки: ембединг
// довгого тексту розмазується й перестає відрізняти одну тему від іншої. Для
// ВІДПОВІДІ потрібні великі: за уривком у три рядки модель не відновить
// контекст. Тому зберігаємо обидва рівні: шукаємо по children, а моделі
// віддаємо parent за ParentID.
//
// Межі — за структурою документа, а не за лічильником токенів. Заголовок
// Depth == 1 відкриває новий parent; Depth > 1 лише подовжує SectionPath, бо
// підрозділ — це уточнення місця, а не новий розділ (так само влаштована
// фікстура ДЗ 6: таблиця з «1.1 Vendors in scope» має ParentID розділу «1 Scope»).
//
// Таблиця — РІВНО один child, байт у байт. Це контракт, а не оптимізація:
// таблиця, розрізана між двома чанками, втрачає заголовки стовпчиків, і
// «| Acme | High | 3 |» перестає означати будь-що.
func chunk(_ agent.Context, doc Document) (Batch, error) {
	if strings.TrimSpace(doc.Markdown) == "" {
		return Batch{}, fmt.Errorf("порожній документ")
	}

	b := Batch{Document: doc}
	// path тримає заголовки за рівнями: path[1] = "1 Scope", path[2] = "1.1 …".
	path := map[int]string{}
	maxDepth := 0
	parentID := ""
	parents, children := 0, 0

	// sectionPath збирає шлях від найменшої глибини до поточної.
	sectionPath := func() string {
		var parts []string
		for d := 1; d <= maxDepth; d++ {
			if t := path[d]; t != "" {
				parts = append(parts, t)
			}
		}
		return strings.Join(parts, " / ")
	}

	openParent := func(title string, depth int) {
		parents++
		parentID = fmt.Sprintf("p-%d", parents)
		if depth <= 1 {
			// Новий розділ скидає глибші рівні: «2 Findings» не успадковує «1.1».
			path = map[int]string{1: title}
			maxDepth = 1
		}
		b.Chunks = append(b.Chunks, Chunk{
			ID: parentID, DocumentID: doc.ID, Level: 0, Kind: string(mdblocks.KindHeading),
			SectionPath: sectionPath(), Text: title,
		})
	}

	addChild := func(kind, text string) {
		if strings.TrimSpace(text) == "" {
			return
		}
		if parentID == "" {
			// Блок до першого заголовка все одно потребує батька: інакше
			// знайдений child нікуди не розгорнути.
			openParent(doc.ID, 1)
		}
		children++
		prefix := "c"
		if kind == string(mdblocks.KindTable) {
			prefix = "t"
		}
		b.Chunks = append(b.Chunks, Chunk{
			ID: fmt.Sprintf("%s-%d", prefix, children), DocumentID: doc.ID,
			ParentID: parentID, Level: 2, Kind: kind,
			SectionPath: sectionPath(), Text: text,
		})
	}

	for _, blk := range doc.Blocks {
		switch blk.Kind {
		case mdblocks.KindHeading:
			// blk.Title уже містить номер розділу («1 Scope»), тож
			// доклеювати blk.Number не треба — вийде «1 1 Scope».
			title := strings.TrimSpace(blk.Title)
			if title == "" {
				title = strings.TrimSpace(blk.Text)
			}
			if blk.Depth > 1 {
				// Підрозділ: подовжуємо шлях, parent лишається той самий.
				for d := blk.Depth; d <= maxDepth; d++ {
					delete(path, d)
				}
				path[blk.Depth] = title
				if blk.Depth > maxDepth {
					maxDepth = blk.Depth
				}
				continue
			}
			openParent(title, 1)

		case mdblocks.KindTable:
			// Жодного ділення — навіть якщо таблиця довша за maxChildTokens.
			addChild(string(mdblocks.KindTable), blk.Text)

		default:
			text := blk.Text
			if chunkeval.Tokens(text) <= maxChildTokens {
				addChild(string(blk.Kind), text)
				continue
			}
			for _, part := range splitBySentences(text, maxChildTokens) {
				addChild(string(blk.Kind), part)
			}
		}
	}

	if len(b.Chunks) == 0 {
		return Batch{}, fmt.Errorf("документ не дав жодного чанка")
	}
	return b, nil
}

// splitBySentences ріже довгий текст по реченнях, не перевищуючи limit токенів.
//
// По реченнях, а не по словах: розрив усередині речення робить обидві половини
// гіршими за ціле — перша обривається, друга починається нізвідки.
func splitBySentences(text string, limit int) []string {
	sentences := sentenceSplit(text)
	var (
		out     []string
		cur     []string
		curSize int
	)
	flush := func() {
		if len(cur) > 0 {
			out = append(out, strings.Join(cur, " "))
			cur, curSize = nil, 0
		}
	}
	for _, s := range sentences {
		n := chunkeval.Tokens(s)
		if curSize > 0 && curSize+n > limit {
			flush()
		}
		cur = append(cur, s)
		curSize += n
	}
	flush()
	if len(out) == 0 {
		return []string{text}
	}
	return out
}

// sentenceSplit — простий розділювач речень для української та англійської.
func sentenceSplit(text string) []string {
	var (
		out []string
		cur strings.Builder
	)
	for _, r := range text {
		cur.WriteRune(r)
		if r == '.' || r == '!' || r == '?' {
			if s := strings.TrimSpace(cur.String()); s != "" {
				out = append(out, s)
			}
			cur.Reset()
		}
	}
	if s := strings.TrimSpace(cur.String()); s != "" {
		out = append(out, s)
	}
	return out
}

// maxChildTokens — стеля довжини текстового child (у chunkeval.Tokens).
// Таблиці на неї не зважають: краще великий цілий чанк, ніж дві половини.
const maxChildTokens = 200

// entityColumns — які стовпчики таблиць стають сутностями й якого типу.
//
// Перелік явний, а не «беремо все підряд»: сутність — це те, на що потім
// посилається граф і запит аудитора. «Fee, %» чи «Status» — атрибути рядка,
// а не самостійні об'єкти, і вузлами графа ставати не мають.
var entityColumns = map[string]string{
	"Vendor":     "vendor",
	"Finding ID": "finding",
	"Merchant":   "merchant",
	"Contract":   "contract",
	"Report":     "report",
	"Auditor":    "auditor",
}

// entityEdges — які зв'язки будувати між сутностями одного РЯДКА таблиці.
//
// Рядок таблиці — це вже готове відношення: «F-101 | Acme Bank JSC» означає,
// що у вендора Acme є знахідка F-101. Саме ці ребра й роблять можливим
// multi-hop запит ДЗ 6: Merchant → Contract, Vendor → Finding.
var entityEdges = []struct{ from, rel, to string }{
	{"Vendor", "has_finding", "Finding ID"},
	{"Vendor", "has_report", "Report"},
	{"Vendor", "audited_by", "Auditor"},
	{"Merchant", "has_contract", "Contract"},
}

// entityID робить стабільний ідентифікатор: "vendor:acme-bank-jsc".
//
// Стабільний — значить та сама сутність із двох різних таблиць дає той самий
// ID і зливається в один вузол графа з двома provenance-посиланнями, а не
// розмножується в дублікати.
func entityID(typ, name string) string {
	slug := strings.ToLower(strings.TrimSpace(name))
	slug = strings.Join(strings.FieldsFunc(slug, func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9')
	}), "-")
	return typ + ":" + slug
}

// extractEntities витягує сутності з табличних чанків.
//
// Саме з таблиць, а не з суцільного тексту: у таблиці структура вже є —
// стовпчик каже, що означає значення. Витягувати «Acme Bank JSC» з абзацу
// означало б вгадувати, а вгадування тут коштує дорого: сутність із
// неправильним типом тягне за собою неправильні ребра графа.
func extractEntities(_ agent.Context, b Batch) (Batch, error) {
	seen := map[string]bool{}
	for _, c := range b.Chunks {
		if c.Kind != string(mdblocks.KindTable) {
			continue
		}
		blocks := mdblocks.Parse(c.Text)
		if len(blocks) == 0 || blocks[0].Table == nil {
			continue
		}
		tbl := blocks[0].Table
		for _, header := range tbl.Header {
			typ, ok := entityColumns[header]
			if !ok {
				continue
			}
			for _, name := range tbl.Column(header) {
				name = strings.TrimSpace(name)
				if name == "" {
					continue
				}
				id := entityID(typ, name)
				// Ключ із чанком: та сама сутність із іншої таблиці має
				// додати ще одне provenance-посилання, а не зникнути.
				key := id + "@" + c.ID
				if seen[key] {
					continue
				}
				seen[key] = true
				b.Entities = append(b.Entities, Entity{
					ID: id, Type: typ, Name: name, SourceChunkID: c.ID,
				})
			}
		}
	}
	return b, nil
}

// newIndexVector повертає вузол index_vector над idx.
//
// Індексуємо ЛИШЕ children. Parents у пошук не йдуть навмисно: їхній текст —
// це заголовок розділу, він однаково схожий на будь-який запит і тільки
// засмічує видачу. Parent підвантажується після знаходження child за ParentID —
// у цьому вся суть Parent-Child.
//
// Ембединг тут — детермінований стаб (internal/vecindex), не модель. Для
// справжніх векторів є internal/embed з Ollama; у README це зазначено чесно.
func newIndexVector(idx *vecindex.Index) func(agent.Context, Batch) (Batch, error) {
	return func(_ agent.Context, b Batch) (Batch, error) {
		for _, c := range b.Chunks {
			if c.ParentID == "" {
				continue
			}
			if err := idx.Add(c.ID, c.Text); err != nil {
				return Batch{}, fmt.Errorf("index_vector %s: %w", c.ID, err)
			}
			b.VectorsAdded++
		}
		return b, nil
	}
}

// newIndexGraph повертає вузол index_graph над g.
//
// NodesAdded рахується лише коли AddNode повернув true: повтор тієї самої
// сутності — це злиття provenance, а не новий вузол. Якби рахували кожен
// виклик, статистика показувала б кількість згадок замість кількості об'єктів.
func newIndexGraph(g *kgraph.Graph) func(agent.Context, Batch) (Batch, error) {
	return func(_ agent.Context, b Batch) (Batch, error) {
		for _, e := range b.Entities {
			added, err := g.AddNode(kgraph.Node{
				ID: e.ID, Type: e.Type, Name: e.Name, Sources: []string{e.SourceChunkID},
			})
			if err != nil {
				return Batch{}, fmt.Errorf("index_graph %s: %w", e.ID, err)
			}
			if added {
				b.NodesAdded++
			}
		}
		if err := addRowEdges(g, b); err != nil {
			return Batch{}, err
		}
		return b, nil
	}
}

// addRowEdges будує ребра між сутностями, що стоять в ОДНОМУ рядку таблиці.
func addRowEdges(g *kgraph.Graph, b Batch) error {
	for _, c := range b.Chunks {
		if c.Kind != string(mdblocks.KindTable) {
			continue
		}
		blocks := mdblocks.Parse(c.Text)
		if len(blocks) == 0 || blocks[0].Table == nil {
			continue
		}
		tbl := blocks[0].Table
		for row := range tbl.Rows {
			for _, spec := range entityEdges {
				from, okF := tbl.Cell(row, spec.from)
				to, okT := tbl.Cell(row, spec.to)
				if !okF || !okT || strings.TrimSpace(from) == "" || strings.TrimSpace(to) == "" {
					continue
				}
				if _, err := g.AddEdge(kgraph.Edge{
					From:   entityID(entityColumns[spec.from], from),
					Rel:    spec.rel,
					To:     entityID(entityColumns[spec.to], to),
					Source: c.ID,
				}); err != nil {
					return fmt.Errorf("index_graph edge %s: %w", spec.rel, err)
				}
			}
		}
	}
	return nil
}

// report рахує статистику нарізки.
//
// Це graded-блок завдання, і причина та сама, що зі state_delta в ДЗ 3: без
// чисел твердження «таблиці збережено» неможливо перевірити. «Tables: K/K»
// порівнює таблиці ДОКУМЕНТА з таблицями, що лежать цілими в чанках, — тож
// K/K доводить lossless, а 3/4 одразу показує, що одну розрізано.
func report(_ agent.Context, b Batch) (string, error) {
	var parents, children int
	var childTexts, chunkTexts []string
	byKind := map[string]int{}
	for _, c := range b.Chunks {
		chunkTexts = append(chunkTexts, c.Text)
		if c.ParentID == "" {
			parents++
			continue
		}
		children++
		byKind[c.Kind]++
		childTexts = append(childTexts, c.Text)
	}

	var docTables []string
	for _, blk := range mdblocks.Tables(b.Document.Blocks) {
		docTables = append(docTables, blk.Text)
	}
	preserved := chunkeval.Preserved(docTables, chunkTexts)

	sizes := chunkeval.Measure(childTexts)

	// Експорт для ДЗ 6 (Homework п.8). Шлях задає CHUNKS_OUT; без нього вузол
	// нічого не пише — конвеєр лишається читачем, а не редактором диска.
	exported := ""
	if out := strings.TrimSpace(os.Getenv("CHUNKS_OUT")); out != "" {
		if err := writeChunks(out, b.Chunks); err != nil {
			return "", fmt.Errorf("export chunks: %w", err)
		}
		exported = fmt.Sprintf("\nChunks exported: %s (%d)", out, len(b.Chunks))
	}

	return exported + fmt.Sprintf(
		"Parents: %d, Children: %d (%d text, %d table, %d list)\n"+
			"Child sizes (tokens): %s\n"+
			"Tables: %d/%d preserved\n"+
			"Entities: %d, graph nodes added: %d, vectors: %d",
		parents, children,
		byKind[string(mdblocks.KindText)], byKind[string(mdblocks.KindTable)], byKind[string(mdblocks.KindList)],
		sizes.String(),
		preserved, len(docTables),
		len(b.Entities), b.NodesAdded, b.VectorsAdded,
	), nil
}

// writeChunks зберігає чанки у JSON — формат, який читає ДЗ 6.
//
// Запис через тимчасовий файл і Rename: інакше перерваний прогін лишає
// обрізаний JSON, який ДЗ 6 прочитає як «корпус із трьох чанків» і мовчки
// дасть неповну відповідь.
func writeChunks(path string, chunks []Chunk) error {
	data, err := json.MarshalIndent(chunks, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
