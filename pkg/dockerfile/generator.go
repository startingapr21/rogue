package dockerfile

import (
	// blank import for embeds
	_ "embed"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/replicate/cog/pkg/config"
	"github.com/replicate/cog/pkg/util/console"
	"github.com/replicate/cog/pkg/util/slices"
	"github.com/replicate/cog/pkg/util/version"
	"github.com/replicate/cog/pkg/weights"
)

//go:embed embed/cog.whl
var cogWheelEmbed []byte

const DockerignoreHeader = `# generated by replicate/cog
__pycache__
*.pyc
*.pyo
*.pyd
.Python
env
pip-log.txt
pip-delete-this-directory.txt
.tox
.coverage
.coverage.*
.cache
nosetests.xml
coverage.xml
*.cover
*.log
.git
.mypy_cache
.pytest_cache
.hypothesis
`

type Generator struct {
	Config *config.Config
	Dir    string

	// these are here to make this type testable
	GOOS   string
	GOARCH string

	useCudaBaseImage bool
	useCogBaseImage  bool

	// absolute path to tmpDir, a directory that will be cleaned up
	tmpDir string
	// tmpDir relative to Dir
	relativeTmpDir string

	fileWalker weights.FileWalker

	modelDirs  []string
	modelFiles []string

	pythonRequirementsContents string
}

func NewGenerator(config *config.Config, dir string) (*Generator, error) {
	rootTmp := path.Join(dir, ".cog/tmp")
	if err := os.MkdirAll(rootTmp, 0o755); err != nil {
		return nil, err
	}
	// tmpDir ends up being something like dir/.cog/tmp/build20240620123456.000000
	now := time.Now().Format("20060102150405.000000")
	tmpDir, err := os.MkdirTemp(rootTmp, "build"+now)
	if err != nil {
		return nil, err
	}
	// tmpDir, but without dir prefix. This is the path used in the Dockerfile.
	relativeTmpDir, err := filepath.Rel(dir, tmpDir)
	if err != nil {
		return nil, err
	}

	return &Generator{
		Config:           config,
		Dir:              dir,
		GOOS:             runtime.GOOS,
		GOARCH:           runtime.GOOS,
		tmpDir:           tmpDir,
		relativeTmpDir:   relativeTmpDir,
		fileWalker:       filepath.Walk,
		useCudaBaseImage: true,
		useCogBaseImage:  false,
	}, nil
}

func (g *Generator) SetUseCudaBaseImage(argumentValue string) {
	// "false" -> false, "true" -> true, "auto" -> true, "asdf" -> true
	g.useCudaBaseImage = argumentValue != "false"
}

func (g *Generator) SetUseCogBaseImage(useCogBaseImage bool) {
	g.useCogBaseImage = useCogBaseImage
}

func (g *Generator) IsUsingCogBaseImage() bool {
	return g.useCogBaseImage
}

func (g *Generator) generateInitialSteps() (string, error) {
	baseImage, err := g.BaseImage()
	if err != nil {
		return "", err
	}
	installPython, err := g.installPython()
	if err != nil {
		return "", err
	}
	aptInstalls, err := g.aptInstalls()
	if err != nil {
		return "", err
	}
	runCommands, err := g.runCommands()
	if err != nil {
		return "", err
	}

	if g.useCogBaseImage {
		pipInstalls, err := g.pipInstalls()
		if err != nil {
			return "", err
		}
		return joinStringsWithoutLineSpace([]string{
			"#syntax=docker/dockerfile:1.4",
			"FROM " + baseImage,
			aptInstalls,
			pipInstalls,
			runCommands,
		}), nil
	}

	pipInstallStage, err := g.pipInstallStage()
	if err != nil {
		return "", err
	}

	return joinStringsWithoutLineSpace([]string{
		"#syntax=docker/dockerfile:1.4",
		pipInstallStage,
		"FROM " + baseImage,
		g.preamble(),
		g.installTini(),
		installPython,
		aptInstalls,
		g.copyPipPackagesFromInstallStage(),
		runCommands,
	}), nil
}

func (g *Generator) GenerateModelBase() (string, error) {
	initialSteps, err := g.generateInitialSteps()
	if err != nil {
		return "", err
	}
	return strings.Join([]string{
		initialSteps,
		`WORKDIR /src`,
		`EXPOSE 5000`,
		`CMD ["python", "-m", "cog.server.http"]`,
	}, "\n"), nil
}

// GenerateDockerfileWithoutSeparateWeights generates a Dockerfile that doesn't write model weights to a separate layer.
func (g *Generator) GenerateDockerfileWithoutSeparateWeights() (string, error) {
	base, err := g.GenerateModelBase()
	if err != nil {
		return "", err
	}
	return joinStringsWithoutLineSpace([]string{
		base,
		`COPY . /src`,
	}), nil
}

// GenerateModelBaseWithSeparateWeights creates the Dockerfile and .dockerignore file contents for model weights
// It returns four values:
// - weightsBase: The base image used for Dockerfile generation for model weights.
// - dockerfile: A string that represents the Dockerfile content generated by the function.
// - dockerignoreContents: A string that represents the .dockerignore content.
// - err: An error object if an error occurred during Dockerfile generation; otherwise nil.
func (g *Generator) GenerateModelBaseWithSeparateWeights(imageName string) (weightsBase string, dockerfile string, dockerignoreContents string, err error) {
	weightsBase, g.modelDirs, g.modelFiles, err = g.generateForWeights()
	if err != nil {
		return "", "", "", fmt.Errorf("Failed to generate Dockerfile for model weights files: %w", err)
	}
	initialSteps, err := g.generateInitialSteps()
	if err != nil {
		return "", "", "", err
	}

	// Inject weights base image into initial steps so we can COPY from it
	base := []string{}
	initialStepsLines := strings.Split(initialSteps, "\n")
	for i, line := range initialStepsLines {
		if strings.HasPrefix(line, "FROM ") {
			base = append(base, fmt.Sprintf("FROM %s AS %s", imageName+"-weights", "weights"))
			base = append(base, initialStepsLines[i:]...)
			break
		} else {
			base = append(base, line)
		}
	}

	for _, p := range append(g.modelDirs, g.modelFiles...) {
		base = append(base, "COPY --from=weights --link "+path.Join("/src", p)+" "+path.Join("/src", p))
	}

	base = append(base,
		`WORKDIR /src`,
		`EXPOSE 5000`,
		`CMD ["python", "-m", "cog.server.http"]`,
		`COPY . /src`,
	)

	dockerignoreContents = makeDockerignoreForWeights(g.modelDirs, g.modelFiles)
	return weightsBase, joinStringsWithoutLineSpace(base), dockerignoreContents, nil
}

func (g *Generator) generateForWeights() (string, []string, []string, error) {
	modelDirs, modelFiles, err := weights.FindWeights(g.fileWalker)
	if err != nil {
		return "", nil, nil, err
	}
	// generate dockerfile to store these model weights files
	dockerfileContents := `#syntax=docker/dockerfile:1.4
FROM scratch
`
	for _, p := range append(modelDirs, modelFiles...) {
		dockerfileContents += fmt.Sprintf("\nCOPY %s %s", p, path.Join("/src", p))
	}

	return dockerfileContents, modelDirs, modelFiles, nil
}

func makeDockerignoreForWeights(dirs, files []string) string {
	var contents string
	for _, p := range dirs {
		contents += fmt.Sprintf("%[1]s\n%[1]s/**/*\n", p)
	}
	for _, p := range files {
		contents += fmt.Sprintf("%[1]s\n", p)
	}
	return DockerignoreHeader + contents
}

func (g *Generator) Cleanup() error {
	if err := os.RemoveAll(g.tmpDir); err != nil {
		return fmt.Errorf("Failed to clean up %s: %w", g.tmpDir, err)
	}
	return nil
}

func (g *Generator) BaseImage() (string, error) {
	if g.useCogBaseImage {
		var changed bool
		var err error

		cudaVersion := g.Config.Build.CUDA

		pythonVersion := g.Config.Build.PythonVersion
		pythonVersion, changed, err = stripPatchVersion(pythonVersion)
		if err != nil {
			return "", err
		}
		if changed {
			console.Warnf("Stripping patch version from Python version %s to %s", g.Config.Build.PythonVersion, pythonVersion)
		}

		torchVersion, _ := g.Config.TorchVersion()
		torchVersion, changed, err = stripPatchVersion(torchVersion)
		if err != nil {
			return "", err
		}
		if changed {
			console.Warnf("Stripping patch version from Torch version %s to %s", g.Config.Build.PythonVersion, pythonVersion)
		}

		// validate that the base image configuration exists
		imageGenerator, err := NewBaseImageGenerator(cudaVersion, pythonVersion, torchVersion)
		if err != nil {
			return "", err
		}
		baseImage := BaseImageName(imageGenerator.cudaVersion, imageGenerator.pythonVersion, imageGenerator.torchVersion)
		return baseImage, nil
	}

	if g.Config.Build.GPU && g.useCudaBaseImage {
		return g.Config.CUDABaseImageTag()
	}
	return "python:" + g.Config.Build.PythonVersion + "-slim", nil
}

func (g *Generator) preamble() string {
	return `ENV DEBIAN_FRONTEND=noninteractive
ENV PYTHONUNBUFFERED=1
ENV LD_LIBRARY_PATH=$LD_LIBRARY_PATH:/usr/lib/x86_64-linux-gnu:/usr/local/nvidia/lib64:/usr/local/nvidia/bin
ENV NVIDIA_DRIVER_CAPABILITIES=all`
}

func (g *Generator) installTini() string {
	// Install tini as the image entrypoint to provide signal handling and process
	// reaping appropriate for PID 1.
	//
	// N.B. If you remove/change this, consider removing/changing the `has_init`
	// image label applied in image/build.go.
	lines := []string{
		`RUN --mount=type=cache,target=/var/cache/apt,sharing=locked set -eux; \
apt-get update -qq && \
apt-get install -qqy --no-install-recommends curl; \
rm -rf /var/lib/apt/lists/*; \
TINI_VERSION=v0.19.0; \
TINI_ARCH="$(dpkg --print-architecture)"; \
curl -sSL -o /sbin/tini "https://github.com/krallin/tini/releases/download/${TINI_VERSION}/tini-${TINI_ARCH}"; \
chmod +x /sbin/tini`,
		`ENTRYPOINT ["/sbin/tini", "--"]`,
	}
	return strings.Join(lines, "\n")
}

func (g *Generator) aptInstalls() (string, error) {
	packages := g.Config.Build.SystemPackages
	if len(packages) == 0 {
		return "", nil
	}

	if g.useCogBaseImage {
		packages = slices.FilterString(packages, func(pkg string) bool {
			return !slices.ContainsString(baseImageSystemPackages, pkg)
		})
	}

	return "RUN --mount=type=cache,target=/var/cache/apt,sharing=locked apt-get update -qq && apt-get install -qqy " +
		strings.Join(packages, " ") +
		" && rm -rf /var/lib/apt/lists/*", nil
}

func (g *Generator) installPython() (string, error) {
	if g.Config.Build.GPU && g.useCudaBaseImage && !g.useCogBaseImage {
		return g.installPythonCUDA()
	}
	return "", nil
}

func (g *Generator) installPythonCUDA() (string, error) {
	// TODO: check that python version is valid

	py := g.Config.Build.PythonVersion
	return `ENV PATH="/root/.pyenv/shims:/root/.pyenv/bin:$PATH"
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked apt-get update -qq && apt-get install -qqy --no-install-recommends \
	make \
	build-essential \
	libssl-dev \
	zlib1g-dev \
	libbz2-dev \
	libreadline-dev \
	libsqlite3-dev \
	wget \
	curl \
	llvm \
	libncurses5-dev \
	libncursesw5-dev \
	xz-utils \
	tk-dev \
	libffi-dev \
	liblzma-dev \
	git \
	ca-certificates \
	&& rm -rf /var/lib/apt/lists/*
` + fmt.Sprintf(`RUN curl -s -S -L https://raw.githubusercontent.com/pyenv/pyenv-installer/master/bin/pyenv-installer | bash && \
	git clone https://github.com/momo-lab/pyenv-install-latest.git "$(pyenv root)"/plugins/pyenv-install-latest && \
	pyenv install-latest "%s" && \
	pyenv global $(pyenv install-latest --print "%s") && \
	pip install "wheel<1"`, py, py), nil
	// for sitePackagesLocation, kind of need to determine which specific version latest is (3.8 -> 3.8.17 or 3.8.18)
	// install-latest essentially does pyenv install --list | grep $py | tail -1
	// there are many bad options, but a symlink to $(pyenv prefix) is the least bad one
}

func (g *Generator) installCog() (string, error) {
	// Wheel name needs to be full format otherwise pip refuses to install it
	cogFilename := "cog-0.0.1.dev-py3-none-any.whl"
	lines, containerPath, err := g.writeTemp(cogFilename, cogWheelEmbed)
	if err != nil {
		return "", err
	}
	lines = append(lines, fmt.Sprintf("RUN --mount=type=cache,target=/root/.cache/pip pip install -t /dep %s", containerPath))
	return strings.Join(lines, "\n"), nil
}

func (g *Generator) pipInstalls() (string, error) {
	var err error
	excludePackages := []string{}
	if torchVersion, ok := g.Config.TorchVersion(); ok {
		excludePackages = []string{"torch==" + torchVersion}
	}
	if torchvisionVersion, ok := g.Config.TorchvisionVersion(); ok {
		excludePackages = append(excludePackages, "torchvision=="+torchvisionVersion)
	}
	g.pythonRequirementsContents, err = g.Config.PythonRequirementsForArch(g.GOOS, g.GOARCH, excludePackages)
	if err != nil {
		return "", err
	}

	if strings.Trim(g.pythonRequirementsContents, "") == "" {
		return "", nil
	}

	console.Debugf("Generated requirements.txt:\n%s", g.pythonRequirementsContents)
	copyLine, containerPath, err := g.writeTemp("requirements.txt", []byte(g.pythonRequirementsContents))
	if err != nil {
		return "", err
	}

	return strings.Join([]string{
		copyLine[0],
		"RUN pip install -r " + containerPath,
	}, "\n"), nil
}

func (g *Generator) pipInstallStage() (string, error) {
	installCog, err := g.installCog()
	if err != nil {
		return "", err
	}
	g.pythonRequirementsContents, err = g.Config.PythonRequirementsForArch(g.GOOS, g.GOARCH, []string{})
	if err != nil {
		return "", err
	}

	pipStageImage := "python:" + g.Config.Build.PythonVersion
	if strings.Trim(g.pythonRequirementsContents, "") == "" {
		return `FROM ` + pipStageImage + ` as deps
` + installCog, nil
	}

	console.Debugf("Generated requirements.txt:\n%s", g.pythonRequirementsContents)
	copyLine, containerPath, err := g.writeTemp("requirements.txt", []byte(g.pythonRequirementsContents))
	if err != nil {
		return "", err
	}

	// Not slim, so that we can compile wheels
	fromLine := `FROM ` + pipStageImage + ` as deps`
	// Sometimes, in order to run `pip install` successfully, some system packages need to be installed
	// or some other change needs to happen
	// this is a bodge to support that
	// it will be reverted when we add custom dockerfiles
	buildStageDeps := os.Getenv("COG_EXPERIMENTAL_BUILD_STAGE_DEPS")
	if buildStageDeps != "" {
		fromLine = fromLine + "\nRUN " + buildStageDeps
	}
	lines := []string{
		fromLine,
		installCog,
		copyLine[0],
		"RUN --mount=type=cache,target=/root/.cache/pip pip install -t /dep -r " + containerPath,
	}
	return strings.Join(lines, "\n"), nil
}

// copyPipPackagesFromInstallStage copies the Python dependencies installed in the deps stage into the main image
func (g *Generator) copyPipPackagesFromInstallStage() string {
	// placing packages in workdir makes imports faster but seems to break integration tests
	// return "COPY --from=deps --link /dep COPY --from=deps /src"
	// ...except it's actually /root/.pyenv/versions/3.8.17/lib/python3.8/site-packages
	py := g.Config.Build.PythonVersion
	if g.Config.Build.GPU && (g.useCudaBaseImage || g.useCogBaseImage) {
		// this requires buildkit!
		// we should check for buildkit and otherwise revert to symlinks or copying into /src
		// we mount to avoid copying, which avoids having two copies in this layer
		return `
RUN --mount=type=bind,from=deps,source=/dep,target=/dep \
    cp -rf /dep/* $(pyenv prefix)/lib/python*/site-packages; \
    cp -rf /dep/bin/* $(pyenv prefix)/bin; \
    pyenv rehash
`
	}

	return "COPY --from=deps --link /dep /usr/local/lib/python" + py + "/site-packages"
}

func (g *Generator) runCommands() (string, error) {
	runCommands := g.Config.Build.Run

	// For backwards compatibility
	for _, command := range g.Config.Build.PreInstall {
		runCommands = append(runCommands, config.RunItem{Command: command})
	}

	lines := []string{}
	for _, run := range runCommands {
		command := strings.TrimSpace(run.Command)
		if strings.Contains(command, "\n") {
			return "", fmt.Errorf(`One of the commands in 'run' contains a new line, which won't work. You need to create a new list item in YAML prefixed with '-' for each command.

This is the offending line: %s`, command)
		}

		if len(run.Mounts) > 0 {
			mounts := []string{}
			for _, mount := range run.Mounts {
				if mount.Type == "secret" {
					secretMount := fmt.Sprintf("--mount=type=secret,id=%s,target=%s", mount.ID, mount.Target)
					mounts = append(mounts, secretMount)
				}
			}
			lines = append(lines, fmt.Sprintf("RUN %s %s", strings.Join(mounts, " "), command))
		} else {
			lines = append(lines, "RUN "+command)
		}
	}
	return strings.Join(lines, "\n"), nil
}

// writeTemp writes a temporary file that can be used as part of the build process
// It returns the lines to add to Dockerfile to make it available and the filename it ends up as inside the container
func (g *Generator) writeTemp(filename string, contents []byte) ([]string, string, error) {
	path := filepath.Join(g.tmpDir, filename)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return []string{}, "", fmt.Errorf("Failed to write %s: %w", filename, err)
	}
	if err := os.WriteFile(path, contents, 0o644); err != nil {
		return []string{}, "", fmt.Errorf("Failed to write %s: %w", filename, err)
	}
	return []string{fmt.Sprintf("COPY %s /tmp/%s", filepath.Join(g.relativeTmpDir, filename), filename)}, "/tmp/" + filename, nil
}

func joinStringsWithoutLineSpace(chunks []string) string {
	lines := []string{}
	for _, chunk := range chunks {
		chunkLines := strings.Split(chunk, "\n")
		lines = append(lines, chunkLines...)
	}
	return strings.Join(filterEmpty(lines), "\n")
}

func filterEmpty(list []string) []string {
	filtered := []string{}
	for _, s := range list {
		if s != "" {
			filtered = append(filtered, s)
		}
	}
	return filtered
}

func (g *Generator) GenerateWeightsManifest() (*weights.Manifest, error) {
	m := weights.NewManifest()

	for _, dir := range g.modelDirs {
		err := g.fileWalker(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				return nil
			}

			return m.AddFile(path)
		})
		if err != nil {
			return nil, err
		}
	}

	for _, path := range g.modelFiles {
		err := m.AddFile(path)
		if err != nil {
			return nil, err
		}
	}

	return m, nil
}

func stripPatchVersion(versionString string) (string, bool, error) {
	if versionString == "" {
		return "", false, nil
	}

	v, err := version.NewVersion(versionString)
	if err != nil {
		return "", false, fmt.Errorf("Invalid version: %s", versionString)
	}

	strippedVersion := fmt.Sprintf("%d.%d", v.Major, v.Minor)
	changed := strippedVersion != versionString

	return strippedVersion, changed, nil
}
