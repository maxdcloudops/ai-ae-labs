package manifest

import (
	"strings"
	"testing"
)

// TestReferencePassesEverything — еталон мусить проходити і видимі, і
// приховані правила. Якщо ні, холдаут міряє мою помилку, а не агента.
func TestReferencePassesEverything(t *testing.T) {
	t.Parallel()
	for _, task := range All() {
		t.Run(task.ID, func(t *testing.T) {
			t.Parallel()
			if rep := Grade(task.Reference, task.Visible); !rep.OK {
				t.Fatalf("еталон не проходить видимі: stage=%s\n%s", rep.Stage, rep.Output)
			}
			if rep := Grade(task.Reference, task.Holdout); !rep.OK {
				t.Fatalf("еталон не проходить holdout: stage=%s\n%s", rep.Stage, rep.Output)
			}
		})
	}
}

// TestBuggyPassesVisibleAndFailsHoldout — саме ця асиметрія робить
// «false accept» вимірюваним. Buggy, який падає на видимих, нічого не
// доводить: цикл його просто не прийняв би.
func TestBuggyPassesVisibleAndFailsHoldout(t *testing.T) {
	t.Parallel()
	for _, task := range All() {
		t.Run(task.ID, func(t *testing.T) {
			t.Parallel()
			if rep := Grade(task.Buggy, task.Visible); !rep.OK {
				t.Fatalf("Buggy падає на видимих правилах — цикл його не прийме, і холдаут нічого не виявить: stage=%s\n%s",
					rep.Stage, rep.Output)
			}
			rep := Grade(task.Buggy, task.Holdout)
			if rep.OK {
				t.Fatalf("Buggy проходить holdout — тоді це не хибне прийняття, а правильний результат")
			}
			if len(rep.Failed) == 0 {
				t.Error("холдаут упав, але не назвав жодного правила")
			}
		})
	}
}

// TestParseRejectsWhatKubectlAccepts — те, через що цей валідатор узагалі
// існує: kubectl --dry-run=client (навіть --validate=strict) приймає обидва
// ці маніфести.
func TestParseRejectsWhatKubectlAccepts(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, src, want string
	}{
		{
			name: "replicas рядком замість числа",
			src: `apiVersion: apps/v1
kind: Deployment
metadata: {name: pay-api}
spec:
  replicas: "two"
  selector: {matchLabels: {app: pay-api}}
  template:
    metadata: {labels: {app: pay-api}}
    spec: {containers: [{name: api, image: pay-api:1.0.0}]}
`,
			want: "має бути цілим числом",
		},
		{
			name: "селектор не збігається з мітками пода",
			src: `apiVersion: apps/v1
kind: Deployment
metadata: {name: pay-api}
spec:
  replicas: 2
  selector: {matchLabels: {app: pay-api}}
  template:
    metadata: {labels: {app: pay-apii}}
    spec: {containers: [{name: api, image: pay-api:1.0.0}]}
`,
			want: "не збігається",
		},
		{
			name: "образ без тегу",
			src: `apiVersion: apps/v1
kind: Deployment
metadata: {name: pay-api}
spec:
  replicas: 2
  selector: {matchLabels: {app: pay-api}}
  template:
    metadata: {labels: {app: pay-api}}
    spec: {containers: [{name: api, image: registry.example/pay-api}]}
`,
			want: "без тегу",
		},
		{
			name: "дубльований ключ YAML",
			src: `apiVersion: apps/v1
kind: Deployment
metadata: {name: pay-api}
spec:
  replicas: 2
  replicas: 3
`,
			want: "YAML не розбирається",
		},
		{
			name: "невідомий kind",
			src:  "apiVersion: v1\nkind: Flurb\nmetadata: {name: x}\n",
			want: "не з тих, що перевіряє ця лаба",
		},
		{
			name: "неправильний apiVersion для kind",
			src:  "apiVersion: v1\nkind: Deployment\nmetadata: {name: x}\n",
			want: "apiVersion для Deployment",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rep := Grade(tt.src, nil)
			if rep.OK {
				t.Fatal("валідатор прийняв маніфест, який мусив відхилити")
			}
			if !strings.Contains(rep.Output, tt.want) {
				t.Errorf("повідомлення = %q, want містити %q", rep.Output, tt.want)
			}
		})
	}
}

// TestGradeStopsAtParseBeforeChecks — порядок: спершу розбір, потім схема,
// потім правила. П'ять скарг «поля немає» над файлом із синтаксичною
// помилкою ховають ту єдину, що має значення.
func TestGradeStopsAtParseBeforeChecks(t *testing.T) {
	t.Parallel()
	called := false
	checks := []Check{{Name: "never", Why: "—", Fn: func(*Manifest) error {
		called = true
		return nil
	}}}
	rep := Grade("не: [YAML", checks)
	if rep.Stage != "parse" {
		t.Errorf("stage = %q, want parse", rep.Stage)
	}
	if called {
		t.Error("правила виконались над маніфестом, який не розібрався")
	}
}

// TestGradeReportsEveryFailedCheck — правила незалежні, тому звіт містить усі,
// а не перше: maker може виправити їх за один прохід.
func TestGradeReportsEveryFailedCheck(t *testing.T) {
	t.Parallel()
	task, _ := ByID("pay_api_deployment")
	bad := strings.Replace(task.Reference, "replicas: 3", "replicas: 1", 1)
	bad = strings.Replace(bad, "      maxUnavailable: 0\n", "", 1)
	rep := Grade(bad, task.Visible)
	if rep.OK {
		t.Fatal("маніфест із двома порушеннями прийнято")
	}
	if len(rep.Failed) != 2 {
		t.Errorf("порушень названо %d (%v), want 2", len(rep.Failed), rep.Failed)
	}
}

// TestSpecNamesEveryVisibleRule — maker бачить специфікацію; правило, якого в
// ній немає, — це пастка, а не вимога.
func TestSpecNamesEveryVisibleRule(t *testing.T) {
	t.Parallel()
	for _, task := range All() {
		spec := task.Spec()
		for _, c := range task.Visible {
			if !strings.Contains(spec, c.Name) {
				t.Errorf("%s: специфікація не згадує правило %q", task.ID, c.Name)
			}
		}
		for _, c := range task.Holdout {
			if strings.Contains(spec, c.Name) {
				t.Errorf("%s: приховане правило %q потрапило в специфікацію", task.ID, c.Name)
			}
		}
	}
}

func TestByIDAndIDs(t *testing.T) {
	t.Parallel()
	if _, ok := ByID("nope"); ok {
		t.Error("ByID знайшов неіснуюче завдання")
	}
	ids := IDs()
	if len(ids) != 3 {
		t.Fatalf("завдань %d, want 3", len(ids))
	}
	for _, id := range ids {
		if _, ok := ByID(id); !ok {
			t.Errorf("ByID(%q) не знайшов завдання зі свого ж списку", id)
		}
	}
}

// TestSchemaErrorPaths — кожна гілка схеми має свою названу помилку. Таблиця
// рядків, а не один «невалідний маніфест»: саме текст помилки читає maker, і
// «щось не так» не дає йому нічого виправити.
func TestSchemaErrorPaths(t *testing.T) {
	t.Parallel()
	tests := []struct{ name, src, want string }{
		{"порожній вхід", "   ", "порожній"},
		{"не обʼєкт", "- a\n- b", "не розбирається"},
		{"немає apiVersion", "kind: Service\nmetadata: {name: x}\n", "apiVersion відсутній"},
		{"kind не рядок", "apiVersion: v1\nkind: 7\nmetadata: {name: x}\n", "має бути рядком"},
		{"імʼя не DNS-1123", "apiVersion: v1\nkind: Service\nmetadata: {name: Pay_API}\n", "DNS-1123"},
		{"metadata.name порожній", "apiVersion: v1\nkind: Service\nmetadata: {name: \"\"}\n", "порожній"},

		{"CronJob: розклад із 4 полів",
			"apiVersion: batch/v1\nkind: CronJob\nmetadata: {name: j}\nspec: {schedule: \"1 2 3 4\"}\n",
			"має 4 полів"},
		{"CronJob: немає restartPolicy",
			"apiVersion: batch/v1\nkind: CronJob\nmetadata: {name: j}\nspec: {schedule: \"1 2 3 4 5\"}\n",
			"restartPolicy відсутній"},
		{"CronJob: restartPolicy Always",
			`apiVersion: batch/v1
kind: CronJob
metadata: {name: j}
spec:
  schedule: "1 2 3 4 5"
  jobTemplate: {spec: {template: {spec: {restartPolicy: Always}}}}
`, "неприйнятний для Job"},
		{"CronJob: немає контейнерів",
			`apiVersion: batch/v1
kind: CronJob
metadata: {name: j}
spec:
  schedule: "1 2 3 4 5"
  jobTemplate: {spec: {template: {spec: {restartPolicy: OnFailure}}}}
`, "containers має бути непорожнім"},

		{"Service: порожній селектор",
			"apiVersion: v1\nkind: Service\nmetadata: {name: s}\nspec: {selector: {}, ports: [{port: 80, targetPort: http}]}\n",
			"selector порожній"},
		{"Service: немає портів",
			"apiVersion: v1\nkind: Service\nmetadata: {name: s}\nspec: {selector: {app: a}}\n",
			"ports має бути непорожнім"},
		{"Service: немає targetPort",
			"apiVersion: v1\nkind: Service\nmetadata: {name: s}\nspec: {selector: {app: a}, ports: [{port: 80}]}\n",
			"targetPort відсутній"},
		{"Service: порт не число",
			"apiVersion: v1\nkind: Service\nmetadata: {name: s}\nspec: {selector: {app: a}, ports: [{port: \"80\", targetPort: http}]}\n",
			"має бути цілим числом"},
		{"Service: селектор не обʼєкт",
			"apiVersion: v1\nkind: Service\nmetadata: {name: s}\nspec: {selector: 7, ports: [{port: 80, targetPort: http}]}\n",
			"має бути обʼєктом"},
		{"Service: мітка не рядок",
			"apiVersion: v1\nkind: Service\nmetadata: {name: s}\nspec: {selector: {app: 7}, ports: [{port: 80, targetPort: http}]}\n",
			"має бути рядком"},

		{"Deployment: образ latest",
			`apiVersion: apps/v1
kind: Deployment
metadata: {name: d}
spec:
  replicas: 2
  selector: {matchLabels: {app: a}}
  template:
    metadata: {labels: {app: a}}
    spec: {containers: [{name: c, image: reg.example/x:latest}]}
`, "тегом latest"},
		{"Deployment: дубль імені контейнера",
			`apiVersion: apps/v1
kind: Deployment
metadata: {name: d}
spec:
  replicas: 2
  selector: {matchLabels: {app: a}}
  template:
    metadata: {labels: {app: a}}
    spec: {containers: [{name: c, image: x:1}, {name: c, image: y:1}]}
`, "повторюється"},
		{"Deployment: контейнер без image",
			`apiVersion: apps/v1
kind: Deployment
metadata: {name: d}
spec:
  replicas: 2
  selector: {matchLabels: {app: a}}
  template:
    metadata: {labels: {app: a}}
    spec: {containers: [{name: c}]}
`, "image відсутній"},
		{"Deployment: порожній селектор",
			`apiVersion: apps/v1
kind: Deployment
metadata: {name: d}
spec:
  replicas: 2
  selector: {matchLabels: {}}
  template:
    metadata: {labels: {app: a}}
    spec: {containers: [{name: c, image: x:1}]}
`, "matchLabels порожній"},
		{"Deployment: replicas менше 1",
			`apiVersion: apps/v1
kind: Deployment
metadata: {name: d}
spec:
  replicas: 0
  selector: {matchLabels: {app: a}}
  template:
    metadata: {labels: {app: a}}
    spec: {containers: [{name: c, image: x:1}]}
`, "має бути не менше 1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rep := Grade(tt.src, nil)
			if rep.OK {
				t.Fatal("маніфест прийнято, хоча він невалідний")
			}
			if !strings.Contains(rep.Output, tt.want) {
				t.Errorf("повідомлення = %q, want містити %q", rep.Output, tt.want)
			}
		})
	}
}

// TestImageWithDigestIsAccepted — пін за digest це явніше, ніж тег, і має
// проходити: правило проти :latest не повинно забороняти найкращий варіант.
func TestImageWithDigestIsAccepted(t *testing.T) {
	t.Parallel()
	if err := checkImage("c", "reg.example/x@sha256:"+strings.Repeat("a", 64)); err != nil {
		t.Errorf("образ, пінений за digest, відхилено: %v", err)
	}
	if err := checkImage("c", "localhost:5000/x:1.2.3"); err != nil {
		t.Errorf("порт у реєстрі прийнято за тег: %v", err)
	}
}

// TestPodSpecAndContainers — доступ до пода працює для обох форм (Deployment і
// CronJob), і повертає nil для Service, у якого пода немає.
func TestPodSpecAndContainers(t *testing.T) {
	t.Parallel()
	for _, task := range All() {
		m, err := Parse(task.Reference)
		if err != nil {
			t.Fatalf("%s: %v", task.ID, err)
		}
		switch task.ID {
		case "pay_api_service":
			if m.PodSpec() != nil {
				t.Error("Service не має пода, а PodSpec щось повернув")
			}
			if len(m.Containers()) != 0 {
				t.Error("Service не має контейнерів")
			}
		default:
			if m.PodSpec() == nil {
				t.Errorf("%s: PodSpec = nil", task.ID)
			}
			if len(m.Containers()) == 0 {
				t.Errorf("%s: контейнерів не знайдено", task.ID)
			}
		}
	}
}

// TestInsecurePassesValidatorAndFailsCritics — Insecure мусить проходити
// валідатор (інакше цикл зупиниться на гейті й критики не отримають слова) і
// мати що показати критикам.
func TestInsecurePassesTheValidator(t *testing.T) {
	t.Parallel()
	for _, task := range All() {
		if task.Insecure == "" {
			continue
		}
		if rep := Grade(task.Insecure, task.Visible); !rep.OK {
			t.Errorf("%s: Insecure падає на видимих правилах (stage=%s) — критики не дійдуть до слова:\n%s",
				task.ID, rep.Stage, rep.Output)
		}
	}
}
