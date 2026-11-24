package docs_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

var (
	linkPattern      = regexp.MustCompile(`\]\(\s*<?([^)\s>]+)>?(?:\s+(?:"[^"]*"|'[^']*'))?\s*\)`)
	definitionLink   = regexp.MustCompile(`^\s{0,3}\[[^\]]+\]:\s*<?([^\s>]+)>?`)
	hrefPattern      = regexp.MustCompile(`href="([^"]+)"`)
	envTableRow      = regexp.MustCompile("(?m)^\\| `(DOORSTEP_[A-Z0-9_]+)` \\|")
	schemePattern    = regexp.MustCompile(`^[a-z][a-z0-9+.-]*:`)
	headingPattern   = regexp.MustCompile(`^#{1,6}\s+(.*?)\s*#*\s*$`)
	inlineLink       = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	envPattern       = regexp.MustCompile(`^DOORSTEP_[A-Z0-9_]+$`)
	flagFuncPattern  = regexp.MustCompile(`^(String|Bool|Int|Int64|Uint|Duration|StringSlice|StringArray|Count)(Var)?P?$`)
	decisionFile     = regexp.MustCompile(`^\d{4}-.*\.md$`)
	decisionStatus   = regexp.MustCompile(`(?m)^- Status: (.+)$`)
	decisionRowLink  = regexp.MustCompile(`^\| \[\d{4}\]\(([^)]+)\) \|.*\| ([^|]+?) \| [^|]+ \|$`)
	wordsToDropRunes = regexp.MustCompile(`[^\p{L}\p{N} _-]`)
)

func repoFS() fs.FS {
	return os.DirFS("..")
}

func readFile(t *testing.T, name string) string {
	t.Helper()
	data, err := fs.ReadFile(repoFS(), name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(data)
}

func markdownFiles(t *testing.T, roots ...string) []string {
	t.Helper()
	var files []string
	for _, root := range roots {
		info, err := fs.Stat(repoFS(), root)
		if err != nil {
			t.Fatalf("stat %s: %v", root, err)
		}
		if !info.IsDir() {
			files = append(files, root)
			continue
		}
		err = fs.WalkDir(repoFS(), root, func(name string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !entry.IsDir() && strings.HasSuffix(name, ".md") {
				files = append(files, name)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	slices.Sort(files)
	return files
}

func proseLines(content string) []string {
	var lines []string
	fence := ""
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if fence != "" {
			if strings.HasPrefix(trimmed, fence) {
				fence = ""
			}
			continue
		}
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			fence = trimmed[:3]
			continue
		}
		lines = append(lines, line)
	}
	return lines
}

func headingSlug(heading string) string {
	text := inlineLink.ReplaceAllString(heading, "$1")
	text = strings.ReplaceAll(text, "`", "")
	text = wordsToDropRunes.ReplaceAllString(strings.ToLower(text), "")
	return strings.ReplaceAll(text, " ", "-")
}

func anchorsOf(content string) map[string]bool {
	anchors := map[string]bool{}
	seen := map[string]int{}
	for _, line := range proseLines(content) {
		match := headingPattern.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		slug := headingSlug(match[1])
		if count := seen[slug]; count > 0 {
			anchors[slug+"-"+strconv.Itoa(count)] = true
		} else {
			anchors[slug] = true
		}
		seen[slug]++
	}
	return anchors
}

func linkTargets(line string) []string {
	var targets []string
	for _, match := range linkPattern.FindAllStringSubmatch(line, -1) {
		targets = append(targets, match[1])
	}
	if match := definitionLink.FindStringSubmatch(line); match != nil {
		targets = append(targets, match[1])
	}
	for _, match := range hrefPattern.FindAllStringSubmatch(line, -1) {
		targets = append(targets, match[1])
	}
	return targets
}

func TestLinkTargets(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		line string
		want []string
	}{
		{"inline", "see [a](docs/a.md#x) now", []string{"docs/a.md#x"}},
		{"titled", `see [a](docs/a.md "Title") now`, []string{"docs/a.md"}},
		{"angle brackets", "see [a](<docs/a.md>)", []string{"docs/a.md"}},
		{"definition", "[ref]: docs/a.md", []string{"docs/a.md"}},
		{"html", `<a href="docs/a.md">a</a>`, []string{"docs/a.md"}},
		{"none", "plain text", nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := linkTargets(test.line); !slices.Equal(got, test.want) {
				t.Errorf("linkTargets(%q) = %v, want %v", test.line, got, test.want)
			}
		})
	}
}

func TestLinksResolve(t *testing.T) {
	t.Parallel()

	files := markdownFiles(t, "README.md", "CONTRIBUTING.md", "SECURITY.md", "CODE_OF_CONDUCT.md", "docs", "deploy")
	anchorCache := map[string]map[string]bool{}
	anchorsFor := func(name string) map[string]bool {
		if anchors, ok := anchorCache[name]; ok {
			return anchors
		}
		anchors := anchorsOf(readFile(t, name))
		anchorCache[name] = anchors
		return anchors
	}

	total := 0
	for _, file := range files {
		content := readFile(t, file)
		var problems []string
		for _, line := range proseLines(content) {
			for _, target := range linkTargets(line) {
				if schemePattern.MatchString(target) {
					continue
				}
				total++
				targetPath, fragment, _ := strings.Cut(target, "#")
				resolved := file
				if targetPath != "" {
					resolved = path.Join(path.Dir(file), targetPath)
				}
				info, err := fs.Stat(repoFS(), resolved)
				if err != nil {
					problems = append(problems, "missing file for "+target)
					continue
				}
				if fragment == "" || info.IsDir() || !strings.HasSuffix(resolved, ".md") {
					continue
				}
				if !anchorsFor(resolved)[fragment] {
					problems = append(problems, "missing anchor for "+target)
				}
			}
		}
		t.Run(file, func(t *testing.T) {
			t.Parallel()
			for _, problem := range problems {
				t.Error(problem)
			}
		})
	}
	if total < 100 {
		t.Fatalf("found only %d relative links, the link pattern is probably broken", total)
	}
}

func TestReadmeIndexesEveryDocument(t *testing.T) {
	t.Parallel()

	readme := readFile(t, "README.md")
	required := []string{"docs/decisions/README.md", "CONTRIBUTING.md", "SECURITY.md", "CODE_OF_CONDUCT.md"}
	for _, file := range markdownFiles(t, "docs") {
		if path.Dir(file) == "docs" {
			required = append(required, file)
		}
	}
	for _, name := range required {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if !strings.Contains(readme, "]("+name) {
				t.Errorf("README.md does not link %s", name)
			}
		})
	}
}

func TestDecisionIndexMatchesRecords(t *testing.T) {
	t.Parallel()

	index := map[string]string{}
	for _, line := range strings.Split(readFile(t, "docs/decisions/README.md"), "\n") {
		if match := decisionRowLink.FindStringSubmatch(line); match != nil {
			index[match[1]] = match[2]
		}
	}

	entries, err := fs.ReadDir(repoFS(), "docs/decisions")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, entry := range entries {
		name := entry.Name()
		if !decisionFile.MatchString(name) {
			continue
		}
		checked++
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			match := decisionStatus.FindStringSubmatch(readFile(t, "docs/decisions/"+name))
			if match == nil {
				t.Fatal("decision has no status line")
			}
			indexed, ok := index[name]
			if !ok {
				t.Fatal("decision has no row in the index")
			}
			if indexed != strings.TrimSpace(match[1]) {
				t.Errorf("index status %q, file status %q", indexed, match[1])
			}
		})
	}
	if checked == 0 {
		t.Fatal("no decision records found")
	}
	if len(index) != checked {
		t.Errorf("index has %d rows for %d decision records", len(index), checked)
	}
}

type configurationSurface struct {
	envs  map[string]bool
	keys  map[string]bool
	flags map[string]bool
}

func parseGoDir(t *testing.T, dir string) []*ast.File {
	t.Helper()
	entries, err := fs.ReadDir(repoFS(), dir)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, readFile(t, dir+"/"+name), 0)
		if err != nil {
			t.Fatalf("parse %s/%s: %v", dir, name, err)
		}
		files = append(files, file)
	}
	return files
}

func stringLiteral(expr ast.Expr) (string, bool) {
	literal, ok := expr.(*ast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		return "", false
	}
	value, err := strconv.Unquote(literal.Value)
	return value, err == nil
}

func stringConstants(files []*ast.File) map[string]string {
	constants := map[string]string{}
	for _, file := range files {
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				value := spec.(*ast.ValueSpec)
				for i, name := range value.Names {
					if i >= len(value.Values) {
						continue
					}
					if text, ok := stringLiteral(value.Values[i]); ok {
						constants[name.Name] = text
					}
				}
			}
		}
	}
	return constants
}

func isFlagSetCall(expr ast.Expr) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return false
	}
	selector, ok := call.Fun.(*ast.SelectorExpr)
	return ok && (selector.Sel.Name == "Flags" || selector.Sel.Name == "PersistentFlags" || selector.Sel.Name == "NewFlagSet")
}

func flagSetNames(file *ast.File) map[string]bool {
	names := map[string]bool{}
	ast.Inspect(file, func(node ast.Node) bool {
		switch typed := node.(type) {
		case *ast.Field:
			star, ok := typed.Type.(*ast.StarExpr)
			if !ok {
				return true
			}
			selector, ok := star.X.(*ast.SelectorExpr)
			if ok && selector.Sel.Name == "FlagSet" {
				for _, name := range typed.Names {
					names[name.Name] = true
				}
			}
		case *ast.AssignStmt:
			for i, right := range typed.Rhs {
				if ident, ok := typed.Lhs[min(i, len(typed.Lhs)-1)].(*ast.Ident); ok && isFlagSetCall(right) {
					names[ident.Name] = true
				}
			}
		}
		return true
	})
	return names
}

func isFlagSet(expr ast.Expr, names map[string]bool) bool {
	if ident, ok := expr.(*ast.Ident); ok {
		return names[ident.Name]
	}
	return isFlagSetCall(expr)
}

func collectSurface(t *testing.T) configurationSurface {
	t.Helper()
	surface := configurationSurface{envs: map[string]bool{}, keys: map[string]bool{}, flags: map[string]bool{}}

	configFiles := parseGoDir(t, "internal/config")
	cliFiles := parseGoDir(t, "internal/cli")

	for _, files := range [][]*ast.File{configFiles, cliFiles} {
		for _, text := range stringConstants(files) {
			if envPattern.MatchString(text) {
				surface.envs[text] = true
			}
		}
	}

	yamlTag := regexp.MustCompile(`yaml:"([^",]*)`)
	for _, file := range configFiles {
		ast.Inspect(file, func(node ast.Node) bool {
			field, ok := node.(*ast.Field)
			if !ok || field.Tag == nil {
				return true
			}
			tag, err := strconv.Unquote(field.Tag.Value)
			if err != nil {
				return true
			}
			if match := yamlTag.FindStringSubmatch(tag); match != nil && match[1] != "" && match[1] != "-" {
				surface.keys[match[1]] = true
			}
			return true
		})
	}

	constants := stringConstants(cliFiles)
	for _, file := range cliFiles {
		flagSets := flagSetNames(file)
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !flagFuncPattern.MatchString(selector.Sel.Name) || !isFlagSet(selector.X, flagSets) {
				return true
			}
			for _, arg := range call.Args {
				if text, ok := stringLiteral(arg); ok {
					surface.flags[text] = true
					break
				}
				if ident, ok := arg.(*ast.Ident); ok {
					if text, ok := constants[ident.Name]; ok {
						surface.flags[text] = true
						break
					}
				}
			}
			return true
		})
	}
	return surface
}

func TestConfigurationReferenceIsComplete(t *testing.T) {
	t.Parallel()

	surface := collectSurface(t)
	reference := readFile(t, "docs/configuration.md")

	minimums := []struct {
		name  string
		items map[string]bool
		min   int
	}{
		{"environment variables", surface.envs, 20},
		{"yaml keys", surface.keys, 100},
		{"flags", surface.flags, 25},
	}
	for _, group := range minimums {
		if len(group.items) < group.min {
			t.Fatalf("collected only %d %s, the collector is probably broken", len(group.items), group.name)
		}
	}

	indexed := map[string]bool{}
	for _, match := range envTableRow.FindAllStringSubmatch(reference, -1) {
		indexed[match[1]] = true
	}
	for env := range surface.envs {
		t.Run(env, func(t *testing.T) {
			t.Parallel()
			if !indexed[env] {
				t.Errorf("docs/configuration.md has no row for %s in the environment variable index", env)
			}
		})
	}
	for key := range surface.keys {
		documented := regexp.MustCompile("(?m)^\\|[^|]*`" + regexp.QuoteMeta(key) + "`[^|]*\\||^\\s*(- )?" + regexp.QuoteMeta(key) + ":|`[A-Za-z0-9_.\\[\\]]*\\." + regexp.QuoteMeta(key) + "(\\[\\])?`")
		t.Run("yaml "+key, func(t *testing.T) {
			t.Parallel()
			if !documented.MatchString(reference) {
				t.Errorf("docs/configuration.md does not document the key %s in a table row, YAML example or dotted path", key)
			}
		})
	}
	for flag := range surface.flags {
		t.Run("--"+flag, func(t *testing.T) {
			t.Parallel()
			if !strings.Contains(reference, "--"+flag) {
				t.Errorf("docs/configuration.md does not mention --%s", flag)
			}
		})
	}
}

func TestTestVariablesAreDocumentedInDevelopmentGuide(t *testing.T) {
	t.Parallel()

	guide := readFile(t, "docs/development.md")
	for _, name := range []string{"DOORSTEP_TEST_DATABASE_URL", "DOORSTEP_TEST_REQUIRE_DATABASE"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if !strings.Contains(guide, name) {
				t.Errorf("docs/development.md does not mention %s", name)
			}
		})
	}
}
