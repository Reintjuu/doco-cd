package docker

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"

	"github.com/compose-spec/compose-go/v2/dotenv"
	"github.com/compose-spec/compose-go/v2/template"
	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/compose/v5/pkg/remote"
	"github.com/moby/buildkit/frontend/dockerfile/dfgitutil"
	"go.yaml.in/yaml/v4"
)

// composeFileRefs returns the local files a project loads through include (with the entry's
// env files) and extends.file. compose-go doesn't keep them in types.Project and only reports
// top-level includes to listeners (compose-spec/compose-go#1007), so the compose files are read
// again here. Remote references and unreadable files are skipped.
func composeFileRefs(p *types.Project) []string {
	w := &composeRefWalker{visited: make(map[string]bool)}

	for _, f := range p.ComposeFiles {
		w.walkComposeFile(f, p.WorkingDir, p.Environment)
	}

	return w.files
}

type composeRefWalker struct {
	// visited also stops include and extends cycles.
	visited map[string]bool
	files   []string
}

// addFile records path if it is an existing regular file.
func (w *composeRefWalker) addFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}

	w.files = append(w.files, path)

	return true
}

// walkComposeFile follows the includes and extends of a compose file of the project or an
// included one. projectDir is the directory its relative references resolve against.
func (w *composeRefWalker) walkComposeFile(file, projectDir string, env types.Mapping) {
	key := "compose\x00" + file + "\x00" + projectDir
	if w.visited[key] {
		return
	}

	w.visited[key] = true

	for _, doc := range readComposeDocuments(file) {
		includes, _ := doc["include"].([]any)
		for _, entry := range includes {
			w.walkInclude(entry, projectDir, env)
		}

		for _, section := range []string{"services", "jobs"} {
			entries, _ := doc[section].(map[string]any)
			for _, entry := range entries {
				w.walkExtends(entry, section, projectDir, env)
			}
		}
	}
}

// walkInclude records the local files of an include entry and walks the included compose
// files. Like compose-go, it resolves the paths, env files and project_directory of the entry
// against baseDir, and the references inside the included files against the entry's project
// directory, which defaults to the directory of the first path.
func (w *composeRefWalker) walkInclude(entry any, baseDir string, env types.Mapping) {
	var paths, envFiles []string

	projectDir := ""

	switch v := entry.(type) {
	case string:
		paths = []string{v}
	case map[string]any:
		paths = stringOrList(v["path"])
		envFiles = stringOrList(v["env_file"])
		projectDir, _ = v["project_directory"].(string)
	default:
		return
	}

	if len(paths) == 0 {
		return
	}

	mainPath := interpolateComposeRef(paths[0], env)
	if mainPath == "" {
		return
	}

	// Without project_directory, a remote main file puts the project directory in its
	// remote copy: the default .env and the references inside the files are not ours.
	remoteProject := false

	switch projectDir = interpolateComposeRef(projectDir, env); {
	case projectDir != "":
		projectDir = resolveComposeRef(baseDir, projectDir)
	case isRemoteComposeRef(mainPath):
		remoteProject = true
	default:
		projectDir = filepath.Dir(resolveComposeRef(baseDir, mainPath))
	}

	var localPaths, resolvedEnvFiles []string

	for _, p := range paths {
		if p = interpolateComposeRef(p, env); p != "" && !isRemoteComposeRef(p) {
			localPaths = append(localPaths, resolveComposeRef(baseDir, p))
		}
	}

	if len(envFiles) == 0 && !remoteProject {
		if dotEnv := filepath.Join(projectDir, ".env"); w.addFile(dotEnv) {
			resolvedEnvFiles = append(resolvedEnvFiles, dotEnv)
		}
	}

	for _, f := range envFiles {
		f = interpolateComposeRef(f, env)
		if f == "" || f == "/dev/null" {
			continue
		}

		if f = resolveComposeRef(baseDir, f); w.addFile(f) {
			resolvedEnvFiles = append(resolvedEnvFiles, f)
		}
	}

	includeEnv := env
	if len(resolvedEnvFiles) > 0 {
		if fromFiles, err := dotenv.GetEnvFromFile(env, resolvedEnvFiles); err == nil {
			includeEnv = env.Clone().Merge(fromFiles)
		}
	}

	for _, p := range localPaths {
		if w.addFile(p) && !remoteProject {
			w.walkComposeFile(p, projectDir, includeEnv)
		}
	}
}

// walkExtends follows extends.file of a service or job. compose-go resolves extends before
// it applies profiles, so services of inactive profiles count too.
func (w *composeRefWalker) walkExtends(entry any, section, baseDir string, env types.Mapping) {
	m, _ := entry.(map[string]any)
	ext, _ := m["extends"].(map[string]any)

	file, ref, ok := extendsFile(ext, env)
	if !ok {
		return
	}

	w.walkExtendedFile(resolveComposeRef(baseDir, file), section, ref, env)
}

// walkExtendedFile records an extended file and follows the extends chain of the entry ref
// in it. References in an extended file are relative to its directory, and its includes are
// ignored, as compose-go does.
func (w *composeRefWalker) walkExtendedFile(file, section, ref string, env types.Mapping) {
	key := "extends\x00" + file + "\x00" + section + "\x00" + ref
	if w.visited[key] {
		return
	}

	w.visited[key] = true

	if !w.addFile(file) {
		return
	}

	for _, doc := range readComposeDocuments(file) {
		entries, _ := doc[section].(map[string]any)
		extended, _ := entries[ref].(map[string]any)

		switch v := extended["extends"].(type) {
		case string:
			w.walkExtendedFile(file, section, interpolateComposeRef(v, env), env)
		case map[string]any:
			if nextFile, nextRef, ok := extendsFile(v, env); ok {
				w.walkExtendedFile(resolveComposeRef(filepath.Dir(file), nextFile), section, nextRef, env)
			} else if nextRef, _ := v["service"].(string); nextRef != "" {
				w.walkExtendedFile(file, section, interpolateComposeRef(nextRef, env), env)
			}
		}
	}
}

// extendsFile returns the file and entry an extends mapping refers to. ok is false without a
// file or for a remote one.
func extendsFile(ext map[string]any, env types.Mapping) (file, ref string, ok bool) {
	file, _ = ext["file"].(string)
	ref, _ = ext["service"].(string)

	file = interpolateComposeRef(file, env)
	ref = interpolateComposeRef(ref, env)

	if file == "" || ref == "" || isRemoteComposeRef(file) {
		return "", "", false
	}

	return file, ref, true
}

// readComposeDocuments parses the YAML documents of a compose file, up to the first one that
// fails to parse.
func readComposeDocuments(file string) []map[string]any {
	data, err := os.ReadFile(file) //nolint:gosec // compose file paths are resolved by doco-cd itself
	if err != nil {
		return nil
	}

	var docs []map[string]any

	decoder := yaml.NewDecoder(bytes.NewReader(data))

	for {
		var node yaml.Node
		if err := decoder.Decode(&node); err != nil {
			return docs
		}

		dropResetNodes(&node)

		var doc map[string]any
		if err := node.Decode(&doc); err != nil {
			return docs
		}

		if doc != nil {
			docs = append(docs, doc)
		}
	}
}

// dropResetNodes removes the values tagged !reset, which compose-go drops from a document
// before it loads it.
func dropResetNodes(n *yaml.Node) {
	isReset := func(v *yaml.Node) bool {
		return v.Tag == "!reset" || (v.Kind == yaml.AliasNode && v.Alias != nil && v.Alias.Tag == "!reset")
	}

	switch n.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		kept := n.Content[:0]

		for _, v := range n.Content {
			if !isReset(v) {
				dropResetNodes(v)
				kept = append(kept, v)
			}
		}

		n.Content = kept
	case yaml.MappingNode:
		kept := n.Content[:0]

		for i := 0; i+1 < len(n.Content); i += 2 {
			if v := n.Content[i+1]; !isReset(v) {
				dropResetNodes(v)
				kept = append(kept, n.Content[i], v)
			}
		}

		n.Content = kept
	default:
	}
}

// stringOrList reads a compose field that takes a string or a list of strings.
func stringOrList(v any) []string {
	switch v := v.(type) {
	case string:
		return []string{v}
	case []any:
		list := make([]string, 0, len(v))

		for _, item := range v {
			if s, ok := item.(string); ok {
				list = append(list, s)
			}
		}

		return list
	default:
		return nil
	}
}

// interpolateComposeRef substitutes variables like compose-go does. It returns "" if that
// fails, e.g. for a required variable that is not set.
func interpolateComposeRef(s string, env types.Mapping) string {
	if !strings.Contains(s, "$") {
		return s
	}

	out, err := template.Substitute(s, func(key string) (string, bool) {
		v, ok := env[key]

		return v, ok
	})
	if err != nil {
		return ""
	}

	return out
}

func resolveComposeRef(baseDir, p string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}

	return filepath.Join(baseDir, p)
}

// isRemoteComposeRef reports whether doco-cd loads ref from a Git repository or an OCI
// artifact instead of the local file system.
func isRemoteComposeRef(ref string) bool {
	if strings.HasPrefix(ref, remote.OciPrefix) {
		return true
	}

	_, _, err := dfgitutil.ParseGitRef(ref)

	return err == nil
}
