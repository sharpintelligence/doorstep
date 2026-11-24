package kustomize_test

import (
	"bytes"
	"cmp"
	"errors"
	"io"
	"io/fs"
	"net"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/sharpintelligence/doorstep/internal/config"
)

const (
	doorstepImage = "ghcr.io/sharpintelligence/doorstep"
	digestPattern = `^sha256:[0-9a-f]{64}$`
)

type document map[string]any

func (d document) lookup(t *testing.T, keys ...any) any {
	t.Helper()
	var current any = map[string]any(d)
	for _, key := range keys {
		switch k := key.(type) {
		case string:
			m, ok := current.(map[string]any)
			if !ok {
				t.Fatalf("path %v: %v is not a mapping", keys, current)
			}
			current, ok = m[k]
			if !ok {
				t.Fatalf("path %v: missing key %q", keys, k)
			}
		case int:
			list, ok := current.([]any)
			if !ok || k >= len(list) {
				t.Fatalf("path %v: no element %d in %v", keys, k, current)
			}
			current = list[k]
		}
	}
	return current
}

func readDocuments(t *testing.T, name string) []document {
	t.Helper()
	data, err := os.ReadFile(filepath.Clean(name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var documents []document
	for {
		var doc map[string]any
		err := decoder.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return documents
		}
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		documents = append(documents, document(doc))
	}
}

func findDocument(t *testing.T, name, kind, objectName string) document {
	t.Helper()
	for _, doc := range readDocuments(t, name) {
		metadata, _ := doc["metadata"].(map[string]any)
		if doc["kind"] == kind && metadata["name"] == objectName {
			return doc
		}
	}
	t.Fatalf("%s has no %s named %s", name, kind, objectName)
	return nil
}

func baseDeployment(t *testing.T) document {
	t.Helper()
	return findDocument(t, "base/deployment.yaml", "Deployment", "doorstep")
}

func baseContainer(t *testing.T) map[string]any {
	t.Helper()
	return baseDeployment(t).lookup(t, "spec", "template", "spec", "containers", 0).(map[string]any)
}

func registryPatch(t *testing.T) document {
	t.Helper()
	return findDocument(t, "components/registry-credentials/deployment-patch.yaml", "Deployment", "doorstep")
}

func namesOf(t *testing.T, items any, key string) []string {
	t.Helper()
	list, _ := items.([]any)
	var names []string
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("list item %v is not a mapping", item)
		}
		names = append(names, m[key].(string))
	}
	return names
}

func TestDeploymentEnvironmentNamesAreKnownSettings(t *testing.T) {
	t.Parallel()

	known := []string{
		config.EnvConfig,
		config.EnvListenAddr,
		config.EnvMetricsListenAddr,
		config.EnvShutdownTimeout,
		config.EnvShutdownDelay,
		config.EnvLogLevel,
		config.EnvLogFormat,
		config.EnvDatabaseURL,
		config.EnvDatabaseURLFile,
		config.EnvRegistryCredentialsFile,
		config.EnvGitHubAppID,
		config.EnvGitHubAppPrivateKey,
		config.EnvGitHubAppPrivateKeyFile,
		config.EnvGitHubAPIURL,
		config.EnvGitHubWebhookSecret,
		config.EnvGitHubWebhookSecretFile,
		config.EnvPollInterval,
		config.EnvKubePodDiagnostics,
	}
	tests := []struct {
		name      string
		container func(t *testing.T) map[string]any
		required  []string
	}{
		{
			name:      "base",
			container: baseContainer,
			required:  []string{config.EnvConfig, config.EnvDatabaseURLFile},
		},
		{
			name: "registry credentials component",
			container: func(t *testing.T) map[string]any {
				t.Helper()
				return registryPatch(t).lookup(t, "spec", "template", "spec", "containers", 0).(map[string]any)
			},
			required: []string{config.EnvRegistryCredentialsFile},
		},
		{
			name: "pod diagnostics component",
			container: func(t *testing.T) map[string]any {
				t.Helper()
				return findDocument(t, "components/pod-diagnostics/deployment-patch.yaml", "Deployment", "doorstep").lookup(t, "spec", "template", "spec", "containers", 0).(map[string]any)
			},
			required: []string{config.EnvKubePodDiagnostics},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			names := namesOf(t, tt.container(t)["env"], "name")
			for _, name := range names {
				if strings.HasPrefix(name, "DOORSTEP_") && !slices.Contains(known, name) {
					t.Errorf("env %s is not a setting known to internal/config", name)
				}
			}
			for _, name := range tt.required {
				if !slices.Contains(names, name) {
					t.Errorf("env %s is not set", name)
				}
			}
		})
	}
}

func TestDeploymentPortsMatchDefaults(t *testing.T) {
	t.Parallel()

	defaults := config.Default().Server
	portOf := func(t *testing.T, address string) string {
		t.Helper()
		_, port, err := net.SplitHostPort(address)
		if err != nil {
			t.Fatalf("split %q: %v", address, err)
		}
		return port
	}
	service := findDocument(t, "base/service.yaml", "Service", "doorstep")
	servicePorts := service.lookup(t, "spec", "ports").([]any)
	tests := []struct {
		name    string
		address string
	}{
		{name: "http", address: defaults.ListenAddr},
		{name: "metrics", address: defaults.MetricsListenAddr},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			want := portOf(t, tt.address)
			var containerPort string
			for _, p := range baseContainer(t)["ports"].([]any) {
				m := p.(map[string]any)
				if m["name"] == tt.name {
					containerPort = intString(m["containerPort"])
				}
			}
			if containerPort != want {
				t.Errorf("containerPort %s = %q, want %q", tt.name, containerPort, want)
			}
			var found bool
			for _, p := range servicePorts {
				m := p.(map[string]any)
				if m["name"] != tt.name {
					continue
				}
				found = true
				if got := intString(m["port"]); got != want {
					t.Errorf("service port %s = %q, want %q", tt.name, got, want)
				}
				if m["targetPort"] != tt.name {
					t.Errorf("service targetPort %s = %v, want the named container port", tt.name, m["targetPort"])
				}
			}
			if !found {
				t.Errorf("service has no port named %s", tt.name)
			}
		})
	}
}

func intString(value any) string {
	number, ok := value.(int)
	if !ok {
		return ""
	}
	return strconv.Itoa(number)
}

func TestDeploymentIsRestricted(t *testing.T) {
	t.Parallel()

	deployment := baseDeployment(t)
	podSpec := []any{"spec", "template", "spec"}
	containerSpec := append(slices.Clone(podSpec), "containers", 0)
	with := func(prefix []any, rest ...any) []any {
		return append(slices.Clone(prefix), rest...)
	}
	tests := []struct {
		name string
		path []any
		want any
	}{
		{"replicas", []any{"spec", "replicas"}, 1},
		{"strategy", []any{"spec", "strategy", "type"}, "RollingUpdate"},
		{"max unavailable", []any{"spec", "strategy", "rollingUpdate", "maxUnavailable"}, 0},
		{"max surge", []any{"spec", "strategy", "rollingUpdate", "maxSurge"}, 1},
		{"run as non root", with(podSpec, "securityContext", "runAsNonRoot"), true},
		{"run as user", with(podSpec, "securityContext", "runAsUser"), 65532},
		{"run as group", with(podSpec, "securityContext", "runAsGroup"), 65532},
		{"fs group", with(podSpec, "securityContext", "fsGroup"), 65532},
		{"seccomp", with(podSpec, "securityContext", "seccompProfile", "type"), "RuntimeDefault"},
		{"privilege escalation", with(containerSpec, "securityContext", "allowPrivilegeEscalation"), false},
		{"read only root filesystem", with(containerSpec, "securityContext", "readOnlyRootFilesystem"), true},
		{"startup probe path", with(containerSpec, "startupProbe", "httpGet", "path"), "/healthz"},
		{"liveness probe path", with(containerSpec, "livenessProbe", "httpGet", "path"), "/healthz"},
		{"readiness probe path", with(containerSpec, "readinessProbe", "httpGet", "path"), "/readyz"},
		{"readiness probe port", with(containerSpec, "readinessProbe", "httpGet", "port"), "http"},
		{"memory request", with(containerSpec, "resources", "requests", "memory"), "64Mi"},
		{"memory limit", with(containerSpec, "resources", "limits", "memory"), "256Mi"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := deployment.lookup(t, tt.path...); got != tt.want {
				t.Errorf("%v = %v, want %v", tt.path, got, tt.want)
			}
		})
	}

	t.Run("capabilities dropped", func(t *testing.T) {
		t.Parallel()
		got := deployment.lookup(t, with(containerSpec, "securityContext", "capabilities", "drop")...)
		if want := []any{"ALL"}; !slices.Equal(got.([]any), want) {
			t.Errorf("capabilities.drop = %v, want %v", got, want)
		}
	})

	t.Run("no CPU limit", func(t *testing.T) {
		t.Parallel()
		limits := deployment.lookup(t, with(containerSpec, "resources", "limits")...).(map[string]any)
		if _, ok := limits["cpu"]; ok {
			t.Error("a CPU limit would throttle the service")
		}
	})

	t.Run("host access absent", func(t *testing.T) {
		t.Parallel()
		spec := deployment.lookup(t, podSpec...).(map[string]any)
		for _, key := range []string{"hostNetwork", "hostPID", "hostIPC"} {
			if spec[key] == true {
				t.Errorf("%s is enabled", key)
			}
		}
		for _, volume := range spec["volumes"].([]any) {
			if _, ok := volume.(map[string]any)["hostPath"]; ok {
				t.Errorf("volume %v is a hostPath", volume)
			}
		}
	})

	mountSources := []struct {
		name string
		spec func(t *testing.T) map[string]any
	}{
		{"base", func(t *testing.T) map[string]any {
			t.Helper()
			return deployment.lookup(t, podSpec...).(map[string]any)
		}},
		{"registry credentials component", func(t *testing.T) map[string]any {
			t.Helper()
			return registryPatch(t).lookup(t, podSpec...).(map[string]any)
		}},
	}
	for _, source := range mountSources {
		t.Run(source.name+" mounts are read only", func(t *testing.T) {
			t.Parallel()
			container := source.spec(t)["containers"].([]any)[0].(map[string]any)
			for _, mount := range container["volumeMounts"].([]any) {
				if mount.(map[string]any)["readOnly"] != true {
					t.Errorf("mount %v is writable", mount)
				}
			}
		})
	}
}

func TestClusterRolesAreReadOnly(t *testing.T) {
	t.Parallel()

	tests := []struct {
		file        string
		bindingFile string
		name        string
		rule        map[string][]string
	}{
		{
			file: "base/rbac.yaml",
			name: "doorstep-applications-reader",
			rule: map[string][]string{"apiGroups": {"argoproj.io"}, "resources": {"applications"}, "verbs": {"get", "list", "watch"}},
		},
		{
			file: "components/job-gate/rbac.yaml",
			name: "doorstep-jobs-reader",
			rule: map[string][]string{"apiGroups": {"batch"}, "resources": {"jobs"}, "verbs": {"get"}},
		},
		{
			file:        "components/pod-diagnostics/rbac.yaml",
			bindingFile: "components/pod-diagnostics-cluster-wide/rbac.yaml",
			name:        "doorstep-pods-reader",
			rule:        map[string][]string{"apiGroups": {""}, "resources": {"pods"}, "verbs": {"list"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			bindingFile := cmp.Or(tt.bindingFile, tt.file)
			files := slices.Compact([]string{tt.file, bindingFile})
			documents := 0
			for _, file := range files {
				documents += len(readDocuments(t, file))
			}
			if documents != 2 {
				t.Errorf("%v hold %d documents, want a ClusterRole and its binding", files, documents)
			}
			role := findDocument(t, tt.file, "ClusterRole", tt.name)
			rules := role.lookup(t, "rules").([]any)
			if len(rules) != 1 {
				t.Fatalf("got %d rules, want 1", len(rules))
			}
			rule := rules[0].(map[string]any)
			if len(rule) != len(tt.rule) {
				t.Errorf("rule has keys %v, want only %v", rule, tt.rule)
			}
			for key, values := range tt.rule {
				if got := namesFromStrings(rule[key]); !slices.Equal(got, values) {
					t.Errorf("%s = %v, want %v", key, got, values)
				}
			}

			binding := findDocument(t, bindingFile, "ClusterRoleBinding", tt.name)
			if got := binding.lookup(t, "roleRef", "name"); got != tt.name {
				t.Errorf("binding roleRef = %v", got)
			}
			if got := binding.lookup(t, "roleRef", "kind"); got != "ClusterRole" {
				t.Errorf("binding roleRef kind = %v", got)
			}
			subjects := binding.lookup(t, "subjects").([]any)
			if len(subjects) != 1 {
				t.Fatalf("binding has %d subjects, want 1", len(subjects))
			}
			if got := binding.lookup(t, "subjects", 0, "kind"); got != "ServiceAccount" {
				t.Errorf("binding subject kind = %v", got)
			}
			if got := binding.lookup(t, "subjects", 0, "name"); got != "doorstep" {
				t.Errorf("binding subject = %v", got)
			}
		})
	}
}

func namesFromStrings(value any) []string {
	list, _ := value.([]any)
	var out []string
	for _, item := range list {
		out = append(out, item.(string))
	}
	return out
}

func TestImagesArePinnedByDigest(t *testing.T) {
	t.Parallel()

	files := []string{"base/kustomization.yaml"}
	overlays, err := filepath.Glob("examples/*/kustomization.yaml")
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, overlays...)
	if len(overlays) == 0 {
		t.Fatal("no example overlays found")
	}
	pattern := regexp.MustCompile(digestPattern)
	for _, file := range files {
		t.Run(file, func(t *testing.T) {
			t.Parallel()
			kustomization := readDocuments(t, file)[0]
			var entry map[string]any
			for _, image := range kustomization.lookup(t, "images").([]any) {
				if m := image.(map[string]any); m["name"] == doorstepImage {
					entry = m
				}
			}
			if entry == nil {
				t.Fatalf("no images entry for %s", doorstepImage)
			}
			digest, _ := entry["digest"].(string)
			if !pattern.MatchString(digest) {
				t.Errorf("digest %q does not match %s", digest, digestPattern)
			}
			for _, forbidden := range []string{"newTag", "newName"} {
				if _, ok := entry[forbidden]; ok {
					t.Errorf("images entry sets %s", forbidden)
				}
			}
		})
	}

	t.Run("deployment image is untagged", func(t *testing.T) {
		t.Parallel()
		if got := baseContainer(t)["image"]; got != doorstepImage {
			t.Errorf("image = %v, want %s", got, doorstepImage)
		}
	})
}

func configFiles(t *testing.T) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && entry.Name() == "config.yaml" {
			files = append(files, name)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no config.yaml files found")
	}
	return files
}

func loadConfig(t *testing.T, name string) config.File {
	t.Helper()
	data, err := os.ReadFile(filepath.Clean(name))
	if err != nil {
		t.Fatal(err)
	}
	file, err := config.Parse(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return file
}

func TestConfigFilesAreValid(t *testing.T) {
	t.Parallel()

	for _, name := range configFiles(t) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			file := loadConfig(t, name)
			if err := file.Apply(config.Default()).ValidateSettings(); err != nil {
				t.Errorf("invalid settings: %v", err)
			}
			if err := file.Catalog().Validate(); err != nil {
				t.Errorf("invalid catalog: %v", err)
			}
			if strings.HasPrefix(name, "examples") && len(file.Apps) == 0 {
				t.Error("example defines no apps")
			}
		})
	}
}

func TestExampleSecretPathsAreMounted(t *testing.T) {
	t.Parallel()

	var mounts []string
	for _, source := range []document{baseDeployment(t), registryPatch(t)} {
		container := source.lookup(t, "spec", "template", "spec", "containers", 0).(map[string]any)
		mounts = append(mounts, namesOf(t, container["volumeMounts"], "mountPath")...)
	}
	for _, name := range configFiles(t) {
		if !strings.HasPrefix(name, "examples") {
			continue
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			file := loadConfig(t, name)
			settings := file.Apply(config.Default())
			paths := []string{
				settings.Database.URLFile,
				settings.GitHub.PrivateKeyFile,
				settings.Registry.CredentialsFile,
			}
			for _, ref := range file.Catalog().SecretRefs() {
				paths = append(paths, ref.File)
			}
			var checked int
			for _, p := range paths {
				if p == "" {
					continue
				}
				checked++
				if !slices.Contains(mounts, path.Dir(p)) {
					t.Errorf("%s is not under a mounted directory %v", p, mounts)
				}
			}
			if checked == 0 {
				t.Error("example references no secret files")
			}
		})
	}
}

func TestSelfHostedExampleManagesItsOwnImage(t *testing.T) {
	t.Parallel()

	const dir = "examples/self-hosted"
	file := loadConfig(t, dir+"/config.yaml")
	var self *config.App
	for i, app := range file.Catalog().Apps {
		if app.Image.Repository == doorstepImage {
			self = &file.Catalog().Apps[i]
		}
	}
	if self == nil {
		t.Fatalf("no app in %s deploys %s", dir, doorstepImage)
	}
	checks := []struct {
		name string
		got  any
		want any
	}{
		{"source repository", string(self.Source.Repo), "sharpintelligence/doorstep"},
		{"source branch", self.Source.Branch, "release"},
		{"required checks", strings.Join(self.Source.RequiredChecks, ","), "check,lint-manifests,image"},
		{"pin strategy", self.Pin.Strategy(), config.PinKustomize},
		{"pinned image", self.Pin.Kustomize.Image, doorstepImage},
		{"migration trailer", self.Migrations != nil && self.Migrations.TrailerEnabled() && len(self.Migrations.Labels) == 0, true},
		{"automatic rollback", self.AutoRollback, true},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
	if timeout := self.ArgoCD.Timeout(); timeout < 5*time.Minute+time.Minute {
		t.Errorf("sync timeout %s does not cover the 300 s startup probe", timeout)
	}
	var gates []string
	for _, gate := range self.Gates.After {
		gates = append(gates, string(gate.Type))
	}
	if !slices.Contains(gates, "health") || !slices.Contains(gates, "soak") {
		t.Errorf("after gates = %v, want health and soak", gates)
	}

	kustomization := readDocuments(t, dir+"/kustomization.yaml")[0]
	var digests []string
	for _, image := range kustomization.lookup(t, "images").([]any) {
		if m := image.(map[string]any); m["name"] == self.Pin.Kustomize.Image {
			digest, _ := m["digest"].(string)
			digests = append(digests, digest)
		}
	}
	if len(digests) != 1 || !regexp.MustCompile(digestPattern).MatchString(digests[0]) {
		t.Errorf("images entries for %s = %v, want exactly one with a digest doorstep can pin", self.Pin.Kustomize.Image, digests)
	}
}
