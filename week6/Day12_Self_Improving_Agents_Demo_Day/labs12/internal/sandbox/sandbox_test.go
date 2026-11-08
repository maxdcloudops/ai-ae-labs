package sandbox

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func newTest(t *testing.T) *Sandbox {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, StrategyFile), []byte("# стратегія\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ExperimentsDir), 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := New(root, StrategyFile, ExperimentsDir)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestIsMutableOnlyForTheDeclaredSurface — головна перевірка чек-листа:
// IsMutable == true РІВНО для strategy.md і для шляхів усередині experiments/,
// і false для всього іншого, включно з трьома способами втечі.
func TestIsMutableOnlyForTheDeclaredSurface(t *testing.T) {
	t.Parallel()
	s := newTest(t)

	allowed := []string{
		StrategyFile,
		"./" + StrategyFile,
		s.StrategyPath(),
		ExperimentsDir + "/cand-1.md",
		ExperimentsDir + "/nested/cand-2.md",
	}
	for _, p := range allowed {
		if !s.IsMutable(p) {
			_, err := s.Resolve(p)
			t.Errorf("IsMutable(%q) = false, want true: %v", p, err)
		}
	}

	denied := []string{
		"",                      // порожній шлях
		"notes.md",              // інший файл у корені
		ExperimentsDir,          // сам каталог — не файл для запису
		"experiments-evil/x.md", // префікс, що лише ПОЧИНАЄТЬСЯ як дозволений
		"../outside.md",         // traversal
		"../../etc/hosts",       // traversal глибше
		"sub/../../outside.md",  // traversal після згортання
		"/etc/hosts",            // абсолютний шлях назовні
		"/tmp",                  // абсолютний каталог назовні
	}
	for _, p := range denied {
		if s.IsMutable(p) {
			t.Errorf("IsMutable(%q) = true, want false — поверхня змін протікає", p)
		}
	}
}

// TestResolveNamesTheReasonWithASentinel — відмова мусить бути
// розпізнаваною через errors.Is і називати, що саме дозволено: інакше агент
// не знає, що виправити.
func TestResolveNamesTheReasonWithASentinel(t *testing.T) {
	t.Parallel()
	s := newTest(t)
	_, err := s.Resolve("../outside.md")
	if !errors.Is(err, ErrOutsideSurface) {
		t.Fatalf("err = %v, want errors.Is(ErrOutsideSurface)", err)
	}
	for _, want := range []string{StrategyFile, ExperimentsDir} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("повідомлення не називає дозволене %q: %v", want, err)
		}
	}
}

// TestSymlinkInPlaceOfStrategyIsRefused — симлінк на місці дозволеного файлу
// перетворює «дозволений запис» на запис у будь-що. Перевірка Lstat стоїть
// саме для цього.
func TestSymlinkInPlaceOfStrategyIsRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("символьні посилання на Windows потребують інших прав")
	}
	t.Parallel()
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "evil.md")
	if err := os.WriteFile(outside, []byte("чуже"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, StrategyFile)); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ExperimentsDir), 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := New(root, StrategyFile, ExperimentsDir)
	if err != nil {
		t.Fatal(err)
	}
	if s.IsMutable(StrategyFile) {
		t.Error("симлінк на місці strategy.md визнано мутабельним")
	}
	if err := s.WriteStrategy([]byte("нове")); !errors.Is(err, ErrOutsideSurface) {
		t.Errorf("запис через симлінк: err = %v, want ErrOutsideSurface", err)
	}
	// І головне: чужий файл не змінився.
	got, err := os.ReadFile(outside)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "чуже" {
		t.Errorf("запис пройшов крізь симлінк: %q", got)
	}
}

// TestSymlinkedDirectoryIsRefused — другий вектор: не сам файл, а каталог на
// шляху до нього. `link/strategy.md`, де link → назовні, має базове імʼя
// дозволеного файлу й не має права пройти.
func TestSymlinkedDirectoryIsRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("символьні посилання на Windows потребують інших прав")
	}
	t.Parallel()
	s := newTest(t)
	outsideDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(outsideDir, StrategyFile), []byte("чуже"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideDir, filepath.Join(s.Root(), "link")); err != nil {
		t.Fatal(err)
	}
	if s.IsMutable("link/" + StrategyFile) {
		t.Error("шлях через симлінк-каталог визнано мутабельним")
	}
}

// TestDeclarationItselfIsValidated — якщо саму поверхню оголосити з `..` або
// абсолютним шляхом, усі подальші перевірки були б театром.
func TestDeclarationItselfIsValidated(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	bad := []struct{ name, file, dir string }{
		{"файл з ..", "../strategy.md", ExperimentsDir},
		{"абсолютний файл", "/etc/hosts", ExperimentsDir},
		{"порожній файл", "", ExperimentsDir},
		{"файл — це крапка", ".", ExperimentsDir},
		{"каталог з ..", StrategyFile, "../experiments"},
		{"файл і каталог однакові", StrategyFile, StrategyFile},
	}
	for _, tt := range bad {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := New(root, tt.file, tt.dir); err == nil {
				t.Errorf("New(%q, %q) прийнято, хоча оголошення виводить за межі", tt.file, tt.dir)
			}
		})
	}
	if _, err := New("", StrategyFile, ExperimentsDir); err == nil {
		t.Error("порожній root прийнято")
	}
	if _, err := New(filepath.Join(root, "nope"), StrategyFile, ExperimentsDir); err == nil {
		t.Error("неіснуючий root прийнято")
	}
	f := filepath.Join(root, "file")
	if err := os.WriteFile(f, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := New(f, StrategyFile, ExperimentsDir); err == nil {
		t.Error("файл як root прийнято")
	}
}

// TestWriteIsAtomicAndRoundTrips — запис доходить, читання віддає те саме, і
// тимчасових файлів після себе не лишається.
func TestWriteIsAtomicAndRoundTrips(t *testing.T) {
	t.Parallel()
	s := newTest(t)
	const content = "# нова стратегія\nцитуй кожну цифру\n"
	if err := s.WriteStrategy([]byte(content)); err != nil {
		t.Fatal(err)
	}
	got, err := s.ReadStrategy()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != content {
		t.Errorf("прочитано %q, want %q", got, content)
	}
	// Каталог experiments/ створюється під запис автоматично.
	if err := s.Write(ExperimentsDir+"/deep/cand.md", []byte("дискард")); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(s.Root())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("лишився тимчасовий файл %q", e.Name())
		}
	}
}

// TestWriteRefusesOutsideAndReadReportsMissing — шляхи помилок: запис поза
// поверхнею не доходить до диска, а читання відсутнього файлу повертає
// помилку, а не порожній рядок (інакше «стратегія зникла» виглядало б як
// «стратегія порожня»).
func TestWriteRefusesOutsideAndReadReportsMissing(t *testing.T) {
	t.Parallel()
	s := newTest(t)
	if err := s.Write("../outside.md", []byte("x")); !errors.Is(err, ErrOutsideSurface) {
		t.Errorf("err = %v, want ErrOutsideSurface", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(s.Root()), "outside.md")); err == nil {
		t.Error("файл поза поверхнею все одно створено")
	}

	empty := t.TempDir()
	if err := os.MkdirAll(filepath.Join(empty, ExperimentsDir), 0o755); err != nil {
		t.Fatal(err)
	}
	s2, err := New(empty, StrategyFile, ExperimentsDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s2.ReadStrategy(); err == nil {
		t.Error("читання відсутньої стратегії не дало помилки")
	}
	// Шляхи-аксесори лишаються коректними навіть коли файлу ще немає.
	if s2.StrategyRel() != StrategyFile {
		t.Errorf("StrategyRel = %q", s2.StrategyRel())
	}
	if filepath.Base(s2.ExperimentsPath()) != ExperimentsDir {
		t.Errorf("ExperimentsPath = %q", s2.ExperimentsPath())
	}
}
