package powershell

import (
	"fmt"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"

	tree_sitter "github.com/tree-sitter/go-tree-sitter"
	tree_sitter_powershell "github.com/wharflab/tree-sitter-powershell/bindings/go"
)

var (
	staticURLAssignment = regexp.MustCompile(`(?is)^\s*(\$[\w]+(?:\.[\w]+)?)\s*=\s*['"](https?://[^'"\r\n]+)['"]\s*$`)
	staticURLHashEntry  = regexp.MustCompile(`(?is)\b(url[\w]*)\s*=\s*['"](https?://[^'"\r\n]+)['"]`)
	constantAssignment  = regexp.MustCompile(`(?im)^\s*\$([\w]+)\s*=\s*['"]([^'"\r\n]+)['"]\s*$`)
	envSubexpressionRef = regexp.MustCompile(`(?i)\$\(\$env:([A-Za-z_][A-Za-z0-9_]*)\)`)
	subexpressionRef    = regexp.MustCompile(`\$\(\$([A-Za-z_][A-Za-z0-9_]*)\)`)
	envVariableRef      = regexp.MustCompile(`(?i)\$env:([A-Za-z_][A-Za-z0-9_]*)`)
	bracedVariableRef   = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)
	variableReference   = regexp.MustCompile(`\$([A-Za-z_][A-Za-z0-9_]*)`)
	httpReference       = regexp.MustCompile(`(?i)https?://[^\s'"<>]+`)
	url64HashtableKey   = regexp.MustCompile(`(?im)^(\s*)url64(?:bit)?(\s*=)`)
	urlHashtableKey     = regexp.MustCompile(`(?im)^(\s*)url(\s*=)`)
)

type Resource struct {
	URL      string
	Filename string
	Variable string
}

type edit struct {
	start       uint
	end         uint
	replacement string
}

type Options struct {
	PackageVersion        string
	UseLocalInstallHelper bool
}

func Rewrite(script string) (string, []Resource, error) {
	return RewriteWithOptions(script, Options{})
}

func UsesDownloadInstallHelper(script string) (bool, error) {
	source := []byte(strings.TrimPrefix(script, "\uFEFF"))
	parser := tree_sitter.NewParser()
	defer parser.Close()
	language := tree_sitter.NewLanguage(tree_sitter_powershell.Language())
	if err := parser.SetLanguage(language); err != nil {
		return false, fmt.Errorf("load PowerShell grammar: %w", err)
	}
	tree := parser.Parse(source, nil)
	if tree == nil {
		return false, fmt.Errorf("parse PowerShell: parser returned no tree")
	}
	defer tree.Close()
	if tree.RootNode().HasError() {
		return false, fmt.Errorf("parse PowerShell: syntax tree contains errors")
	}
	found := false
	walk(tree.RootNode(), func(node *tree_sitter.Node) {
		if node.Kind() == "command_name" &&
			strings.EqualFold(strings.TrimSpace(node.Utf8Text(source)), "Install-ChocolateyPackage") {
			found = true
		}
	})
	return found, nil
}

func RewriteWithOptions(script string, options Options) (string, []Resource, error) {
	hadUTF8BOM := strings.HasPrefix(script, "\uFEFF")
	script = strings.TrimPrefix(script, "\uFEFF")
	restoreBOM := func(value string) string {
		if hadUTF8BOM {
			return "\uFEFF" + value
		}
		return value
	}
	source := []byte(script)
	parser := tree_sitter.NewParser()
	defer parser.Close()
	language := tree_sitter.NewLanguage(tree_sitter_powershell.Language())
	if err := parser.SetLanguage(language); err != nil {
		return "", nil, fmt.Errorf("load PowerShell grammar: %w", err)
	}
	tree := parser.Parse(source, nil)
	if tree == nil {
		return "", nil, fmt.Errorf("parse PowerShell: parser returned no tree")
	}
	defer tree.Close()
	root := tree.RootNode()
	if root.HasError() {
		return "", nil, fmt.Errorf("parse PowerShell: syntax tree contains errors")
	}
	usesLocalInstallHelper := options.UseLocalInstallHelper
	walk(root, func(node *tree_sitter.Node) {
		if node.Kind() == "command_name" && strings.EqualFold(strings.TrimSpace(node.Utf8Text(source)), "Install-ChocolateyPackage") {
			usesLocalInstallHelper = true
		}
	})

	constants := map[string]string{
		"packageversion":           options.PackageVersion,
		"packagepnpmversion":       options.PackageVersion,
		"chocolateypackageversion": options.PackageVersion,
	}
	for _, match := range constantAssignment.FindAllStringSubmatch(script, -1) {
		constants[strings.ToLower(match[1])] = match[2]
	}
	var edits []edit
	var resources []Resource
	var unresolved []string
	seenEdits := make(map[[2]uint]bool)
	seenResources := make(map[string]bool)
	filenameOwners := make(map[string]string)
	hasToolsDir := false
	addResource := func(node *tree_sitter.Node, rawURL, variable string) {
		resolved, ok := resolveURL(rawURL, constants)
		if !ok {
			unresolved = append(unresolved, rawURL)
			return
		}
		filename := resourceFilename(resolved, variable)
		if owner, exists := filenameOwners[strings.ToLower(filename)]; exists && owner != resolved {
			ext := path.Ext(filename)
			base := strings.TrimSuffix(filename, ext)
			filename = base + "-" + safeFilenamePart(variable) + ext
		}
		filenameOwners[strings.ToLower(filename)] = resolved
		key := [2]uint{node.StartByte(), node.EndByte()}
		if seenEdits[key] {
			return
		}
		seenEdits[key] = true
		replacement := fmt.Sprintf("([Uri](Join-Path $toolsDir '%s')).AbsoluteUri", escapeSingleQuote(filename))
		if usesLocalInstallHelper {
			replacement = fmt.Sprintf("(Join-Path $toolsDir '%s')", escapeSingleQuote(filename))
		}
		edits = append(edits, edit{node.StartByte(), node.EndByte(), replacement})
		resourceKey := resolved + "\x00" + filename
		if !seenResources[resourceKey] {
			seenResources[resourceKey] = true
			resources = append(resources, Resource{URL: resolved, Filename: filename, Variable: variable})
		}
	}
	walk(root, func(node *tree_sitter.Node) {
		nodeText := node.Utf8Text(source)
		if strings.Contains(strings.ToLower(nodeText), "$toolsdir") {
			hasToolsDir = true
		}
		switch node.Kind() {
		case "assignment_expression":
			match := staticURLAssignment.FindStringSubmatch(nodeText)
			if match != nil && strings.Contains(strings.ToLower(match[1]), "url") {
				literal := findURLStringNode(node, source)
				if literal != nil {
					addResource(literal, match[2], strings.TrimPrefix(match[1], "$"))
				}
			}
		case "command_name":
			if usesLocalInstallHelper && strings.EqualFold(strings.TrimSpace(nodeText), "Install-ChocolateyPackage") {
				edits = append(edits, edit{node.StartByte(), node.EndByte(), "Install-ChocolateyInstallPackage"})
			}
			if isDownloadHelper(nodeText) {
				for _, literal := range stringChildren(node.Parent(), source) {
					if rawURL := httpReference.FindString(literal.Utf8Text(source)); rawURL != "" {
						addResource(literal, rawURL, "url")
					}
				}
			}
		}
		if usesLocalInstallHelper && node.Kind() == "command_parameter" {
			switch strings.ToLower(strings.TrimSpace(nodeText)) {
			case "-url":
				edits = append(edits, edit{node.StartByte(), node.EndByte(), "-File"})
			case "-url64bit":
				edits = append(edits, edit{node.StartByte(), node.EndByte(), "-File64"})
			}
		}
		match := staticURLHashEntry.FindStringSubmatch(nodeText)
		if match != nil {
			literal := findURLStringNode(node, source)
			if literal != nil {
				addResource(literal, match[2], strings.ToLower(match[1]))
			}
		}
	})

	if len(unresolved) > 0 {
		sort.Strings(unresolved)
		return "", nil, fmt.Errorf("unresolved dynamic download URLs: %s", strings.Join(unresolved, ", "))
	}
	if len(resources) == 0 && len(edits) == 0 {
		return restoreBOM(script), nil, nil
	}
	rewritten := applyEdits(source, edits)
	if usesLocalInstallHelper {
		rewritten = url64HashtableKey.ReplaceAllString(rewritten, "${1}file64${2}")
		rewritten = urlHashtableKey.ReplaceAllString(rewritten, "${1}file${2}")
	}
	if !hasToolsDir {
		rewritten = "$toolsDir = Split-Path -Parent $MyInvocation.MyCommand.Definition\r\n" + rewritten
	}
	return restoreBOM(rewritten), resources, nil
}

func safeFilenamePart(value string) string {
	value = strings.ToLower(strings.TrimPrefix(value, "$"))
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			return r
		}
		return '-'
	}, value)
}

func resolveURL(raw string, constants map[string]string) (string, bool) {
	resolved := envSubexpressionRef.ReplaceAllStringFunc(raw, func(value string) string {
		match := envSubexpressionRef.FindStringSubmatch(value)
		return constants[strings.ToLower(match[1])]
	})
	resolved = subexpressionRef.ReplaceAllStringFunc(resolved, func(value string) string {
		match := subexpressionRef.FindStringSubmatch(value)
		return constants[strings.ToLower(match[1])]
	})
	resolved = bracedVariableRef.ReplaceAllStringFunc(resolved, func(value string) string {
		match := bracedVariableRef.FindStringSubmatch(value)
		return constants[strings.ToLower(match[1])]
	})
	resolved = envVariableRef.ReplaceAllStringFunc(resolved, func(value string) string {
		match := envVariableRef.FindStringSubmatch(value)
		return constants[strings.ToLower(match[1])]
	})
	resolved = variableReference.ReplaceAllStringFunc(resolved, func(value string) string {
		return constants[strings.ToLower(strings.TrimPrefix(value, "$"))]
	})
	return resolved, !variableReference.MatchString(resolved)
}

func isDownloadHelper(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	return strings.Contains(name, "chocolatey") && (strings.Contains(name, "package") || strings.Contains(name, "webfile") || strings.Contains(name, "windowsupdate"))
}

func findURLStringNode(node *tree_sitter.Node, source []byte) *tree_sitter.Node {
	var result *tree_sitter.Node
	walk(node, func(child *tree_sitter.Node) {
		if result == nil && child.Kind() == "string_literal" && httpReference.MatchString(child.Utf8Text(source)) {
			result = child
		}
	})
	return result
}

func stringChildren(node *tree_sitter.Node, source []byte) []*tree_sitter.Node {
	if node == nil {
		return nil
	}
	var result []*tree_sitter.Node
	walk(node, func(child *tree_sitter.Node) {
		if child.Kind() == "string_literal" && httpReference.MatchString(child.Utf8Text(source)) {
			result = append(result, child)
		}
	})
	return result
}

func parseExternalURLs(script string) ([]string, error) {
	source := []byte(script)
	parser := tree_sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(tree_sitter.NewLanguage(tree_sitter_powershell.Language())); err != nil {
		return nil, fmt.Errorf("load PowerShell grammar: %w", err)
	}
	tree := parser.Parse(source, nil)
	if tree == nil {
		return nil, fmt.Errorf("parse rewritten PowerShell: parser returned no tree")
	}
	defer tree.Close()
	if tree.RootNode().HasError() {
		return nil, fmt.Errorf("parse rewritten PowerShell: syntax tree contains errors")
	}
	return externalURLs(tree.RootNode(), source), nil
}

func externalURLs(root *tree_sitter.Node, source []byte) []string {
	var result []string
	walk(root, func(node *tree_sitter.Node) {
		if node.Kind() != "string_literal" {
			return
		}
		result = append(result, httpReference.FindAllString(node.Utf8Text(source), -1)...)
	})
	return result
}

func walk(node *tree_sitter.Node, visit func(*tree_sitter.Node)) {
	visit(node)
	for i := uint(0); i < node.NamedChildCount(); i++ {
		walk(node.NamedChild(i), visit)
	}
}

func applyEdits(source []byte, edits []edit) string {
	sort.Slice(edits, func(i, j int) bool { return edits[i].start > edits[j].start })
	result := append([]byte(nil), source...)
	lastStart := uint(len(source) + 1)
	for _, current := range edits {
		if current.end > lastStart {
			continue
		}
		result = append(result[:current.start], append([]byte(current.replacement), result[current.end:]...)...)
		lastStart = current.start
	}
	return string(result)
}

func resourceFilename(rawURL, fallback string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return fallback + ".bin"
	}
	filename := path.Base(parsed.Path)
	if filename == "." || filename == "/" || filename == "" {
		if product := safeQueryFilenamePart(parsed.Query().Get("product")); product != "" {
			if platform := safeQueryFilenamePart(parsed.Query().Get("os")); platform != "" {
				product += "-" + platform
			}
			return product + ".exe"
		}
		return fallback + ".bin"
	}
	return filename
}

func safeQueryFilenamePart(value string) string {
	value = strings.TrimSpace(value)
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._-", r) {
			return r
		}
		return '-'
	}, value)
}

func escapeSingleQuote(value string) string {
	return strings.ReplaceAll(value, "'", "''")
}
