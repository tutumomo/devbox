// Copyright 2023 Jetpack Technologies Inc and contributors. All rights reserved.
// Use of this source code is governed by the license in the LICENSE file.

package shellgen

import (
	"bufio"
	"bytes"
	"context"
	"embed"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/trace"
	"strings"
	"text/template"

	"github.com/pkg/errors"
	"go.jetpack.io/devbox/internal/boxcli/featureflag"
	"go.jetpack.io/devbox/internal/cuecfg"
	"go.jetpack.io/devbox/internal/debug"
	"go.jetpack.io/devbox/internal/redact"
)

//go:embed tmpl/*
var tmplFS embed.FS

// GenerateForPrintEnv will create all the files necessary for processing
// devbox.PrintEnv, which is the core function from which devbox shell/run/direnv
// functionality is derived.
func GenerateForPrintEnv(ctx context.Context, devbox devboxer) error {
	defer trace.StartRegion(ctx, "GenerateForPrintEnv").End()

	plan, err := newFlakePlan(ctx, devbox)
	if err != nil {
		return err
	}

	outPath := genPath(devbox)

	// Preserving shell.nix to avoid breaking old-style .envrc users
	err = writeFromTemplate(outPath, plan, "shell.nix", "shell.nix")
	if err != nil {
		return errors.WithStack(err)
	}

	// Gitignore file is added to the .devbox directory
	err = writeFromTemplate(filepath.Join(devbox.ProjectDir(), ".devbox"), plan, ".gitignore", ".gitignore")
	if err != nil {
		return errors.WithStack(err)
	}

	if plan.needsGlibcPatch() {
		patch, err := newGlibcPatchFlake(devbox.Config().NixPkgsCommitHash(), plan.Packages)
		if err != nil {
			return redact.Errorf("generate glibc patch flake: %v", err)
		}
		if err := patch.writeTo(filepath.Join(FlakePath(devbox), "glibc-patch")); err != nil {
			return redact.Errorf("write glibc patch flake to directory: %v", err)
		}
	}
	err = makeFlakeFile(devbox, outPath, plan)
	if err != nil {
		return errors.WithStack(err)
	}

	return WriteScriptsToFiles(devbox)
}

// Cache and buffers for generating templated files.
var (
	tmplCache = map[string]*template.Template{}
	tmplBuf   bytes.Buffer
)

func writeFromTemplate(path string, plan any, tmplName, generatedName string) error {
	tmplKey := tmplName + ".tmpl"
	tmpl := tmplCache[tmplKey]
	if tmpl == nil {
		tmpl = template.New(tmplKey)
		tmpl.Funcs(templateFuncs)

		var err error
		glob := "tmpl/" + tmplKey
		tmpl, err = tmpl.ParseFS(tmplFS, glob)
		if err != nil {
			return redact.Errorf("parse embedded tmplFS glob %q: %v", redact.Safe(glob), redact.Safe(err))
		}
		tmplCache[tmplKey] = tmpl
	}
	tmplBuf.Reset()
	if err := tmpl.Execute(&tmplBuf, plan); err != nil {
		return redact.Errorf("execute template %s: %v", redact.Safe(tmplKey), err)
	}

	// In some circumstances, Nix looks at the mod time of a file when
	// caching, so we only want to update the file if something has
	// changed. Blindly overwriting the file could invalidate Nix's cache
	// every time, slowing down evaluation considerably.
	err := overwriteFileIfChanged(filepath.Join(path, generatedName), tmplBuf.Bytes(), 0o644)
	if err != nil {
		return redact.Errorf("write %s to file: %v", redact.Safe(tmplName), err)
	}
	return nil
}

// writeGlibcPatchScript writes the embedded glibc patching script to disk so
// that a generated flake can use it.
func writeGlibcPatchScript(path string) error {
	script, err := fs.ReadFile(tmplFS, "tmpl/glibc-patch.bash")
	if err != nil {
		return redact.Errorf("read embedded glibc-patch.bash: %v", redact.Safe(err))
	}
	err = overwriteFileIfChanged(path, script, 0o755)
	if err != nil {
		return redact.Errorf("write glibc-patch.bash to file: %v", err)
	}
	return nil
}

// overwriteFileIfChanged checks that the contents of f == data, and overwrites
// f if they differ. It also ensures that f's permissions are set to perm.
func overwriteFileIfChanged(path string, data []byte, perm os.FileMode) error {
	flag := os.O_RDWR | os.O_CREATE
	file, err := os.OpenFile(path, flag, perm)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}

		// Definitely a new file if we had to make the directory.
		return os.WriteFile(path, data, perm)
	}
	if err != nil {
		return err
	}
	defer file.Close()

	fi, err := file.Stat()
	if err != nil || fi.Mode().Perm() != perm {
		if err := file.Chmod(perm); err != nil {
			return err
		}
	}

	// Fast path - check if the lengths differ.
	if err == nil && fi.Size() != int64(len(data)) {
		return overwriteFile(file, data, 0)
	}

	r := bufio.NewReader(file)
	for offset := range data {
		b, err := r.ReadByte()
		if err != nil || b != data[offset] {
			return overwriteFile(file, data, offset)
		}
	}
	return nil
}

// overwriteFile truncates f to len(data) and writes data[offset:] beginning at
// the same offset in f.
func overwriteFile(f *os.File, data []byte, offset int) error {
	err := f.Truncate(int64(len(data)))
	if err != nil {
		return err
	}
	_, err = f.WriteAt(data[offset:], int64(offset))
	return err
}

func toJSON(a any) string {
	data, err := cuecfg.MarshalJSON(a)
	if err != nil {
		panic(err)
	}
	return string(data)
}

var templateFuncs = template.FuncMap{
	"json":     toJSON,
	"contains": strings.Contains,
	"debug":    debug.IsEnabled,
}

func makeFlakeFile(d devboxer, outPath string, plan *flakePlan) error {
	flakeDir := FlakePath(d)
	templateName := "flake.nix"
	if featureflag.RemoveNixpkgs.Enabled() {
		templateName = "flake_remove_nixpkgs.nix"
	}
	err := writeFromTemplate(flakeDir, plan, templateName, "flake.nix")
	if err != nil {
		return errors.WithStack(err)
	}

	if !isProjectInGitRepo(outPath) {
		// if we are not in a git repository, then carry on
		return nil
	}
	// if we are in a git repository, then nix requires that the flake.nix file be tracked by git

	// make an empty git repo
	// Alternatively consider: git add intent-to-add path/to/flake.nix, and
	// git update-index --assume-unchanged path/to/flake.nix
	// https://nixos.wiki/wiki/Flakes#How_to_add_a_file_locally_in_git_but_not_include_it_in_commits
	cmd := exec.Command("git", "-C", flakeDir, "init")
	if debug.IsEnabled() {
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
	}
	err = cmd.Run()
	if err != nil {
		return errors.WithStack(err)
	}

	// Any files that flake.nix needs at build time must be in git.
	// Otherwise, Nix won't copy it into the flake's build environment.
	cmd = exec.Command("git", "-C", flakeDir, "add", "flake.nix")
	if plan.needsGlibcPatch() {
		cmd.Args = append(cmd.Args, "glibc-patch/flake.nix")
	}
	if debug.IsEnabled() {
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
	}
	return errors.WithStack(cmd.Run())
}

func isProjectInGitRepo(dir string) bool {
	for dir != "/" {
		// Look for a .git directory in `dir`
		_, err := os.Stat(filepath.Join(dir, ".git"))
		if err == nil {
			// Found a .git
			return true
		}
		if !errors.Is(err, fs.ErrNotExist) {
			// An error means we will not find a git repo so return false
			return false
		}
		// No .git directory found, so loop again into the parent dir
		dir = filepath.Dir(dir)
	}
	// We reached the fs-root dir, climbed the highest mountain and
	// we still haven't found what we're looking for.
	return false
}
