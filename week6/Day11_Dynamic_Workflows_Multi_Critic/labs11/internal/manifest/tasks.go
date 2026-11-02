package manifest

import (
	"fmt"
	"strings"
)

// Task is one manifest-writing job, shaped like billing.Task of the Go track so
// the eval harness is the same.
//
// Visible is the specification: its rules go into the prompt and run inside the
// loop. Holdout runs ONLY after the loop has stopped. The pair is what makes
// "false accept" measurable — a candidate that satisfies everything it was
// shown and still fails.
type Task struct {
	ID     string
	Prompt string
	// Visible rules are shown to the maker and run every iteration.
	Visible []Check
	// Holdout rules are hidden and run once, by the eval harness.
	Holdout []Check
	// Reference satisfies Visible and Holdout.
	Reference string
	// Buggy satisfies Visible and fails Holdout. It is how the offline demo
	// produces a false accept on purpose.
	Buggy string
	// Flaw names, in one sentence, what is wrong with Buggy.
	Flaw string
	// Insecure passes the validator and FAILS the critic panel: no pod
	// securityContext, no resources, a secret in plain text. It is what
	// exercises the feedback path — the critics' comments going back into the
	// maker — which neither Reference nor Buggy ever does, because both are
	// security-clean.
	//
	// Empty for a Service: it has no pod, so privileges, limits and secrets
	// critics have nothing to look at. That is itself worth knowing — a critic
	// outside its domain always passes, and a panel of three can quietly
	// become a panel of none.
	Insecure string
}

// Spec is the specification text handed to the maker.
func (t Task) Spec() string { return Spec(t.Visible) }

// ByID returns a task by its id.
func ByID(id string) (Task, bool) {
	for _, t := range All() {
		if t.ID == id {
			return t, true
		}
	}
	return Task{}, false
}

// IDs lists the task ids, in eval order.
func IDs() []string {
	out := make([]string, 0, 3)
	for _, t := range All() {
		out = append(out, t.ID)
	}
	return out
}

// All returns the three tasks of the Kubernetes track.
func All() []Task {
	return []Task{payAPIDeployment(), ledgerCronJob(), payAPIService()}
}

// --- спільні перевірки ------------------------------------------------------

// hasPath builds a check that requires a value at path.
func hasPath(name, why string, path ...string) Check {
	return Check{Name: name, Why: why, Fn: func(m *Manifest) error {
		if m.Get(path...) == nil {
			return fmt.Errorf("%s відсутній", strings.Join(path, "."))
		}
		return nil
	}}
}

// intAtLeast builds a check that requires an integer of at least min.
func intAtLeast(name, why string, min int, path ...string) Check {
	return Check{Name: name, Why: why, Fn: func(m *Manifest) error {
		return intField(m.Raw, min, path...)
	}}
}

// stringEquals builds a check that requires an exact string at path.
func stringEquals(name, why, want string, path ...string) Check {
	return Check{Name: name, Why: why, Fn: func(m *Manifest) error {
		got, err := reqString(m.Raw, path...)
		if err != nil {
			return err
		}
		if got != want {
			return fmt.Errorf("%s = %q, має бути %q", strings.Join(path, "."), got, want)
		}
		return nil
	}}
}

// --- 1. Deployment платіжного API -------------------------------------------

func payAPIDeployment() Task {
	visible := []Check{
		intAtLeast("replicas", "spec.replicas не менше 2 — один под означає простій на кожному деплої", 2, "spec", "replicas"),
		hasPath("readiness_probe", "у контейнера є readinessProbe — без неї Service шле трафік у под, який ще не готовий",
			"spec", "template", "spec", "containers"),
		hasPath("max_unavailable", "spec.strategy.rollingUpdate.maxUnavailable заданий явно — дефолт 25% на 2 подах це 1, тобто половина потужності",
			"spec", "strategy", "rollingUpdate", "maxUnavailable"),
	}
	// readiness_probe перевіряє вміст контейнера, а не лише наявність списку.
	visible[1].Fn = func(m *Manifest) error {
		for _, c := range m.Containers() {
			if _, ok := c["readinessProbe"]; !ok {
				return fmt.Errorf("containers[%v] без readinessProbe", c["name"])
			}
		}
		return nil
	}

	holdout := []Check{
		intAtLeast("grace_period",
			"spec.template.spec.terminationGracePeriodSeconds не менше 30 — платіжний запит має дограти до кінця",
			30, "spec", "template", "spec", "terminationGracePeriodSeconds"),
		{
			Name: "liveness_after_readiness",
			Why:  "livenessProbe.initialDelaySeconds більший за readinessProbe.initialDelaySeconds",
			Fn: func(m *Manifest) error {
				for _, c := range m.Containers() {
					live, ok := c["livenessProbe"].(map[string]any)
					if !ok {
						continue // liveness не обовʼязкова; якщо є — має бути пізнішою
					}
					ready, ok := c["readinessProbe"].(map[string]any)
					if !ok {
						continue
					}
					l, lok := live["initialDelaySeconds"].(int)
					r, rok := ready["initialDelaySeconds"].(int)
					if !lok || !rok {
						return fmt.Errorf("containers[%v]: initialDelaySeconds обох проб має бути цілим числом", c["name"])
					}
					if l <= r {
						return fmt.Errorf("containers[%v]: livenessProbe.initialDelaySeconds=%d <= readinessProbe=%d — под уб'ють раніше, ніж він встигне стати готовим (crash loop)",
							c["name"], l, r)
					}
				}
				return nil
			},
		},
	}

	return Task{
		ID: "pay_api_deployment",
		Prompt: "Напиши Kubernetes-маніфест Deployment для платіжного API LEDGERWORKS.\n" +
			"kind: Deployment, apiVersion: apps/v1, metadata.name: pay-api.\n" +
			"Один контейнер `api`, образ `registry.ledgerworks.example/pay-api:1.4.2`, порт 8080 з імʼям `http`.\n" +
			"Мітки пода: app=pay-api, tier=edge (і селектор має їм відповідати).\n" + Spec(visible),
		Reference: `apiVersion: apps/v1
kind: Deployment
metadata:
  name: pay-api
spec:
  replicas: 3
  strategy:
    type: RollingUpdate
    rollingUpdate:
      maxUnavailable: 0
      maxSurge: 1
  selector:
    matchLabels:
      app: pay-api
      tier: edge
  template:
    metadata:
      labels:
        app: pay-api
        tier: edge
    spec:
      terminationGracePeriodSeconds: 45
      securityContext:
        runAsNonRoot: true
        runAsUser: 10001
      containers:
        - name: api
          image: registry.ledgerworks.example/pay-api:1.4.2
          ports:
            - name: http
              containerPort: 8080
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities:
              drop: ["ALL"]
          resources:
            requests:
              cpu: 100m
              memory: 128Mi
            limits:
              cpu: 500m
              memory: 256Mi
          readinessProbe:
            httpGet:
              path: /readyz
              port: http
            initialDelaySeconds: 5
            periodSeconds: 5
          livenessProbe:
            httpGet:
              path: /healthz
              port: http
            initialDelaySeconds: 30
            periodSeconds: 10
          env:
            - name: LEDGER_DSN
              valueFrom:
                secretKeyRef:
                  name: pay-api-db
                  key: dsn
                  optional: false
`,
		Buggy: `apiVersion: apps/v1
kind: Deployment
metadata:
  name: pay-api
spec:
  replicas: 3
  strategy:
    type: RollingUpdate
    rollingUpdate:
      maxUnavailable: 0
      maxSurge: 1
  selector:
    matchLabels:
      app: pay-api
      tier: edge
  template:
    metadata:
      labels:
        app: pay-api
        tier: edge
    spec:
      terminationGracePeriodSeconds: 5
      securityContext:
        runAsNonRoot: true
        runAsUser: 10001
      containers:
        - name: api
          image: registry.ledgerworks.example/pay-api:1.4.2
          ports:
            - name: http
              containerPort: 8080
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities:
              drop: ["ALL"]
          resources:
            requests:
              cpu: 100m
              memory: 128Mi
            limits:
              cpu: 500m
              memory: 256Mi
          readinessProbe:
            httpGet:
              path: /readyz
              port: http
            initialDelaySeconds: 5
            periodSeconds: 5
          livenessProbe:
            httpGet:
              path: /healthz
              port: http
            initialDelaySeconds: 3
            periodSeconds: 10
          env:
            - name: LEDGER_DSN
              valueFrom:
                secretKeyRef:
                  name: pay-api-db
                  key: dsn
                  optional: false
`,
		Flaw: "terminationGracePeriodSeconds=5 обриває платіжні запити на деплої, а livenessProbe стартує раніше readiness — под уб'ють до готовності",
		Insecure: `apiVersion: apps/v1
kind: Deployment
metadata:
  name: pay-api
spec:
  replicas: 3
  strategy:
    type: RollingUpdate
    rollingUpdate:
      maxUnavailable: 0
      maxSurge: 1
  selector:
    matchLabels:
      app: pay-api
      tier: edge
  template:
    metadata:
      labels:
        app: pay-api
        tier: edge
    spec:
      terminationGracePeriodSeconds: 45
      containers:
        - name: api
          image: registry.ledgerworks.example/pay-api:1.4.2
          ports:
            - name: http
              containerPort: 8080
          readinessProbe:
            httpGet:
              path: /readyz
              port: http
            initialDelaySeconds: 5
          livenessProbe:
            httpGet:
              path: /healthz
              port: http
            initialDelaySeconds: 30
          env:
            - name: LEDGER_PASSWORD
              value: "hunter2"
`,
		Visible: visible, Holdout: holdout,
	}
}

// --- 2. CronJob нічної звірки -----------------------------------------------

func ledgerCronJob() Task {
	visible := []Check{
		intAtLeast("backoff_limit", "spec.jobTemplate.spec.backoffLimit заданий — дефолт 6 означає шість нічних звірок підряд",
			0, "spec", "jobTemplate", "spec", "backoffLimit"),
		intAtLeast("active_deadline", "spec.jobTemplate.spec.activeDeadlineSeconds заданий — звірка, що висить до ранку, блокує наступну",
			60, "spec", "jobTemplate", "spec", "activeDeadlineSeconds"),
		hasPath("starting_deadline", "spec.startingDeadlineSeconds заданий — інакше пропущений запуск наздоганяє в найгірший момент",
			"spec", "startingDeadlineSeconds"),
	}
	holdout := []Check{
		stringEquals("concurrency_forbid",
			"spec.concurrencyPolicy = Forbid — дві звірки одночасно проведуть одні й ті самі транзакції двічі",
			"Forbid", "spec", "concurrencyPolicy"),
		{
			Name: "history_limits",
			Why:  "обидва ліміти історії задані явно",
			Fn: func(m *Manifest) error {
				for _, f := range []string{"successfulJobsHistoryLimit", "failedJobsHistoryLimit"} {
					if m.Get("spec", f) == nil {
						return fmt.Errorf("spec.%s відсутній — історія Job-ів росте без межі", f)
					}
				}
				return nil
			},
		},
	}
	return Task{
		ID: "ledger_cronjob",
		Prompt: "Напиши Kubernetes-маніфест CronJob нічної звірки реєстру LEDGERWORKS.\n" +
			"kind: CronJob, apiVersion: batch/v1, metadata.name: ledger-reconcile.\n" +
			"Розклад: щодня о 02:30. Один контейнер `reconcile`, образ " +
			"`registry.ledgerworks.example/reconcile:0.9.1`, restartPolicy OnFailure.\n" + Spec(visible),
		Reference: `apiVersion: batch/v1
kind: CronJob
metadata:
  name: ledger-reconcile
spec:
  schedule: "30 2 * * *"
  concurrencyPolicy: Forbid
  startingDeadlineSeconds: 600
  successfulJobsHistoryLimit: 3
  failedJobsHistoryLimit: 1
  jobTemplate:
    spec:
      backoffLimit: 2
      activeDeadlineSeconds: 3600
      template:
        spec:
          restartPolicy: OnFailure
          securityContext:
            runAsNonRoot: true
            runAsUser: 10001
          containers:
            - name: reconcile
              image: registry.ledgerworks.example/reconcile:0.9.1
              securityContext:
                allowPrivilegeEscalation: false
                readOnlyRootFilesystem: true
                capabilities:
                  drop: ["ALL"]
              resources:
                requests:
                  cpu: 200m
                  memory: 256Mi
                limits:
                  cpu: "1"
                  memory: 512Mi
              env:
                - name: LEDGER_DSN
                  valueFrom:
                    secretKeyRef:
                      name: ledger-db
                      key: dsn
                      optional: false
`,
		Buggy: `apiVersion: batch/v1
kind: CronJob
metadata:
  name: ledger-reconcile
spec:
  schedule: "30 2 * * *"
  concurrencyPolicy: Allow
  startingDeadlineSeconds: 600
  jobTemplate:
    spec:
      backoffLimit: 2
      activeDeadlineSeconds: 3600
      template:
        spec:
          restartPolicy: OnFailure
          securityContext:
            runAsNonRoot: true
            runAsUser: 10001
          containers:
            - name: reconcile
              image: registry.ledgerworks.example/reconcile:0.9.1
              securityContext:
                allowPrivilegeEscalation: false
                readOnlyRootFilesystem: true
                capabilities:
                  drop: ["ALL"]
              resources:
                requests:
                  cpu: 200m
                  memory: 256Mi
                limits:
                  cpu: "1"
                  memory: 512Mi
              env:
                - name: LEDGER_DSN
                  valueFrom:
                    secretKeyRef:
                      name: ledger-db
                      key: dsn
                      optional: false
`,
		Flaw: "concurrencyPolicy=Allow дозволяє двом звіркам іти одночасно й провести одні транзакції двічі; ліміти історії не задані",
		Insecure: `apiVersion: batch/v1
kind: CronJob
metadata:
  name: ledger-reconcile
spec:
  schedule: "30 2 * * *"
  concurrencyPolicy: Forbid
  startingDeadlineSeconds: 600
  successfulJobsHistoryLimit: 3
  failedJobsHistoryLimit: 1
  jobTemplate:
    spec:
      backoffLimit: 2
      activeDeadlineSeconds: 3600
      template:
        spec:
          restartPolicy: OnFailure
          containers:
            - name: reconcile
              image: registry.ledgerworks.example/reconcile:0.9.1
              env:
                - name: LEDGER_DSN
                  value: "postgres://ledger:hunter2@db/ledger"
`,
		Visible: visible, Holdout: holdout,
	}
}

// --- 3. Service платіжного API ----------------------------------------------

func payAPIService() Task {
	visible := []Check{
		stringEquals("type_clusterip", "spec.type = ClusterIP — платіжний API не публікується назовні сам",
			"ClusterIP", "spec", "type"),
		{
			Name: "named_ports",
			Why:  "кожен порт має імʼя (spec.ports[].name)",
			Fn: func(m *Manifest) error {
				ports, _ := m.Get("spec", "ports").([]any)
				for i, p := range ports {
					pm, _ := p.(map[string]any)
					if s, ok := pm["name"].(string); !ok || strings.TrimSpace(s) == "" {
						return fmt.Errorf("spec.ports[%d].name відсутній", i)
					}
				}
				return nil
			},
		},
		{
			Name: "selector_matches_deployment",
			Why:  "spec.selector = {app: pay-api, tier: edge} — рівно мітки пода з Deployment",
			Fn: func(m *Manifest) error {
				want := map[string]string{"app": "pay-api", "tier": "edge"}
				got, err := labelMap(m.Raw, "spec", "selector")
				if err != nil {
					return err
				}
				if len(got) != len(want) {
					return fmt.Errorf("spec.selector = %v, має бути %v", got, want)
				}
				for k, v := range want {
					if got[k] != v {
						return fmt.Errorf("spec.selector[%q] = %q, має бути %q", k, got[k], v)
					}
				}
				return nil
			},
		},
	}
	holdout := []Check{
		{
			Name: "no_publish_not_ready",
			Why:  "publishNotReadyAddresses не true",
			Fn: func(m *Manifest) error {
				if v, ok := m.Get("spec", "publishNotReadyAddresses").(bool); ok && v {
					return fmt.Errorf("spec.publishNotReadyAddresses=true шле трафік у неготові поди — readinessProbe стає декорацією")
				}
				return nil
			},
		},
		{
			Name: "target_port_named",
			Why:  "targetPort — імʼя порту, не число",
			Fn: func(m *Manifest) error {
				ports, _ := m.Get("spec", "ports").([]any)
				for i, p := range ports {
					pm, _ := p.(map[string]any)
					if _, isInt := pm["targetPort"].(int); isInt {
						return fmt.Errorf("spec.ports[%d].targetPort — число: зміна порту в контейнері тихо зламає маршрутизацію; використай імʼя порту", i)
					}
				}
				return nil
			},
		},
	}
	return Task{
		ID: "pay_api_service",
		Prompt: "Напиши Kubernetes-маніфест Service для платіжного API LEDGERWORKS.\n" +
			"kind: Service, apiVersion: v1, metadata.name: pay-api.\n" +
			"Один порт 443 назовні сервісу, що веде в контейнерний порт з імʼям `http`.\n" +
			"Поди фронтує Deployment pay-api з мітками app=pay-api, tier=edge.\n" + Spec(visible),
		Reference: `apiVersion: v1
kind: Service
metadata:
  name: pay-api
spec:
  type: ClusterIP
  selector:
    app: pay-api
    tier: edge
  ports:
    - name: https
      port: 443
      targetPort: http
      protocol: TCP
`,
		Buggy: `apiVersion: v1
kind: Service
metadata:
  name: pay-api
spec:
  type: ClusterIP
  selector:
    app: pay-api
    tier: edge
  ports:
    - name: https
      port: 443
      targetPort: 8080
      protocol: TCP
`,
		Flaw:    "targetPort задано числом 8080: зміна порту в контейнері тихо зламає маршрутизацію",
		Visible: visible, Holdout: holdout,
	}
}
