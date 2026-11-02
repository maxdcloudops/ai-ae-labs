// Package manifest is the objective critic of the Kubernetes track: it decides
// whether a candidate manifest is valid, with no model and no network.
//
// It exists because the obvious tool does not do this job. `kubectl apply
// --dry-run=client`, even with `--validate=strict`, accepts a Deployment with
// `replicas: "two"` and an invented field — client-side validation was removed
// from kubectl, and strict validation needs a live API server for its OpenAPI
// schema. Verified against kubectl v1.34.1, 11/2026. kubeconform would do it,
// but a lab that only passes when an extra binary is installed is a lab that
// fails on a reviewer's machine (AGENTS.md §5.3: no network, no extra keys).
//
// So the rules below are hand-written, and deliberately only the ones that are
// genuinely objective: an API server would reject the manifest, or the workload
// provably cannot run. Anything arguable — whether a limit is generous, whether
// a probe is tuned well — belongs to an LLM critic, not here.
package manifest

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Manifest is a decoded Kubernetes object, kept as a generic tree.
//
// Untyped on purpose: typing it against k8s.io/api would pull a large
// dependency and, worse, would silently drop the unknown fields this validator
// has to complain about.
type Manifest struct {
	Raw        map[string]any
	APIVersion string
	Kind       string
	Name       string
}

// knownKinds maps a kind to the apiVersion it must carry. A kind outside this
// table is refused rather than guessed: the three tasks of this lab use three
// kinds, and "some other kind, probably fine" is not a validation result.
var knownKinds = map[string]string{
	"Deployment": "apps/v1",
	"CronJob":    "batch/v1",
	"Service":    "v1",
}

// dns1123 is the name format the API server enforces.
var dns1123 = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// Parse decodes and schema-checks a manifest. The returned error is written for
// the maker to read: it names the path and what was expected there.
func Parse(src string) (*Manifest, error) {
	src = strings.TrimSpace(src)
	if src == "" {
		return nil, fmt.Errorf("порожній маніфест")
	}
	// KnownFields makes yaml refuse a duplicate key, which is the one YAML
	// mistake that silently changes meaning instead of failing.
	dec := yaml.NewDecoder(strings.NewReader(src))
	dec.KnownFields(true)
	var raw map[string]any
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("YAML не розбирається: %w", err)
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("маніфест порожній після розбору")
	}

	m := &Manifest{Raw: raw}
	var err error
	if m.APIVersion, err = reqString(raw, "apiVersion"); err != nil {
		return nil, err
	}
	if m.Kind, err = reqString(raw, "kind"); err != nil {
		return nil, err
	}
	want, ok := knownKinds[m.Kind]
	if !ok {
		return nil, fmt.Errorf("kind %q не з тих, що перевіряє ця лаба (%s)", m.Kind, kindList())
	}
	if m.APIVersion != want {
		return nil, fmt.Errorf("apiVersion для %s має бути %q, а не %q", m.Kind, want, m.APIVersion)
	}
	if m.Name, err = reqString(raw, "metadata", "name"); err != nil {
		return nil, err
	}
	if !dns1123.MatchString(m.Name) {
		return nil, fmt.Errorf("metadata.name %q не є DNS-1123: лише [a-z0-9-], починається й кінчається літерою або цифрою", m.Name)
	}
	return m, nil
}

// Schema runs the kind-independent structural rules plus the per-kind ones that
// an API server would enforce.
func (m *Manifest) Schema() error {
	switch m.Kind {
	case "Deployment":
		return m.schemaDeployment()
	case "CronJob":
		return m.schemaCronJob()
	case "Service":
		return m.schemaService()
	}
	return fmt.Errorf("немає схеми для kind %q", m.Kind)
}

func (m *Manifest) schemaDeployment() error {
	if err := intField(m.Raw, 1, "spec", "replicas"); err != nil {
		return err
	}
	sel, err := labelMap(m.Raw, "spec", "selector", "matchLabels")
	if err != nil {
		return err
	}
	if len(sel) == 0 {
		return fmt.Errorf("spec.selector.matchLabels порожній: Deployment без селектора API-сервер відхиляє")
	}
	lbl, err := labelMap(m.Raw, "spec", "template", "metadata", "labels")
	if err != nil {
		return err
	}
	// The rule people actually get wrong, and the API server really does
	// reject: the selector must match the pod template's labels.
	for k, v := range sel {
		if lbl[k] != v {
			return fmt.Errorf("spec.selector.matchLabels[%q]=%q не збігається з spec.template.metadata.labels (%v) — API-сервер відхилить Deployment",
				k, v, lbl)
		}
	}
	return m.checkContainers("spec", "template", "spec")
}

func (m *Manifest) schemaCronJob() error {
	sched, err := reqString(m.Raw, "spec", "schedule")
	if err != nil {
		return err
	}
	if n := len(strings.Fields(sched)); n != 5 {
		return fmt.Errorf("spec.schedule %q має %d полів, а cron-вираз має 5", sched, n)
	}
	policy, err := reqString(m.Raw, "spec", "jobTemplate", "spec", "template", "spec", "restartPolicy")
	if err != nil {
		return err
	}
	if policy != "OnFailure" && policy != "Never" {
		return fmt.Errorf("restartPolicy %q неприйнятний для Job: лише OnFailure або Never", policy)
	}
	return m.checkContainers("spec", "jobTemplate", "spec", "template", "spec")
}

func (m *Manifest) schemaService() error {
	sel, err := labelMap(m.Raw, "spec", "selector")
	if err != nil {
		return err
	}
	if len(sel) == 0 {
		return fmt.Errorf("spec.selector порожній: Service без селектора нікуди не маршрутизує")
	}
	ports, ok := get(m.Raw, "spec", "ports").([]any)
	if !ok || len(ports) == 0 {
		return fmt.Errorf("spec.ports має бути непорожнім списком")
	}
	for i, p := range ports {
		pm, ok := p.(map[string]any)
		if !ok {
			return fmt.Errorf("spec.ports[%d] не є обʼєктом", i)
		}
		if err := intField(pm, 1, "port"); err != nil {
			return fmt.Errorf("spec.ports[%d]: %w", i, err)
		}
		if _, ok := pm["targetPort"]; !ok {
			return fmt.Errorf("spec.ports[%d].targetPort відсутній: без нього Service бʼє у той самий номер порту, що й сам слухає", i)
		}
	}
	return nil
}

// checkContainers validates the container list at podPath.
func (m *Manifest) checkContainers(podPath ...string) error {
	raw := get(m.Raw, append(podPath, "containers")...)
	list, ok := raw.([]any)
	if !ok || len(list) == 0 {
		return fmt.Errorf("%s.containers має бути непорожнім списком", strings.Join(podPath, "."))
	}
	seen := map[string]bool{}
	for i, c := range list {
		cm, ok := c.(map[string]any)
		if !ok {
			return fmt.Errorf("containers[%d] не є обʼєктом", i)
		}
		name, err := reqString(cm, "name")
		if err != nil {
			return fmt.Errorf("containers[%d]: %w", i, err)
		}
		if seen[name] {
			return fmt.Errorf("containers: імʼя %q повторюється — API-сервер відхилить", name)
		}
		seen[name] = true
		image, err := reqString(cm, "image")
		if err != nil {
			return fmt.Errorf("containers[%q]: %w", name, err)
		}
		if err := checkImage(name, image); err != nil {
			return err
		}
	}
	return nil
}

// checkImage requires an explicit, non-floating tag.
//
// This is objective rather than stylistic: :latest (or no tag at all) means two
// pods of the same ReplicaSet can run different code, and a rollback has
// nothing to roll back to.
func checkImage(container, image string) error {
	ref := image
	if i := strings.LastIndex(ref, "/"); i >= 0 {
		ref = ref[i+1:] // drop registry/path so a port in the host is not a tag
	}
	tag := ""
	if i := strings.LastIndex(ref, ":"); i >= 0 {
		tag = ref[i+1:]
	}
	switch {
	case tag == "" && !strings.Contains(ref, "@sha256:"):
		return fmt.Errorf("containers[%q].image %q без тегу: рівносильно :latest", container, image)
	case tag == "latest":
		return fmt.Errorf("containers[%q].image %q із тегом latest: два поди одного ReplicaSet можуть бути різним кодом", container, image)
	}
	return nil
}

// Check is one named rule. Visible checks are the specification the maker is
// shown; holdout checks are run only by the eval harness.
type Check struct {
	// Name appears in the report and in the event log.
	Name string
	// Why is shown to the maker as part of the specification.
	Why string
	// Fn returns nil when the manifest satisfies the rule.
	Fn func(*Manifest) error
}

// Report is the outcome of grading one candidate.
type Report struct {
	OK bool
	// Stage is the first stage that failed: "parse", "schema" or a check name.
	Stage string
	// Output is the failure message, written for the maker to act on.
	Output string
	// Failed lists every failed check name (schema failures stop before checks).
	Failed  []string
	Elapsed time.Duration
}

// Grade parses, schema-checks and then runs checks, in that order.
//
// The order is the point: a manifest that does not parse cannot be judged on
// its security posture, and reporting five "missing field" complaints about a
// file with a YAML syntax error buries the one that matters. Same reason the Go
// track runs the compiler before the critics.
func Grade(src string, checks []Check) Report {
	start := time.Now()
	rep := grade(src, checks)
	rep.Elapsed = time.Since(start)
	return rep
}

func grade(src string, checks []Check) Report {
	m, err := Parse(src)
	if err != nil {
		return Report{Stage: "parse", Output: err.Error(), Failed: []string{"parse"}}
	}
	if err := m.Schema(); err != nil {
		return Report{Stage: "schema", Output: err.Error(), Failed: []string{"schema"}}
	}
	var failed []string
	var msgs []string
	for _, c := range checks {
		if err := c.Fn(m); err != nil {
			failed = append(failed, c.Name)
			msgs = append(msgs, fmt.Sprintf("[%s] %v", c.Name, err))
		}
	}
	if len(failed) > 0 {
		// Every failed check is reported, not just the first: unlike a parse
		// error, these are independent, and the maker can fix them in one pass.
		return Report{Stage: failed[0], Output: strings.Join(msgs, "\n"), Failed: failed}
	}
	return Report{OK: true}
}

// Spec renders the checks as the specification text the maker sees. It is the
// manifest-track equivalent of handing the candidate the test file.
func Spec(checks []Check) string {
	var b strings.Builder
	b.WriteString("Обовʼязкові правила (їх перевіряє валідатор):\n")
	for _, c := range checks {
		fmt.Fprintf(&b, "- %s: %s\n", c.Name, c.Why)
	}
	return b.String()
}

// --- доступ до дерева -------------------------------------------------------

// get walks the tree and returns nil for any missing or wrongly-typed step.
func get(root map[string]any, path ...string) any {
	var cur any = root
	for _, step := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur, ok = m[step]
		if !ok {
			return nil
		}
	}
	return cur
}

// reqString requires a non-empty string at path.
func reqString(root map[string]any, path ...string) (string, error) {
	v := get(root, path...)
	if v == nil {
		return "", fmt.Errorf("%s відсутній", strings.Join(path, "."))
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("%s має бути рядком, а не %T", strings.Join(path, "."), v)
	}
	if strings.TrimSpace(s) == "" {
		return "", fmt.Errorf("%s порожній", strings.Join(path, "."))
	}
	return s, nil
}

// intField requires an integer of at least min at path. YAML decodes 2 as int
// and "2" as string, which is exactly the mistake this catches.
func intField(root map[string]any, min int, path ...string) error {
	v := get(root, path...)
	if v == nil {
		return fmt.Errorf("%s відсутній", strings.Join(path, "."))
	}
	n, ok := v.(int)
	if !ok {
		return fmt.Errorf("%s має бути цілим числом, а не %T (%v)", strings.Join(path, "."), v, v)
	}
	if n < min {
		return fmt.Errorf("%s = %d, має бути не менше %d", strings.Join(path, "."), n, min)
	}
	return nil
}

// labelMap requires a map of string to string at path.
func labelMap(root map[string]any, path ...string) (map[string]string, error) {
	v := get(root, path...)
	if v == nil {
		return nil, fmt.Errorf("%s відсутній", strings.Join(path, "."))
	}
	raw, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s має бути обʼєктом, а не %T", strings.Join(path, "."), v)
	}
	out := make(map[string]string, len(raw))
	for k, val := range raw {
		s, ok := val.(string)
		if !ok {
			return nil, fmt.Errorf("%s[%q] має бути рядком, а не %T", strings.Join(path, "."), k, val)
		}
		out[k] = s
	}
	return out, nil
}

func kindList() string {
	out := make([]string, 0, len(knownKinds))
	for k := range knownKinds {
		out = append(out, k)
	}
	// Sorted so the error message is stable between runs.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return strings.Join(out, ", ")
}

// Containers returns every container object of the manifest, whatever the kind.
// Critics use it; it never fails, because Schema has already run.
func (m *Manifest) Containers() []map[string]any {
	paths := [][]string{
		{"spec", "template", "spec", "containers"},
		{"spec", "jobTemplate", "spec", "template", "spec", "containers"},
	}
	for _, p := range paths {
		if list, ok := get(m.Raw, p...).([]any); ok {
			out := make([]map[string]any, 0, len(list))
			for _, c := range list {
				if cm, ok := c.(map[string]any); ok {
					out = append(out, cm)
				}
			}
			return out
		}
	}
	return nil
}

// PodSpec returns the pod spec of a workload manifest, or nil for a Service.
func (m *Manifest) PodSpec() map[string]any {
	for _, p := range [][]string{
		{"spec", "template", "spec"},
		{"spec", "jobTemplate", "spec", "template", "spec"},
	} {
		if ps, ok := get(m.Raw, p...).(map[string]any); ok {
			return ps
		}
	}
	return nil
}

// Get exposes the tree walker to callers in this lab (the checks and critics).
func (m *Manifest) Get(path ...string) any { return get(m.Raw, path...) }
