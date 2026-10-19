// Package helmtest renders the Helm chart in deploy/helm/tasklane with a small stand-in for Helm's template engine (the Go template
// functions the chart uses, no more) and checks what comes out: valid objects, the guards that refuse a bad install, and the settings
// the application reads. It is not a replacement for `helm lint` and `helm template`, which CI with Helm installed should also run.
package helmtest

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"text/template"

	"gopkg.in/yaml.v3"
)

const chartDir = "../../deploy/helm/tasklane"

func load(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := yaml.Unmarshal(b, &m); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return m
}

func merge(dst, src map[string]any) {
	for k, v := range src {
		if sm, ok := v.(map[string]any); ok {
			if dm, ok := dst[k].(map[string]any); ok {
				merge(dm, sm)
				continue
			}
		}
		dst[k] = v
	}
}

func toYAML(v any) string {
	b, err := yaml.Marshal(v)
	if err != nil {
		panic(err)
	}
	return strings.TrimSuffix(string(b), "\n")
}

func indent(n int, s string) string {
	p := strings.Repeat(" ", n)
	return p + strings.ReplaceAll(s, "\n", "\n"+p)
}

func asInt(v any) int {
	switch x := v.(type) {
	case int:
		return x
	case int64:
		return int(x)
	case float64:
		return int(x)
	case string:
		var n int
		fmt.Sscan(x, &n)
		return n
	}
	return 0
}

func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case string:
		return x != ""
	case bool:
		return x
	case int:
		return x != 0
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

// render returns the chart's objects by file name (a file may hold several documents; an empty file is left out).
func render(values map[string]any) (map[string][]map[string]any, error) {
	chart := load(t0, filepath.Join(chartDir, "Chart.yaml"))
	ctx := map[string]any{
		"Values":   values,
		"Chart":    map[string]any{"Name": chart["name"], "Version": chart["version"], "AppVersion": chart["appVersion"]},
		"Release":  map[string]any{"Name": "rel", "Namespace": "ns", "Service": "Helm"},
		"Template": map[string]any{"BasePath": "tasklane/templates"},
	}
	root := template.New("root")
	funcs := template.FuncMap{
		"toYaml":    toYAML,
		"nindent":   func(n int, s string) string { return "\n" + indent(n, s) },
		"indent":    indent,
		"quote":     func(v any) string { return fmt.Sprintf("%q", fmt.Sprint(v)) },
		"default":   func(d, v any) any { if truthy(v) { return v }; return d },
		"trunc":     func(n int, s string) string { if len(s) > n { return s[:n] }; return s },
		"trimSuffix": func(suf, s string) string { return strings.TrimSuffix(s, suf) },
		"replace":   func(o, n, s string) string { return strings.ReplaceAll(s, o, n) },
		"contains":  func(sub, s string) bool { return strings.Contains(s, sub) },
		"upper":     strings.ToUpper,
		"toString":  func(v any) string { return fmt.Sprint(v) },
		"int":       asInt,
		"sha256sum": func(s string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(s))) },
		"fail":      func(s string) (string, error) { return "", fmt.Errorf("%s", s) },
		"include": func(name string, data any) (string, error) {
			var b bytes.Buffer
			err := root.ExecuteTemplate(&b, name, data)
			return b.String(), err
		},
	}
	root.Funcs(funcs).Option("missingkey=zero")
	files, _ := filepath.Glob(filepath.Join(chartDir, "templates", "*.yaml"))
	more, _ := filepath.Glob(filepath.Join(chartDir, "templates", "tests", "*.yaml"))
	helpers := filepath.Join(chartDir, "templates", "_helpers.tpl")
	for _, f := range append(append([]string{helpers}, files...), more...) {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		name := "tasklane/templates/" + strings.TrimPrefix(filepath.ToSlash(f), filepath.ToSlash(filepath.Join(chartDir, "templates"))+"/")
		if _, err := root.New(name).Parse(string(b)); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
	}
	out := map[string][]map[string]any{}
	names := []string{}
	for _, f := range append(files, more...) {
		names = append(names, "tasklane/templates/"+strings.TrimPrefix(filepath.ToSlash(f), filepath.ToSlash(filepath.Join(chartDir, "templates"))+"/"))
	}
	sort.Strings(names)
	for _, n := range names {
		var b bytes.Buffer
		if err := root.ExecuteTemplate(&b, n, ctx); err != nil {
			return nil, err
		}
		if strings.TrimSpace(b.String()) == "" {
			continue
		}
		var m map[string]any
		if err := yaml.Unmarshal(b.Bytes(), &m); err != nil {
			return nil, fmt.Errorf("%s does not render to valid YAML: %w\n%s", n, err, b.String())
		}
		out[n] = append(out[n], m)
	}
	return out, nil
}

var t0 *testing.T // set by each test, for helpers that read the chart

func base(t *testing.T, over map[string]any) map[string]any {
	t0 = t
	v := load(t, filepath.Join(chartDir, "values.yaml"))
	merge(v, map[string]any{
		"config":   map[string]any{"base_url": "https://tasklane.example.com"},
		"database": map[string]any{"existingSecret": "pg"},
	})
	merge(v, over)
	return v
}

func kind(objs map[string][]map[string]any, k string) map[string]any {
	for _, l := range objs {
		for _, o := range l {
			if o["kind"] == k {
				return o
			}
		}
	}
	return nil
}

func dig(v any, path ...string) any {
	for _, p := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[p]
	}
	return v
}

func envOf(dep map[string]any) map[string]map[string]any {
	c := dig(dep, "spec", "template", "spec", "containers").([]any)[0].(map[string]any)
	out := map[string]map[string]any{}
	for _, e := range c["env"].([]any) {
		m := e.(map[string]any)
		out[m["name"].(string)] = m
	}
	return out
}

func TestINST_ChartDefaultsRenderValidObjects(t *testing.T) {
	objs, err := render(base(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"Deployment", "Service", "ConfigMap", "ServiceAccount", "PersistentVolumeClaim", "Pod"} {
		if kind(objs, k) == nil {
			t.Errorf("no %s rendered", k)
		}
	}
	for _, k := range []string{"Ingress", "PodDisruptionBudget", "NetworkPolicy", "Job"} {
		if kind(objs, k) != nil {
			t.Errorf("%s rendered by default", k)
		}
	}
	dep := kind(objs, "Deployment")
	pod := dig(dep, "spec", "template", "spec").(map[string]any)
	c := pod["containers"].([]any)[0].(map[string]any)
	if c["image"] != "ghcr.io/larytet/tasklane:0.1.0" {
		t.Errorf("image %v", c["image"])
	}
	sc := c["securityContext"].(map[string]any)
	if sc["readOnlyRootFilesystem"] != true || sc["allowPrivilegeEscalation"] != false {
		t.Errorf("container is not locked down: %v", sc)
	}
	if dig(pod, "securityContext", "runAsNonRoot") != true {
		t.Error("pod may run as root")
	}
	if dig(c, "readinessProbe", "httpGet", "path") != "/readyz" || dig(c, "livenessProbe", "httpGet", "path") != "/healthz" {
		t.Error("probes point at the wrong endpoints")
	}
	// the selector must match the pod labels, and the service the selector
	sel := dig(dep, "spec", "selector", "matchLabels").(map[string]any)
	lab := dig(dep, "spec", "template", "metadata", "labels").(map[string]any)
	for k, v := range sel {
		if lab[k] != v {
			t.Errorf("selector %s=%v not in pod labels", k, v)
		}
	}
	ssel := dig(kind(objs, "Service"), "spec", "selector").(map[string]any)
	for k, v := range sel {
		if ssel[k] != v {
			t.Errorf("service selector differs on %s", k)
		}
	}
	// every volume mounted is declared, every config key is one the application knows
	declared := map[string]bool{}
	for _, v := range pod["volumes"].([]any) {
		declared[v.(map[string]any)["name"].(string)] = true
	}
	for _, m := range c["volumeMounts"].([]any) {
		if n := m.(map[string]any)["name"].(string); !declared[n] {
			t.Errorf("volume %s is mounted but not declared", n)
		}
	}
	data := dig(kind(objs, "ConfigMap"), "data").(map[string]any)
	if data["TASKLANE_BASE_URL"] != "https://tasklane.example.com" || data["TASKLANE_BLOB_BACKEND"] != "fs" {
		t.Errorf("config: %v", data)
	}
	if p, _ := dig(kind(objs, "Pod"), "metadata", "annotations").(map[string]any); p["helm.sh/hook"] != "test" {
		t.Error("the test pod is not a helm test hook")
	}
}

func TestINST_ChartConfigKeysAreKeysTheApplicationDefines(t *testing.T) {
	v := base(t, map[string]any{
		"smtp":  map[string]any{"host": "smtp.example.com", "user": "u", "passwordSecret": map[string]any{"name": "smtp"}},
		"blobs": map[string]any{"backend": "s3", "s3": map[string]any{"bucket": "b", "endpoint": "https://s3.example.com", "existingSecret": "s3"}},
		"admin": map[string]any{"existingSecret": "adm"}, "secretKey": map[string]any{"existingSecret": "sk"},
	})
	objs, err := render(v)
	if err != nil {
		t.Fatal(err)
	}
	src, _ := filepath.Glob("../config/*.go")
	var all strings.Builder
	for _, f := range src {
		b, _ := os.ReadFile(f)
		all.Write(b)
	}
	dep := kind(objs, "Deployment")
	names := []string{}
	for k := range dig(kind(objs, "ConfigMap"), "data").(map[string]any) {
		names = append(names, k)
	}
	for k := range envOf(dep) {
		if strings.HasPrefix(k, "TASKLANE_") {
			names = append(names, k)
		}
	}
	for _, n := range names {
		key := strings.ToLower(strings.TrimPrefix(n, "TASKLANE_"))
		if !strings.Contains(all.String(), `"`+key+`"`) {
			t.Errorf("%s is set by the chart but the application defines no config key %q", n, key)
		}
	}
	if len(names) < 15 {
		t.Errorf("only %d settings rendered: %v", len(names), names)
	}
	// secrets reach the container from Secrets, never from the ConfigMap
	for _, n := range []string{"TASKLANE_ADMIN_PASSWORD", "TASKLANE_SECRET_KEY", "TASKLANE_SMTP_PASS", "TASKLANE_S3_SECRET_KEY", "TASKLANE_DATABASE_URL"} {
		if dig(envOf(dep)[n], "valueFrom", "secretKeyRef") == nil {
			t.Errorf("%s does not come from a Secret", n)
		}
	}
}

func TestINST_ChartRefusesBadInstalls(t *testing.T) {
	cases := []struct {
		name string
		over map[string]any
		want string
	}{
		{"no database", map[string]any{"database": map[string]any{"existingSecret": ""}}, "database.existingSecret"},
		{"host without a password secret", map[string]any{"database": map[string]any{"existingSecret": "", "host": "pg"}}, "passwordSecret"},
		{"s3 without a bucket", map[string]any{"blobs": map[string]any{"backend": "s3"}}, "blobs.s3.bucket"},
		{"two replicas on a volume only one node can mount", map[string]any{"replicaCount": 2}, "more than one replica"},
		{"two replicas without persistence", map[string]any{"replicaCount": 2, "blobs": map[string]any{"persistence": map[string]any{"enabled": false}}}, "more than one replica"},
		{"no address", map[string]any{"config": map[string]any{"base_url": ""}}, "base_url"},
		{"grace period shorter than the drain", map[string]any{"terminationGracePeriodSeconds": 5}, "terminationGracePeriodSeconds"},
	}
	for _, c := range cases {
		if _, err := render(base(t, c.over)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want an error naming %q, got %v", c.name, c.want, err)
		}
	}
	// and the allowed forms of the same things pass
	ok := []map[string]any{
		{"replicaCount": 3, "blobs": map[string]any{"backend": "s3", "s3": map[string]any{"bucket": "b"}}},
		{"replicaCount": 2, "blobs": map[string]any{"persistence": map[string]any{"accessMode": "ReadWriteMany"}}},
		{"config": map[string]any{"base_url": ""}, "ingress": map[string]any{"enabled": true, "tls": []any{map[string]any{"secretName": "x"}}}},
		{"database": map[string]any{"existingSecret": "", "host": "pg", "passwordSecret": map[string]any{"name": "pgpw"}}},
	}
	for i, o := range ok {
		if _, err := render(base(t, o)); err != nil {
			t.Errorf("allowed form %d refused: %v", i, err)
		}
	}
}

func TestINST_ChartOptionalPieces(t *testing.T) {
	objs, err := render(base(t, map[string]any{
		"ingress":             map[string]any{"enabled": true, "className": "nginx", "tls": []any{map[string]any{"secretName": "tls", "hosts": []any{"tasklane.example.com"}}}},
		"podDisruptionBudget": map[string]any{"enabled": true},
		"networkPolicy":       map[string]any{"enabled": true},
		"migrations":          map[string]any{"job": map[string]any{"enabled": true}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	ing := kind(objs, "Ingress")
	if ing == nil {
		t.Fatal("no ingress")
	}
	be := dig(ing, "spec", "rules").([]any)[0].(map[string]any)
	if dig(be, "http", "paths").([]any)[0].(map[string]any)["pathType"] != "Prefix" {
		t.Error("path type")
	}
	if kind(objs, "PodDisruptionBudget") == nil || kind(objs, "NetworkPolicy") == nil {
		t.Error("pdb or network policy missing")
	}
	job := kind(objs, "Job")
	if job == nil {
		t.Fatal("no migrate job")
	}
	if a := dig(job, "metadata", "annotations").(map[string]any); a["helm.sh/hook"] != "pre-install,pre-upgrade" {
		t.Errorf("hook: %v", a)
	}
	args := dig(job, "spec", "template", "spec", "containers").([]any)[0].(map[string]any)["args"].([]any)
	if args[0] != "migrate" {
		t.Errorf("job runs %v", args)
	}
	// the base address comes from the ingress when none is given
	objs, _ = render(base(t, map[string]any{"config": map[string]any{"base_url": ""}, "ingress": map[string]any{"enabled": true, "tls": []any{map[string]any{"secretName": "x"}}}}))
	if dig(kind(objs, "ConfigMap"), "data", "TASKLANE_BASE_URL") != "https://tasklane.example.com" {
		t.Errorf("base url from ingress: %v", dig(kind(objs, "ConfigMap"), "data"))
	}
}

func TestINST_ChartSchemaAndDocsAgree(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(chartDir, "values.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s map[string]any
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	props := dig(s, "properties").(map[string]any)
	v := load(t, filepath.Join(chartDir, "values.yaml"))
	for k := range props {
		if _, ok := v[k]; !ok {
			t.Errorf("the schema describes %q, which values.yaml does not have", k)
		}
	}
	// a drain delay the grace period cannot cover is the commonest way to drop requests on a rollout: the defaults must cover it
	if asInt(v["terminationGracePeriodSeconds"]) < 30 {
		t.Error("default grace period too short")
	}
	// every value the templates read exists in values.yaml
	re := regexp.MustCompile(`\.Values\.([A-Za-z]+(?:\.[A-Za-z0-9]+)*)`)
	files, _ := filepath.Glob(filepath.Join(chartDir, "templates", "*"))
	more, _ := filepath.Glob(filepath.Join(chartDir, "templates", "tests", "*"))
	for _, f := range append(files, more...) {
		src, _ := os.ReadFile(f)
		for _, m := range re.FindAllStringSubmatch(string(src), -1) {
			var cur any = v
			for _, p := range strings.Split(m[1], ".") {
				mm, ok := cur.(map[string]any)
				if !ok {
					break // below a leaf (a list entry or a free-form map)
				}
				if cur, ok = mm[p]; !ok {
					if m[1] == "config.base_url" || strings.HasPrefix(m[1], "config.") {
						break
					}
					t.Errorf("%s reads .Values.%s, which values.yaml does not define", filepath.Base(f), m[1])
					break
				}
			}
		}
	}
}
