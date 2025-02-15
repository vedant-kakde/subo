package builder

import (
	"fmt"
	"html/template"
	"io/ioutil"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/pkg/errors"
	"github.com/suborbital/atmo/bundle"
	"github.com/suborbital/atmo/directive"
	"github.com/suborbital/subo/builder/context"
	"github.com/suborbital/subo/subo/release"
	"github.com/suborbital/subo/subo/util"
	"golang.org/x/mod/semver"
)

// Builder is capable of building Wasm modules from source
type Builder struct {
	Context *context.BuildContext

	results []BuildResult

	log util.FriendlyLogger
}

// BuildResult is the results of a build including the built module and logs
type BuildResult struct {
	Succeeded bool
	OutputLog string
}

type Toolchain string

const (
	ToolchainNative = Toolchain("native")
	ToolchainDocker = Toolchain("docker")
)

// ForDirectory creates a Builder bound to a particular directory
func ForDirectory(logger util.FriendlyLogger, dir string) (*Builder, error) {
	ctx, err := context.ForDirectory(dir)
	if err != nil {
		return nil, errors.Wrap(err, "failed to context.FirDirectory")
	}

	b := &Builder{
		Context: ctx,
		results: []BuildResult{},
		log:     logger,
	}

	return b, nil
}

func (b *Builder) BuildWithToolchain(tcn Toolchain) error {
	var err error

	b.results = []BuildResult{}

	// when building in Docker mode, just collect the langs we need to build, and then
	// launch the associated builder images which will do the building
	dockerLangs := map[string]bool{}

	for _, r := range b.Context.Runnables {
		if !b.Context.ShouldBuildLang(r.Runnable.Lang) {
			continue
		}

		if tcn == ToolchainNative {
			b.log.LogStart(fmt.Sprintf("building runnable: %s (%s)", r.Name, r.Runnable.Lang))

			result := &BuildResult{}

			if err := b.checkAndRunPreReqs(r, result); err != nil {
				return errors.Wrap(err, "🚫 failed to checkAndRunPreReqs")
			}

			if flags, err := b.analyzeForCompilerFlags(r); err != nil {
				return errors.Wrap(err, "🚫 failed to analyzeForCompilerFlags")
			} else if flags != "" {
				r.CompilerFlags = flags
			}

			err = b.doNativeBuildForRunnable(r, result)

			// even if there was a failure, load the result into the builder
			// since the logs of the failed build are useful
			b.results = append(b.results, *result)

			if err != nil {
				return errors.Wrapf(err, "🚫 failed to build %s", r.Name)
			}

			fullWasmFilepath := filepath.Join(r.Fullpath, fmt.Sprintf("%s.wasm", r.Name))
			b.log.LogDone(fmt.Sprintf("%s was built -> %s", r.Name, fullWasmFilepath))

		} else {
			dockerLangs[r.Runnable.Lang] = true
		}
	}

	if tcn == ToolchainDocker {
		for lang := range dockerLangs {
			result, err := b.dockerBuildForLang(lang)
			if err != nil {
				return errors.Wrap(err, "failed to dockerBuildForDirectory")
			}

			b.results = append(b.results, *result)
		}
	}

	return nil
}

// Results returns build results for all of the modules built by this builder
// returns os.ErrNotExists if none have been built yet.
func (b *Builder) Results() ([]BuildResult, error) {
	if b.results == nil || len(b.results) == 0 {
		return nil, os.ErrNotExist
	}

	return b.results, nil
}

func (b *Builder) Bundle() error {
	if b.results == nil || len(b.results) == 0 {
		return errors.New("must build before calling Bundle")
	}

	if b.Context.Directive == nil {
		b.Context.Directive = &directive.Directive{
			Identifier: "com.suborbital.app",
			// TODO: insert some git smarts here?
			AppVersion:  "v0.0.1",
			AtmoVersion: fmt.Sprintf("v%s", release.AtmoVersion),
		}
	} else if b.Context.Directive.Headless {
		b.log.LogInfo("updating Directive")

		// bump the appVersion since we're in headless mode
		majorStr := strings.TrimPrefix(semver.Major(b.Context.Directive.AppVersion), "v")
		major, _ := strconv.Atoi(majorStr)
		new := fmt.Sprintf("v%d.0.0", major+1)

		b.Context.Directive.AppVersion = new

		if err := context.WriteDirectiveFile(b.Context.Cwd, b.Context.Directive); err != nil {
			return errors.Wrap(err, "failed to WriteDirectiveFile")
		}
	}

	if err := context.AugmentAndValidateDirectiveFns(b.Context.Directive, b.Context.Runnables); err != nil {
		return errors.Wrap(err, "🚫 failed to AugmentAndValidateDirectiveFns")
	}

	if err := b.Context.Directive.Validate(); err != nil {
		return errors.Wrap(err, "🚫 failed to Validate Directive")
	}

	static, err := context.CollectStaticFiles(b.Context.Cwd)
	if err != nil {
		return errors.Wrap(err, "failed to CollectStaticFiles")
	}

	if static != nil {
		b.log.LogInfo("adding static files to bundle")
	}

	directiveBytes, err := b.Context.Directive.Marshal()
	if err != nil {
		return errors.Wrap(err, "failed to Directive.Marshal")
	}

	modules, err := b.Context.Modules()
	if err != nil {
		return errors.Wrap(err, "failed to Modules for build")
	}

	if err := bundle.Write(directiveBytes, modules, static, b.Context.Bundle.Fullpath); err != nil {
		return errors.Wrap(err, "🚫 failed to WriteBundle")
	}

	return nil
}

func (b *Builder) dockerBuildForLang(lang string) (*BuildResult, error) {
	img := context.ImageForLang(lang, b.Context.BuilderTag)
	if img == "" {
		return nil, fmt.Errorf("%q is not a supported language", lang)
	}

	result := &BuildResult{}

	outputLog, err := util.Run(fmt.Sprintf("docker run --rm --mount type=bind,source=%s,target=/root/runnable %s subo build %s --native --langs %s", b.Context.MountPath, img, b.Context.RelDockerPath, lang))

	result.OutputLog = outputLog

	if err != nil {
		result.Succeeded = false
		return nil, errors.Wrap(err, "failed to Run docker command")
	}

	result.Succeeded = true

	return result, nil
}

// results and resulting file are loaded into the BuildResult pointer
func (b *Builder) doNativeBuildForRunnable(r context.RunnableDir, result *BuildResult) error {
	cmds, err := context.NativeBuildCommands(r.Runnable.Lang)
	if err != nil {
		return errors.Wrap(err, "failed to NativeBuildCommands")
	}

	for _, cmd := range cmds {
		cmdTmpl, err := template.New("cmd").Parse(cmd)
		if err != nil {
			return errors.Wrap(err, "failed to Parse command template")
		}

		fullCmd := &strings.Builder{}
		if err := cmdTmpl.Execute(fullCmd, r); err != nil {
			return errors.Wrap(err, "failed to Execute command template")
		}

		cmdString := strings.TrimSpace(fullCmd.String())

		// Even if the command fails, still load the output into the result object
		outputLog, err := util.RunInDir(cmdString, r.Fullpath)

		result.OutputLog += outputLog + "\n"

		if err != nil {
			result.Succeeded = false
			return errors.Wrap(err, "failed to RunInDir")
		}

		result.Succeeded = true
	}

	return nil
}

func (b *Builder) checkAndRunPreReqs(runnable context.RunnableDir, result *BuildResult) error {
	preReqLangs, ok := context.PreRequisiteCommands[runtime.GOOS]
	if !ok {
		return fmt.Errorf("unsupported OS: %s", runtime.GOOS)
	}

	preReqs, ok := preReqLangs[runnable.Runnable.Lang]
	if !ok {
		return fmt.Errorf("unsupported language: %s", runnable.Runnable.Lang)
	}

	for _, p := range preReqs {
		filepath := filepath.Join(runnable.Fullpath, p.File)

		if _, err := os.Stat(filepath); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				b.log.LogStart(fmt.Sprintf("missing %s, fixing...", p.File))

				outputLog, err := util.RunInDir(p.Command, runnable.Fullpath)

				result.OutputLog += outputLog + "\n"

				if err != nil {
					return errors.Wrapf(err, "failed to Run prerequisite: %s", p.Command)
				}

				b.log.LogDone("fixed!")
			}
		}
	}

	return nil
}

// analyzeForCompilerFlags looks at the Runnable and determines if any additional compiler flags are needed
// this is initially added to support AS-JSON in AssemblyScript with its need for the --transform flag
func (b *Builder) analyzeForCompilerFlags(runnable context.RunnableDir) (string, error) {
	if runnable.Runnable.Lang == "assemblyscript" {
		packageJSONBytes, err := ioutil.ReadFile(filepath.Join(runnable.Fullpath, "package.json"))
		if err != nil {
			return "", errors.Wrap(err, "failed to ReadFile package.json")
		}

		if strings.Contains(string(packageJSONBytes), "json-as") {
			return "--transform ./node_modules/json-as/transform", nil
		}
	}

	return "", nil
}
