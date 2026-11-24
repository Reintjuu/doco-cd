package docker

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/kimdre/doco-cd/internal/filesystem"
)

func writeComposeRefFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()

	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))

		if err := os.MkdirAll(filepath.Dir(path), filesystem.PermDir); err != nil {
			t.Fatalf("failed to create %s: %v", filepath.Dir(path), err)
		}

		if err := os.WriteFile(path, []byte(content), filesystem.PermOwner); err != nil {
			t.Fatalf("failed to write %s: %v", path, err)
		}
	}
}

// TestProjectPathFilter_IncludeAndExtends loads real projects with LoadCompose, so a path
// resolved differently than compose-go resolves it fails the load or the filter.
func TestProjectPathFilter_IncludeAndExtends(t *testing.T) {
	testCases := []struct {
		name string
		// workingDir is the working directory of the stack, relative to the repository root.
		workingDir string
		// composeFiles are relative to workingDir, docker-compose.yml if empty.
		composeFiles []string
		files        map[string]string
		environment  map[string]string
		matching     []string
		notMatching  []string
	}{
		{
			name:       "include with project_directory",
			workingDir: ".",
			files: map[string]string{
				"docker-compose.yml":     "include:\n  - path: apps/demo/compose.yml\n    project_directory: .\n",
				"apps/demo/compose.yml":  "services:\n  demo:\n    image: nginx:1.29-alpine\n",
				"apps/other/compose.yml": "services:\n  other:\n    image: nginx:1.29-alpine\n",
			},
			matching:    []string{"docker-compose.yml", "apps/demo/compose.yml"},
			notMatching: []string{"apps/other/compose.yml", "README.md"},
		},
		{
			name:       "nested include",
			workingDir: "stacks/a",
			files: map[string]string{
				"stacks/a/docker-compose.yml": "include:\n  - ../../apps/web/compose.yaml\n",
				"apps/web/compose.yaml":       "include:\n  - db/compose.yaml\nservices:\n  web:\n    image: nginx:1.29-alpine\n",
				"apps/web/db/compose.yaml":    "services:\n  db:\n    image: postgres:18-alpine\n",
				"apps/web/db/unused.yaml":     "services: {}\n",
			},
			matching: []string{
				"stacks/a/docker-compose.yml",
				"apps/web/compose.yaml",
				"apps/web/db/compose.yaml",
			},
			notMatching: []string{"apps/web/db/unused.yaml", "stacks/b/docker-compose.yml"},
		},
		{
			name:       "extends file, also from an included file and chained",
			workingDir: ".",
			files: map[string]string{
				"docker-compose.yml": "include:\n  - apps/web/compose.yaml\n" +
					"services:\n  app:\n    extends:\n      file: base/common.yaml\n      service: base\n",
				"base/common.yaml":      "services:\n  base:\n    image: nginx:1.29-alpine\n",
				"apps/web/compose.yaml": "services:\n  web:\n    extends:\n      file: ../../shared/web.yaml\n      service: web\n",
				"shared/web.yaml": "services:\n  web:\n    extends:\n      file: root.yaml\n      service: root\n" +
					"  other:\n    extends:\n      file: unused.yaml\n      service: unused\n",
				"shared/root.yaml":   "services:\n  root:\n    image: nginx:1.29-alpine\n",
				"shared/unused.yaml": "services:\n  unused:\n    image: nginx:1.29-alpine\n",
			},
			matching: []string{
				"docker-compose.yml",
				"base/common.yaml",
				"apps/web/compose.yaml",
				"shared/web.yaml",
				"shared/root.yaml",
			},
			notMatching: []string{"shared/unused.yaml"},
		},
		{
			name:        "interpolated include path and include env files",
			workingDir:  ".",
			environment: map[string]string{"APP_DIR": "apps/x"},
			files: map[string]string{
				"docker-compose.yml": "include:\n  - path: ${APP_DIR}/compose.yaml\n    env_file: apps/x/include.env\n" +
					"  - apps/y/compose.yaml\n",
				"apps/x/compose.yaml": "services:\n  x:\n    image: nginx:${TAG}\n",
				"apps/x/include.env":  "TAG=1.29-alpine\n",
				"apps/y/compose.yaml": "services:\n  y:\n    image: nginx:${TAG}\n",
				"apps/y/.env":         "TAG=1.29-alpine\n",
			},
			matching: []string{
				"apps/x/compose.yaml",
				"apps/x/include.env",
				"apps/y/compose.yaml",
				"apps/y/.env",
			},
			notMatching: []string{"apps/x/.env", "apps/z/compose.yaml"},
		},
		{
			name:       "env_file /dev/null disables the default .env",
			workingDir: ".",
			files: map[string]string{
				"docker-compose.yml":  "include:\n  - path: apps/x/compose.yaml\n    env_file: /dev/null\n",
				"apps/x/compose.yaml": "services:\n  x:\n    image: nginx:1.29-alpine\n",
				"apps/x/.env":         "TAG=1.29-alpine\n",
			},
			matching:    []string{"apps/x/compose.yaml"},
			notMatching: []string{"apps/x/.env"},
		},
		{
			name:         "includes of every compose file",
			workingDir:   "stack",
			composeFiles: []string{"docker-compose.yml", "docker-compose.prod.yml"},
			files: map[string]string{
				"stack/docker-compose.yml":      "include:\n  - ../apps/web/compose.yaml\n",
				"stack/docker-compose.prod.yml": "include:\n  - ../apps/metrics/compose.yaml\n",
				"apps/web/compose.yaml":         "services:\n  web:\n    image: nginx:1.29-alpine\n",
				"apps/metrics/compose.yaml":     "services:\n  metrics:\n    image: nginx:1.29-alpine\n",
			},
			matching: []string{
				"stack/docker-compose.yml",
				"stack/docker-compose.prod.yml",
				"apps/web/compose.yaml",
				"apps/metrics/compose.yaml",
			},
		},
		{
			name:       "extends of a service behind an inactive profile",
			workingDir: ".",
			files: map[string]string{
				"docker-compose.yml": "services:\n  app:\n    image: nginx:1.29-alpine\n" +
					"  debug:\n    profiles: [debug]\n    extends:\n      file: base/debug.yaml\n      service: debug\n",
				"base/debug.yaml": "services:\n  debug:\n    image: busybox:1.37\n",
			},
			matching: []string{"base/debug.yaml"},
		},
		{
			name:       "extends of a job",
			workingDir: ".",
			files: map[string]string{
				"docker-compose.yml": "services:\n  app:\n    image: nginx:1.29-alpine\n" +
					"jobs:\n  migrate:\n    extends:\n      file: base/jobs.yaml\n      service: migrate\n",
				"base/jobs.yaml": "jobs:\n  migrate:\n    image: busybox:1.37\n    triggers:\n      manual: true\n",
			},
			matching: []string{"base/jobs.yaml"},
		},
		{
			name:       "anchors, merge keys and reset tags",
			workingDir: ".",
			files: map[string]string{
				"docker-compose.yml": "x-web: &web\n  extends:\n    file: base/web.yaml\n    service: web\n" +
					"services:\n  app:\n    <<: *web\n    image: !override nginx:1.29-alpine\n" +
					"  worker:\n    image: nginx:1.29-alpine\n    extends: !reset\n      file: base/unused.yaml\n      service: web\n",
				"base/web.yaml":    "services:\n  web:\n    image: nginx:1.29-alpine\n",
				"base/unused.yaml": "services:\n  web:\n    image: nginx:1.29-alpine\n",
			},
			matching:    []string{"base/web.yaml"},
			notMatching: []string{"base/unused.yaml"},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			repoPath := filepath.Join(t.TempDir(), "repo")
			writeComposeRefFiles(t, repoPath, tc.files)

			workingDir := filepath.Join(repoPath, filepath.FromSlash(tc.workingDir))

			composeFiles := []string{filepath.Join(workingDir, "docker-compose.yml")}
			if len(tc.composeFiles) > 0 {
				composeFiles = nil
				for _, f := range tc.composeFiles {
					composeFiles = append(composeFiles, filepath.Join(workingDir, f))
				}
			}

			project, err := LoadCompose(t.Context(), nil, repoPath, workingDir, "test",
				composeFiles, nil, nil, tc.environment, ComposeLoadOptions{})
			if err != nil {
				t.Fatalf("LoadCompose() error = %v", err)
			}

			filter, err := ProjectPathFilter(repoPath, project)
			if err != nil {
				t.Fatalf("ProjectPathFilter() error = %v", err)
			}

			if filter == nil {
				t.Fatal("expected a filter, got nil")
			}

			for _, p := range tc.matching {
				if !filter(p) {
					t.Errorf("expected %q to match", p)
				}
			}

			for _, p := range tc.notMatching {
				if filter(p) {
					t.Errorf("expected %q not to match", p)
				}
			}
		})
	}
}

// TestComposeFileRefs covers what a loaded project cannot: remote resources, which would be
// fetched, and include cycles, which compose-go rejects.
func TestComposeFileRefs(t *testing.T) {
	testCases := []struct {
		name  string
		files map[string]string
		want  []string
	}{
		{
			name: "remote includes and extends are skipped",
			files: map[string]string{
				"compose.yaml": "include:\n" +
					"  - git@github.com:example/stack.git#main:compose.yaml\n" +
					"  - https://github.com/example/stack.git#main:compose.yaml\n" +
					"  - oci://registry.example.com/stack:latest\n" +
					"  - local.yaml\n" +
					"services:\n  app:\n    extends:\n      file: https://github.com/example/base.git#main:base.yaml\n      service: base\n",
				"local.yaml": "services: {}\n",
			},
			want: []string{"local.yaml"},
		},
		{
			// compose-go resolves the local paths and env files of the entry against the
			// declaring file, but the references inside the included files and the default
			// .env against the project directory, which is the remote copy here.
			name: "remote main file with local overrides",
			files: map[string]string{
				"compose.yaml": "include:\n" +
					"  - path:\n      - oci://registry.example.com/stack:latest\n      - local-override.yaml\n" +
					"    env_file: stack.env\n" +
					"  - path:\n      - oci://registry.example.com/stack:latest\n      - default-env-override.yaml\n",
				"local-override.yaml":       "include:\n  - nested.yaml\n",
				"default-env-override.yaml": "services: {}\n",
				"stack.env":                 "TAG=1\n",
				".env":                      "TAG=1\n",
				"nested.yaml":               "services: {}\n",
			},
			want: []string{"default-env-override.yaml", "local-override.yaml", "stack.env"},
		},
		{
			name: "remote main file with a local project_directory",
			files: map[string]string{
				"compose.yaml": "include:\n" +
					"  - path:\n      - oci://registry.example.com/stack:latest\n      - override.yaml\n" +
					"    project_directory: apps/pd\n",
				"override.yaml":      "include:\n  - inner.yaml\n",
				"apps/pd/.env":       "TAG=1\n",
				"apps/pd/inner.yaml": "services: {}\n",
				"inner.yaml":         "services: {}\n",
			},
			want: []string{"apps/pd/.env", "apps/pd/inner.yaml", "override.yaml"},
		},
		{
			name: "include cycle terminates",
			files: map[string]string{
				"compose.yaml": "include:\n  - a.yaml\n",
				"a.yaml":       "include:\n  - b.yaml\n",
				"b.yaml":       "include:\n  - a.yaml\n",
			},
			want: []string{"a.yaml", "b.yaml"},
		},
		{
			name: "extends cycle and same-file extends terminate",
			files: map[string]string{
				"compose.yaml": "services:\n  app:\n    extends:\n      file: base.yaml\n      service: a\n",
				"base.yaml": "services:\n  a:\n    extends: b\n" +
					"  b:\n    extends:\n      service: c\n" +
					"  c:\n    extends:\n      file: base.yaml\n      service: a\n",
			},
			want: []string{"base.yaml"},
		},
		{
			name: "missing, unparsable and non-compose files are skipped",
			files: map[string]string{
				"compose.yaml": "include:\n  - missing.yaml\n  - broken.yaml\n  - ${UNSET:?required}/x.yaml\n" +
					"services:\n  app:\n    extends:\n      file: missing-base.yaml\n      service: base\n",
				"broken.yaml": "include: [\n",
			},
			want: []string{"broken.yaml"},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeComposeRefFiles(t, root, tc.files)

			project := &types.Project{
				WorkingDir:   root,
				ComposeFiles: []string{filepath.Join(root, "compose.yaml")},
			}

			var got []string

			for _, f := range composeFileRefs(project) {
				rel, err := filepath.Rel(root, f)
				if err != nil {
					t.Fatalf("failed to make %s relative: %v", f, err)
				}

				got = append(got, filepath.ToSlash(rel))
			}

			slices.Sort(got)
			got = slices.Compact(got)

			if !slices.Equal(got, tc.want) {
				t.Errorf("composeFileRefs() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIsRemoteComposeRef(t *testing.T) {
	testCases := map[string]bool{
		"apps/demo/compose.yml":                   false,
		"./compose.yaml":                          false,
		"../shared/base.yaml":                     false,
		"/srv/stack/compose.yaml":                 false,
		"oci://registry.example.com/stack:1":      true,
		"git@github.com:example/stack.git":        true,
		"https://github.com/example/stack.git":    true,
		"https://example.com/compose.yaml":        false,
		"ssh://git@example.com/example/stack.git": true,
	}

	for ref, want := range testCases {
		if got := isRemoteComposeRef(ref); got != want {
			t.Errorf("isRemoteComposeRef(%q) = %v, want %v", ref, got, want)
		}
	}
}
