package pypi

import (
	"regexp"
	"strings"

	"github.com/go-git/go-git/v5/plumbing/object"
	"gopkg.in/yaml.v3"
)

func walkYamlEnv(n *yaml.Node, cb func(k, v string)) {
	if n == nil {
		return
	}
	if n.Kind == yaml.DocumentNode || n.Kind == yaml.SequenceNode {
		for _, c := range n.Content {
			walkYamlEnv(c, cb)
		}
	} else if n.Kind == yaml.MappingNode {
		for i := 0; i < len(n.Content); i += 2 {
			k := n.Content[i].Value
			v := n.Content[i+1]
			if k == "env" && v.Kind == yaml.MappingNode {
				for j := 0; j < len(v.Content); j += 2 {
					ek := v.Content[j].Value
					ev := v.Content[j+1].Value
					cb(ek, ev)
				}
			}
			if k == "CIBW_ENVIRONMENT" || k == "CIBW_ENVIRONMENT_LINUX" || k == "CIBW_ENV" {
				cb(k, v.Value)
			}
			walkYamlEnv(v, cb)
		}
	}
}

func extractCibuildwheelEnv(tree *object.Tree, searchDir string) string {
	if tree == nil {
		return ""
	}
	var envs []string
	
	// Check pyproject.toml
	if f, err := tree.File("pyproject.toml"); err == nil {
		if content, err := f.Contents(); err == nil {
			inEnv := false
			for _, line := range strings.Split(content, "\n") {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "[") {
					inEnv = strings.HasPrefix(line, "[tool.cibuildwheel.environment]") || strings.HasPrefix(line, "[tool.cibuildwheel.linux.environment]")
					continue
				}
				if inEnv && strings.Contains(line, "=") {
					parts := strings.SplitN(line, "=", 2)
					k := strings.TrimSpace(parts[0])
					v := strings.TrimSpace(parts[1])
					// basic toml string removal
					v = strings.TrimSuffix(strings.TrimPrefix(v, `"`), `"`)
					v = strings.TrimSuffix(strings.TrimPrefix(v, `'`), `'`)
					envs = append(envs, k+"="+v)
				}
			}
		}
	}

	// Check .github/workflows/
	workflowsDir := ".github/workflows"
	// if searchDir != "" && searchDir != "." {
	//    Usually workflows are at the root of the repo, not in searchDir.
	// }
	
	err := tree.Files().ForEach(func(f *object.File) error {
		if strings.HasPrefix(f.Name, workflowsDir) && (strings.HasSuffix(f.Name, ".yml") || strings.HasSuffix(f.Name, ".yaml")) {
			content, err := f.Contents()
			if err == nil {
				var node yaml.Node
				if err := yaml.Unmarshal([]byte(content), &node); err == nil {
					walkYamlEnv(&node, func(k, v string) {
						if k == "CIBW_ENVIRONMENT" || k == "CIBW_ENVIRONMENT_LINUX" || k == "CIBW_ENV" {
							v = strings.ReplaceAll(v, "\n", " ")
							
							// Find assignments like VAR=val or VAR='val' or VAR="val"
							// but skip anything containing ${{ or }}
							re := regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*=(?:'[^']*'|"[^"]*"|[^\s]+)`)
							matches := re.FindAllString(v, -1)
							for _, m := range matches {
								if !strings.Contains(m, "{{") && !strings.Contains(m, "}}") {
									envs = append(envs, m)
								}
							}
						}
					})
				}
			}
		}
		return nil
	})
	if err != nil {
		return ""
	}
	
	seen := make(map[string]bool)
	var deduped []string
	for _, env := range envs {
		if !seen[env] {
			seen[env] = true
			deduped = append(deduped, env)
		}
	}
	
	return strings.Join(deduped, " ")
}
