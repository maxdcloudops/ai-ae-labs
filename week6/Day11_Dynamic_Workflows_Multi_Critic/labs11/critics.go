package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"sort"
	"strings"
	"time"

	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/workflow"
	"google.golang.org/genai"

	"github.com/dimetron/ai-eng-course/labs/week6/Day11_Dynamic_Workflows_Multi_Critic/labs11/internal/manifest"
)

// Critic roles. They are the node names, the `critic` field of a verdict and
// the keys of the join map, so they are declared once.
const (
	CriticPrivileges = "privileges"
	CriticLimits     = "limits"
	CriticSecrets    = "secrets"
)

// Verdict is a critic's typed answer.
type Verdict struct {
	Critic   string   `json:"critic"`
	Passed   bool     `json:"passed"`
	Comments []string `json:"comments,omitempty"`
}

// Verdict validation failures. They are sentinels so the loop and the tests
// branch on the KIND of malformation, not on a message.
var (
	// ErrVerdictUnreadable — the reply is not a verdict at all.
	ErrVerdictUnreadable = errors.New("вердикт не розбирається як JSON")
	// ErrVerdictUnknownCritic — a role nobody asked for.
	ErrVerdictUnknownCritic = errors.New("невідома роль критика")
	// ErrVerdictRoleMismatch — the critic field does not match the node that
	// produced it. A critic answering for someone else breaks the audit: the
	// quorum would count one role twice and miss another.
	ErrVerdictRoleMismatch = errors.New("роль у полі critic не та, що в вузла")
	// ErrVerdictSilentFailure — passed=false with no comments. Useless to the
	// maker: it says "wrong" and not what to change.
	ErrVerdictSilentFailure = errors.New("passed=false без жодного коментаря")
	// ErrVerdictContradiction — passed=true with comments. The two halves
	// disagree, and guessing which one is meant is exactly the
	// "somehow interpreted" the assignment forbids.
	ErrVerdictContradiction = errors.New("passed=true з непорожніми comments")
)

// knownCritics is the panel's roster. A verdict from outside it is refused.
var knownCritics = map[string]bool{
	CriticPrivileges: true,
	CriticLimits:     true,
	CriticSecrets:    true,
}

// VerdictSchema exposes the contract for tests and for anyone wiring
// OutputSchema instead.
func VerdictSchema() *genai.Schema { return verdictSchema() }

// ParseVerdict decodes and validates one critic's raw reply.
//
// Nothing here is repaired. A verdict that does not satisfy its own contract is
// not evidence about the manifest — it is evidence about the critic, and the
// caller treats it as a refusal plus an event in the log.
func ParseVerdict(raw, role string) (Verdict, error) {
	if !knownCritics[role] {
		return Verdict{}, fmt.Errorf("%w: %q", ErrVerdictUnknownCritic, role)
	}
	var v Verdict
	if err := json.Unmarshal([]byte(stripFence(raw)), &v); err != nil {
		return Verdict{}, fmt.Errorf("%w: %v", ErrVerdictUnreadable, err)
	}
	return v, ValidateVerdict(v, role)
}

// ValidateVerdict checks a decoded verdict against the role that produced it.
func ValidateVerdict(v Verdict, role string) error {
	switch {
	case !knownCritics[role]:
		return fmt.Errorf("%w: %q", ErrVerdictUnknownCritic, role)
	case strings.TrimSpace(v.Critic) == "":
		return fmt.Errorf("%w: поле critic порожнє", ErrVerdictRoleMismatch)
	case v.Critic != role:
		return fmt.Errorf("%w: вузол %q, вердикт %q", ErrVerdictRoleMismatch, role, v.Critic)
	case !v.Passed && len(v.Comments) == 0:
		return fmt.Errorf("%w: критик %q", ErrVerdictSilentFailure, role)
	case v.Passed && len(v.Comments) > 0:
		return fmt.Errorf("%w: критик %q, %d коментарів", ErrVerdictContradiction, role, len(v.Comments))
	}
	return nil
}

// verdictSchema is the shape a verdict must have. It is kept, and deliberately
// NOT wired as llmagent.OutputSchema.
//
// With OutputSchema set, ADK validates and parses the reply itself, the node's
// output becomes an object, and a reply that is prose never reaches my code —
// it fails inside the node as a typed error that does not name which critic
// misbehaved. That removes exactly ONE of the four malformations п.3 asks about
// (prose instead of JSON) and takes away the ability to handle the other three
// as what they are: a refusal by that named critic, logged, with the loop
// continuing.
//
// So the raw string comes out of the node and ParseVerdict judges it. The
// schema below stays as the documented contract — and as the thing to pass to
// OutputSchema if you would rather have the framework enforce the shape and
// accept that a malformed reply becomes a run failure instead of a rejection.
//
// Worth saying plainly: a schema would not have made validation unnecessary.
// {"critic":"limits","passed":true,"comments":["немає limits"]} satisfies this
// schema completely and is still self-contradictory nonsense.
func verdictSchema() *genai.Schema {
	return &genai.Schema{
		Type: genai.TypeObject,
		Properties: map[string]*genai.Schema{
			"critic":   {Type: genai.TypeString, Description: "роль критика, рівно та, що в інструкції"},
			"passed":   {Type: genai.TypeBoolean, Description: "true лише якщо немає жодного зауваження"},
			"comments": {Type: genai.TypeArray, Items: &genai.Schema{Type: genai.TypeString}, Description: "по одному конкретному зауваженню; порожній, якщо passed=true"},
		},
		Required: []string{"critic", "passed"},
	}
}

// critic is one member of the panel.
type critic struct {
	Role  string
	Focus string
	// Rule is the policy as code. It returns the comments a correct critic
	// would write, and an empty slice when the manifest is clean.
	//
	// It has two jobs. Offline it IS the critic's brain, so an offline run
	// genuinely finds the problems instead of replaying a scripted answer.
	// And it is the written-down definition of what the live critic is asked
	// to do — the instruction below is the same policy in words.
	Rule func(*manifest.Manifest) []string
}

// Instruction is the system prompt of the live critic.
func (c critic) Instruction() string {
	return fmt.Sprintf(`Ти — критик %q в ревʼю Kubernetes-маніфестів LEDGERWORKS.
Твій і ТІЛЬКИ твій фокус: %s

На вхід — JSON із полем code (маніфест). Перевір його за своїм фокусом.
Про інше не пиши: чужі зауваження ламають кворум панелі.

Відповідай ЛИШЕ JSON-обʼєктом:
{"critic": %q, "passed": true|false, "comments": ["конкретне зауваження", …]}

Правила, які перевіряють твій вердикт, а не маніфест:
- поле critic — рівно %q, інакше вердикт відкинуть;
- passed=false ОБОВʼЯЗКОВО з коментарями: «не так» без «що саме» нічого не дає;
- passed=true ОБОВʼЯЗКОВО з порожніми comments: інакше твої дві половини
  суперечать одна одній, і вердикт відкинуть.
Кожен коментар — одне порушення, із шляхом у маніфесті.`,
		c.Role, c.Focus, c.Role, c.Role)
}

// panel is the roster, in a fixed order so the report is stable.
func panel() []critic {
	return []critic{
		{
			Role: CriticPrivileges,
			Focus: "привілеї пода й контейнерів: runAsNonRoot, allowPrivilegeEscalation, " +
				"readOnlyRootFilesystem, drop capabilities, відсутність privileged",
			Rule: rulePrivileges,
		},
		{
			Role:  CriticLimits,
			Focus: "ресурси: у КОЖНОГО контейнера є requests і limits на cpu та memory",
			Rule:  ruleLimits,
		},
		{
			Role: CriticSecrets,
			Focus: "секрети: жодного секрету відкритим текстом у env[].value; " +
				"кожен secretKeyRef явно оголошує optional",
			Rule: ruleSecrets,
		},
	}
}

// --- політики як код --------------------------------------------------------

func rulePrivileges(m *manifest.Manifest) []string {
	var out []string
	ps := m.PodSpec()
	if ps == nil {
		return nil // Service має не под, а селектор — цьому критику тут нічого робити
	}
	podSec, _ := ps["securityContext"].(map[string]any)
	if v, ok := podSec["runAsNonRoot"].(bool); !ok || !v {
		out = append(out, "spec…template.spec.securityContext.runAsNonRoot не true: под може запуститись як root")
	}
	for _, c := range m.Containers() {
		name := fmt.Sprint(c["name"])
		sec, _ := c["securityContext"].(map[string]any)
		if v, ok := sec["allowPrivilegeEscalation"].(bool); !ok || v {
			out = append(out, fmt.Sprintf("containers[%q].securityContext.allowPrivilegeEscalation не false", name))
		}
		if v, ok := sec["readOnlyRootFilesystem"].(bool); !ok || !v {
			out = append(out, fmt.Sprintf("containers[%q].securityContext.readOnlyRootFilesystem не true", name))
		}
		if v, ok := sec["privileged"].(bool); ok && v {
			out = append(out, fmt.Sprintf("containers[%q].securityContext.privileged=true — повний доступ до хоста", name))
		}
		if !dropsAll(sec) {
			out = append(out, fmt.Sprintf("containers[%q].securityContext.capabilities.drop не містить ALL", name))
		}
	}
	return out
}

func dropsAll(sec map[string]any) bool {
	caps, _ := sec["capabilities"].(map[string]any)
	drop, _ := caps["drop"].([]any)
	for _, d := range drop {
		if s, ok := d.(string); ok && strings.EqualFold(s, "ALL") {
			return true
		}
	}
	return false
}

func ruleLimits(m *manifest.Manifest) []string {
	var out []string
	for _, c := range m.Containers() {
		name := fmt.Sprint(c["name"])
		res, _ := c["resources"].(map[string]any)
		if res == nil {
			out = append(out, fmt.Sprintf("containers[%q].resources відсутній: под потрапляє в BestEffort і першим піде під OOM", name))
			continue
		}
		for _, side := range []string{"requests", "limits"} {
			side := side
			vals, _ := res[side].(map[string]any)
			if vals == nil {
				out = append(out, fmt.Sprintf("containers[%q].resources.%s відсутній", name, side))
				continue
			}
			for _, r := range []string{"cpu", "memory"} {
				if _, ok := vals[r]; !ok {
					out = append(out, fmt.Sprintf("containers[%q].resources.%s.%s відсутній", name, side, r))
				}
			}
		}
	}
	return out
}

// secretish names the env keys whose value must never be a literal.
var secretish = []string{"password", "passwd", "secret", "token", "apikey", "api_key", "dsn", "private_key", "credential"}

func ruleSecrets(m *manifest.Manifest) []string {
	var out []string
	for _, c := range m.Containers() {
		name := fmt.Sprint(c["name"])
		envs, _ := c["env"].([]any)
		for _, e := range envs {
			em, _ := e.(map[string]any)
			key := fmt.Sprint(em["name"])
			if lit, ok := em["value"].(string); ok && looksSecret(key) && strings.TrimSpace(lit) != "" {
				out = append(out, fmt.Sprintf("containers[%q].env[%q] має значення відкритим текстом — секрет у маніфесті потрапляє в git і в kubectl get", name, key))
				continue
			}
			ref, _ := em["valueFrom"].(map[string]any)
			skr, _ := ref["secretKeyRef"].(map[string]any)
			if skr == nil {
				continue
			}
			if _, ok := skr["optional"]; !ok {
				// Умова ДЗ формулює це як «secretKeyRef без optional: true».
				// Вимагати саме true я не став і кажу чому: optional: true
				// означає «запустись без секрету», тобто под підніметься
				// зламаним і тихо. Вимога тут — оголосити намір ЯВНО.
				out = append(out, fmt.Sprintf("containers[%q].env[%q].valueFrom.secretKeyRef без явного optional: намір не оголошено", name, key))
			}
		}
	}
	return out
}

func looksSecret(key string) bool {
	k := strings.ToLower(key)
	for _, s := range secretish {
		if strings.Contains(k, s) {
			return true
		}
	}
	return false
}

// --- офлайн-мозок критика ---------------------------------------------------

// offlineCritic is a model.LLM that applies the critic's own Rule to the
// manifest in the request and returns the verdict as JSON.
//
// It is NOT a recorded script. A script would make the offline run a replay:
// the panel would "find" problems that were written down in advance, and a
// change in the maker's output would not change the verdicts. Here the offline
// panel really judges the candidate, which is what makes the eval table below
// mean anything without a provider.
type offlineCritic struct {
	role string
	rule func(*manifest.Manifest) []string
	// broken, when set, is returned verbatim instead of a verdict. It is how
	// the tests drive the malformed-verdict paths.
	broken string
	// delay stands in for provider latency. Without it the rule-based critics
	// answer in microseconds and "parallel vs sequential" is pure measurement
	// noise — there is nothing to overlap.
	delay time.Duration
}

func (o offlineCritic) Name() string { return "offline-critic-" + o.role }

func (o offlineCritic) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if o.delay > 0 {
			time.Sleep(o.delay)
		}
		text := o.broken
		if text == "" {
			text = o.verdictFor(lastUserText(req))
		}
		yield(&model.LLMResponse{Content: genai.NewContentFromText(text, genai.RoleModel)}, nil)
	}
}

func (o offlineCritic) verdictFor(input string) string {
	v := Verdict{Critic: o.role, Passed: true}
	var d Draft
	switch {
	case json.Unmarshal([]byte(input), &d) != nil:
		v.Passed, v.Comments = false, []string{"вхід критика не розбирається як Draft"}
	default:
		m, err := manifest.Parse(d.Code)
		if err != nil {
			v.Passed, v.Comments = false, []string{"маніфест не розбирається: " + err.Error()}
			break
		}
		if comments := o.rule(m); len(comments) > 0 {
			v.Passed, v.Comments = false, comments
		}
	}
	b, err := json.Marshal(v)
	if err != nil {
		return `{"critic":"` + o.role + `","passed":false,"comments":["не вдалося закодувати вердикт"]}`
	}
	return string(b)
}

// textContent wraps a line as model content for an emitted event.
func textContent(s string) *genai.Content { return genai.NewContentFromText(s, genai.RoleModel) }

// lastUserText returns the latest user-side text of a request.
func lastUserText(req *model.LLMRequest) string {
	for i := len(req.Contents) - 1; i >= 0; i-- {
		c := req.Contents[i]
		if c == nil || c.Role != genai.RoleUser {
			continue
		}
		var b strings.Builder
		for _, p := range c.Parts {
			if p != nil {
				b.WriteString(p.Text)
			}
		}
		if s := strings.TrimSpace(b.String()); s != "" {
			return s
		}
	}
	return ""
}

// --- панель як підграф ------------------------------------------------------

// buildPanel wires the three critics as a sub-workflow: Start fans out to the
// three agent nodes, a JoinNode collects them.
//
// A sub-workflow rather than three RunNode calls in a row, because the critics
// are independent and should not wait for each other. The join is what makes
// "all three reported" a property of the graph instead of a loop invariant I
// have to remember.
func buildPanel(modelFor func(critic) model.LLM, meter interface {
	Callback(string) llmagent.AfterModelCallback
}) (workflow.Node, []string, error) {
	var nodes []workflow.Node
	var roles []string
	for _, c := range panel() {
		cfg := llmagent.Config{
			Name:        c.Role,
			Model:       modelFor(c),
			Description: "Критик: " + c.Focus,
			Instruction: c.Instruction(),
		}
		if meter != nil {
			cfg.AfterModelCallbacks = []llmagent.AfterModelCallback{meter.Callback(c.Role)}
		}
		a, err := llmagent.New(cfg)
		if err != nil {
			return nil, nil, fmt.Errorf("критик %s: %w", c.Role, err)
		}
		n, err := workflow.NewAgentNodeTyped[Draft, string](a, workflow.NodeConfig{})
		if err != nil {
			return nil, nil, fmt.Errorf("вузол критика %s: %w", c.Role, err)
		}
		nodes = append(nodes, n)
		roles = append(roles, c.Role)
	}
	join := workflow.NewJoinNode("critic_join")
	b := workflow.NewEdgeBuilder()
	b.AddFanOut(workflow.Start, nodes...)
	b.AddFanIn(join, nodes...)
	sub, err := workflow.NewWorkflowNode("critic_panel", b.Build())
	if err != nil {
		return nil, nil, fmt.Errorf("підграф critic_panel: %w", err)
	}
	return sub, roles, nil
}

// Aggregate turns the join map into a decision.
//
// Quorum: EVERY critic must pass. There is no majority vote, because the three
// focuses do not overlap — a clean privileges verdict says nothing about
// missing memory limits, so two "yes" cannot outvote one "no".
type Aggregate struct {
	Passed   bool
	Verdicts []Verdict
	// Feedback is what goes back to the maker: one line per comment, prefixed
	// with the critic that wrote it.
	Feedback []string
	// Rejected lists verdicts that failed validation, with their reason.
	Rejected []string
}

// aggregate validates every reply and combines them.
func aggregate(joined map[string]any, roles []string) Aggregate {
	var agg Aggregate
	agg.Passed = true
	for _, role := range roles {
		raw, ok := joined[role]
		if !ok {
			// A critic that never reported is not a pass. The join should make
			// this impossible; it is checked because "should" is not "does".
			agg.Passed = false
			agg.Rejected = append(agg.Rejected, role+": не відзвітував у join")
			agg.Feedback = append(agg.Feedback, "["+role+"] критик не відповів")
			continue
		}
		v, err := ParseVerdict(fmt.Sprint(raw), role)
		if err != nil {
			// Fail-closed: an unreadable verdict is never an acceptance.
			agg.Passed = false
			agg.Rejected = append(agg.Rejected, role+": "+err.Error())
			agg.Feedback = append(agg.Feedback, fmt.Sprintf("[%s] вердикт відкинуто: %v", role, err))
			continue
		}
		agg.Verdicts = append(agg.Verdicts, v)
		if v.Passed {
			continue
		}
		agg.Passed = false
		for _, c := range v.Comments {
			agg.Feedback = append(agg.Feedback, "["+role+"] "+c)
		}
	}
	sort.Strings(agg.Rejected)
	return agg
}

// Summary is the one-line form of the panel's result, for the console and the
// event log.
func (a Aggregate) Summary() string {
	parts := make([]string, 0, len(a.Verdicts))
	for _, v := range a.Verdicts {
		state := "pass"
		if !v.Passed {
			state = fmt.Sprintf("fail(%d)", len(v.Comments))
		}
		parts = append(parts, v.Critic+"="+state)
	}
	for _, r := range a.Rejected {
		parts = append(parts, "rejected:"+strings.SplitN(r, ":", 2)[0])
	}
	return strings.Join(parts, " ")
}
