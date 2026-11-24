package kustomize_test

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/kyaml/filesys"
)

func render(t *testing.T, dir string) []document {
	t.Helper()
	resources, err := krusty.MakeKustomizer(krusty.MakeDefaultOptions()).Run(filesys.MakeFsOnDisk(), dir)
	if err != nil {
		t.Fatalf("build %s: %v", dir, err)
	}
	data, err := resources.AsYaml()
	if err != nil {
		t.Fatalf("render %s: %v", dir, err)
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
			t.Fatalf("parse rendered %s: %v", dir, err)
		}
		documents = append(documents, document(doc))
	}
}

func renderedOfKind(documents []document, kind string) []document {
	var found []document
	for _, doc := range documents {
		if doc["kind"] == kind {
			found = append(found, doc)
		}
	}
	return found
}

func renderedPodSpec(t *testing.T, documents []document) map[string]any {
	t.Helper()
	deployments := renderedOfKind(documents, "Deployment")
	if len(deployments) != 1 {
		t.Fatalf("got %d deployments, want 1", len(deployments))
	}
	return deployments[0].lookup(t, "spec", "template", "spec").(map[string]any)
}

func envValues(t *testing.T, podSpec map[string]any) map[string]string {
	t.Helper()
	container := podSpec["containers"].([]any)[0].(map[string]any)
	values := map[string]string{}
	for _, item := range container["env"].([]any) {
		entry := item.(map[string]any)
		name := entry["name"].(string)
		if _, duplicate := values[name]; duplicate {
			t.Errorf("env %s is defined twice", name)
		}
		values[name], _ = entry["value"].(string)
	}
	return values
}

func volumeNamesAndConfigMaps(podSpec map[string]any) ([]string, map[string]string) {
	var names []string
	configMaps := map[string]string{}
	for _, item := range podSpec["volumes"].([]any) {
		volume := item.(map[string]any)
		name := volume["name"].(string)
		names = append(names, name)
		if source, ok := volume["configMap"].(map[string]any); ok {
			configMaps[name] = source["name"].(string)
		}
	}
	return names, configMaps
}

func assertRenderedPod(t *testing.T, documents []document, wantEnv map[string]string) {
	t.Helper()
	podSpec := renderedPodSpec(t, documents)
	volumeNames, configMapVolumes := volumeNamesAndConfigMaps(podSpec)

	seen := map[string]bool{}
	for _, name := range volumeNames {
		if seen[name] {
			t.Errorf("volume %s is defined twice", name)
		}
		seen[name] = true
	}

	container := podSpec["containers"].([]any)[0].(map[string]any)
	for _, mount := range container["volumeMounts"].([]any) {
		name := mount.(map[string]any)["name"].(string)
		if !seen[name] {
			t.Errorf("mount %s has no volume", name)
		}
	}

	env := envValues(t, podSpec)
	for name, want := range wantEnv {
		if got := env[name]; got != want {
			t.Errorf("env %s = %q, want %q", name, got, want)
		}
	}

	configMaps := renderedOfKind(documents, "ConfigMap")
	if len(configMaps) != 1 {
		t.Fatalf("got %d config maps, want 1", len(configMaps))
	}
	generated := configMaps[0].lookup(t, "metadata", "name").(string)
	if !regexp.MustCompile(`^doorstep-config-[a-z0-9]+$`).MatchString(generated) {
		t.Errorf("config map name %q has no content hash", generated)
	}
	if got := configMapVolumes["config"]; got != generated {
		t.Errorf("config volume references %q, want %q", got, generated)
	}
}

func assertBindingSubjectsNamespaced(t *testing.T, documents []document, namespace string) {
	t.Helper()
	bindings := renderedOfKind(documents, "ClusterRoleBinding")
	if len(bindings) != 1 {
		t.Fatalf("got %d bindings, want 1", len(bindings))
	}
	if got := bindings[0].lookup(t, "subjects", 0, "namespace"); got != namespace {
		t.Errorf("subject namespace = %v, want %s", got, namespace)
	}
}

func TestRenderedExamples(t *testing.T) {
	t.Parallel()

	overlays, err := filepath.Glob("examples/*")
	if err != nil || len(overlays) == 0 {
		t.Fatalf("no example overlays found: %v", err)
	}
	for _, overlay := range overlays {
		t.Run(overlay, func(t *testing.T) {
			t.Parallel()
			documents := render(t, overlay)
			assertRenderedPod(t, documents, map[string]string{
				"DOORSTEP_CONFIG":                    "/etc/doorstep/config/config.yaml",
				"DOORSTEP_DATABASE_URL_FILE":         "/etc/doorstep/secrets/database-url",
				"DOORSTEP_REGISTRY_CREDENTIALS_FILE": "/etc/doorstep/registry/.dockerconfigjson",
			})
			assertBindingSubjectsNamespaced(t, documents, "doorstep")
		})
	}
}

func documentedKubernetesBlocks(t *testing.T, marker string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "docs", "install.md"))
	if err != nil {
		t.Fatal(err)
	}
	var blocks []string
	for _, block := range strings.Split(string(data), "```yaml kubernetes\n")[1:] {
		body, _, _ := strings.Cut(block, "```")
		if strings.Contains(body, marker) {
			blocks = append(blocks, body)
		}
	}
	return blocks
}

func TestDocumentedCloudNativePGPatchRenders(t *testing.T) {
	t.Parallel()

	blocks := documentedKubernetesBlocks(t, "pg-app")
	if len(blocks) != 1 {
		t.Fatalf("got %d documented blocks mentioning pg-app, want 1", len(blocks))
	}
	var documented map[string]any
	if err := yaml.Unmarshal([]byte(blocks[0]), &documented); err != nil {
		t.Fatal(err)
	}
	overlay := map[string]any{
		"apiVersion": "kustomize.config.k8s.io/v1beta1",
		"kind":       "Kustomization",
		"resources":  []string{"../examples/homelab"},
		"patches":    documented["patches"],
	}
	data, err := yaml.Marshal(overlay)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(".", "render-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.WriteFile(filepath.Join(dir, "kustomization.yaml"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	documents := render(t, dir)
	assertRenderedPod(t, documents, map[string]string{
		"DOORSTEP_CONFIG":                    "/etc/doorstep/config/config.yaml",
		"DOORSTEP_DATABASE_URL_FILE":         "/etc/doorstep/database/database-url",
		"DOORSTEP_REGISTRY_CREDENTIALS_FILE": "/etc/doorstep/registry/.dockerconfigjson",
	})
	names, _ := volumeNamesAndConfigMaps(renderedPodSpec(t, documents))
	for _, want := range []string{"config", "secrets", "database", "registry-credentials"} {
		if !slices.Contains(names, want) {
			t.Errorf("volumes %v lack %s", names, want)
		}
	}
}

func TestRunAsIdentityMatchesDockerfile(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile(filepath.Join("..", "..", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	match := regexp.MustCompile(`(?m)^USER (\d+):(\d+)$`).FindStringSubmatch(string(data))
	if match == nil {
		t.Fatal("Dockerfile has no numeric USER user:group line")
	}
	securityContext := baseDeployment(t).lookup(t, "spec", "template", "spec", "securityContext").(map[string]any)
	for field, want := range map[string]string{"runAsUser": match[1], "runAsGroup": match[2], "fsGroup": match[2]} {
		if got := intString(securityContext[field]); got != want {
			t.Errorf("%s = %s, want %s from the Dockerfile", field, got, want)
		}
	}
}
