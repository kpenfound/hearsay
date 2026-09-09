// A Dagger module for Hearsay.
//
// Everything that runs code runs here: lint, the tests, a real Postgres with
// pgvector for the ones that need a database, the binary, the container image,
// migrations and the local dev stack. CI calls these functions and does nothing
// else, so a green pipeline and a green laptop mean the same thing.
package main

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"dagger/hearsay/internal/dagger"
)

const (
	// postgresImage must be the pgvector build, not stock Postgres: ADR-0004
	// puts the embedding column on the L1 table, so a plain server fails on the
	// first migration. Postgres 16 is the minimum that ADR sets.
	postgresImage = "pgvector/pgvector:pg16"

	// golangciImage pins the same linter version CONTRIBUTING.md tells a
	// contributor to match. It carries a Go toolchain of its own, which may be
	// older than the one go.mod asks for; GOTOOLCHAIN=auto fetches the right
	// one into the shared module cache.
	golangciImage = "golangci/golangci-lint:v2.13.2"

	// runtimeImage is the base of the image hearsay ships in. The binary is
	// static (CGO_ENABLED=0), so this only has to supply certificates and
	// timezones.
	runtimeImage = "alpine:3.22"

	workdir    = "/src"
	binaryPath = "/out/hearsay"

	// versionPkg is where the build identity is stamped. Keep it in step with
	// internal/version.
	versionPkg = "github.com/kpenfound/hearsay/internal/version"

	// The database the test and dev stacks use. Local throwaways, so the
	// credentials are not a secret and are the same everywhere.
	pgUser     = "hearsay"
	pgPassword = "hearsay"
	pgDatabase = "hearsay"
	pgHost     = "postgres"

	// databaseEnv is how the binary will be told which database to use. Config
	// is #37's; when it names this variable something else, this is the line
	// that changes.
	databaseEnv = "HEARSAY_DATABASE_URL"

	// apiPort is the port the API service will listen on. Nothing listens yet —
	// see Dev.
	apiPort = 8080
)

// Hearsay is the module's entry point.
type Hearsay struct {
	// The repository to run against.
	Source *dagger.Directory
}

func New(
	// The repository root.
	//
	// +defaultPath="/"
	// +ignore=[".git", ".bees", ".dagger", "dist", "hearsay"]
	source *dagger.Directory,
) *Hearsay {
	return &Hearsay{Source: source}
}

// Lint runs go vet, golangci-lint and the formatter check.
func (h *Hearsay) Lint(ctx context.Context) (string, error) {
	base, err := h.goBase(ctx)
	if err != nil {
		return "", err
	}
	vet, err := base.WithExec([]string{"go", "vet", "./..."}).Stdout(ctx)
	if err != nil {
		return "", fmt.Errorf("go vet: %w", err)
	}

	lint, err := h.lintBase().WithExec([]string{"golangci-lint", "run"}).Stdout(ctx)
	if err != nil {
		return "", fmt.Errorf("golangci-lint run: %w", err)
	}

	// Formatting is part of the lint gate, not a separate ritual: --diff prints
	// what `golangci-lint fmt` would change and fails if there is anything.
	format, err := h.lintBase().WithExec([]string{"golangci-lint", "fmt", "--diff"}).Stdout(ctx)
	if err != nil {
		return "", fmt.Errorf("golangci-lint fmt --diff: %w", err)
	}

	return vet + lint + format + "lint ok\n", nil
}

// TidyCheck fails if `go mod tidy` would change go.mod or go.sum.
func (h *Hearsay) TidyCheck(ctx context.Context) (string, error) {
	base, err := h.goBase(ctx)
	if err != nil {
		return "", err
	}
	// go.sum does not exist while the module has no dependencies, so both
	// sides of its comparison are allowed to be missing.
	script := `set -e
cp go.mod /tmp/go.mod.before
if [ -f go.sum ]; then cp go.sum /tmp/go.sum.before; else : > /tmp/go.sum.before; fi
go mod tidy
diff -u /tmp/go.mod.before go.mod
if [ -f go.sum ]; then diff -u /tmp/go.sum.before go.sum; else diff -u /tmp/go.sum.before /dev/null; fi
echo "go.mod and go.sum are tidy"`
	out, err := base.WithExec([]string{"sh", "-c", script}).Stdout(ctx)
	if err != nil {
		return "", fmt.Errorf("go mod tidy would change go.mod or go.sum: %w", err)
	}
	return out, nil
}

// UnitTest runs the tests that need nothing but the module.
func (h *Hearsay) UnitTest(ctx context.Context) (string, error) {
	base, err := h.goBase(ctx)
	if err != nil {
		return "", err
	}
	out, err := base.WithExec([]string{"go", "test", "-race", "./..."}).Stdout(ctx)
	if err != nil {
		return "", fmt.Errorf("go test: %w", err)
	}
	return out, nil
}

// IntegrationTest runs the tests tagged `integration` against a real Postgres
// with pgvector.
func (h *Hearsay) IntegrationTest(ctx context.Context) (string, error) {
	db := h.Postgres()

	// The suite has no integration tests yet, so a run of zero tests passing is
	// no evidence that the harness works. Prove the database first: CREATE
	// EXTENSION fails on a Postgres image without pgvector, which is the
	// mistake ADR-0004 warns about.
	check, err := dag.Container().
		From(postgresImage).
		WithServiceBinding(pgHost, db).
		WithEnvVariable("PGPASSWORD", pgPassword).
		WithExec([]string{
			"psql", "-h", pgHost, "-U", pgUser, "-d", pgDatabase,
			"-v", "ON_ERROR_STOP=1",
			"-c", "CREATE EXTENSION IF NOT EXISTS vector",
			"-c", "SELECT extname, extversion FROM pg_extension WHERE extname = 'vector'",
		}).
		Stdout(ctx)
	if err != nil {
		return "", fmt.Errorf("pgvector is not usable on %s: %w", postgresImage, err)
	}

	// ADR-0006 puts `hearsay migrate up` between the database and the tests, so
	// that a missing migration fails CI rather than being papered over by a
	// fixture. The subcommand still refuses (there is nothing to migrate), so
	// the step goes in with the L0 store.
	base, err := h.goBase(ctx)
	if err != nil {
		return "", err
	}
	out, err := base.
		WithServiceBinding(pgHost, db).
		WithEnvVariable(databaseEnv, databaseURL()).
		WithExec([]string{"go", "test", "-race", "-tags=integration", "./..."}).
		Stdout(ctx)
	if err != nil {
		return "", fmt.Errorf("go test -tags=integration: %w", err)
	}
	return check + out, nil
}

// Test runs the whole suite: the unit tests, and the integration tests against a
// real Postgres with pgvector.
func (h *Hearsay) Test(ctx context.Context) (string, error) {
	unit, err := h.UnitTest(ctx)
	if err != nil {
		return "", err
	}
	integration, err := h.IntegrationTest(ctx)
	if err != nil {
		return "", err
	}
	return unit + integration, nil
}

// Build compiles the hearsay binary for Linux.
func (h *Hearsay) Build(ctx context.Context,
	// Target architecture (amd64, arm64). Defaults to the engine's own.
	//
	// +optional
	arch string,
	// Version to stamp into the binary. Defaults to what internal/version works
	// out for itself.
	//
	// +optional
	version string,
) (*dagger.File, error) {
	base, err := h.goBase(ctx)
	if err != nil {
		return nil, err
	}
	ldflags := []string{"-s", "-w"}
	if version != "" {
		ldflags = append(ldflags, "-X", versionPkg+".version="+version)
	}

	ctr := base.
		WithEnvVariable("CGO_ENABLED", "0").
		WithEnvVariable("GOOS", "linux").
		// go build does not create the directory it is told to write into.
		WithDirectory("/out", dag.Directory())
	if arch != "" {
		ctr = ctr.WithEnvVariable("GOARCH", arch)
	}
	return ctr.WithExec([]string{
		"go", "build", "-trimpath",
		"-ldflags", strings.Join(ldflags, " "),
		"-o", binaryPath, "./cmd/hearsay",
	}).File(binaryPath), nil
}

// Image builds the container image hearsay ships in. One image runs all four
// services; the subcommand is the argument (ADR-0003).
func (h *Hearsay) Image(ctx context.Context,
	// Target architecture (amd64, arm64). Defaults to the engine's own.
	//
	// +optional
	arch string,
	// Version to stamp into the binary.
	//
	// +optional
	version string,
) (*dagger.Container, error) {
	bin, err := h.Build(ctx, arch, version)
	if err != nil {
		return nil, err
	}

	opts := dagger.ContainerOpts{}
	if arch != "" {
		opts.Platform = dagger.Platform("linux/" + arch)
	}
	return dag.Container(opts).
		From(runtimeImage).
		WithExec([]string{"apk", "add", "--no-cache", "ca-certificates", "tzdata"}).
		WithFile("/usr/local/bin/hearsay", bin).
		WithUser("nobody").
		WithEntrypoint([]string{"/usr/local/bin/hearsay"}).
		// The base image's CMD would otherwise be handed to hearsay as a
		// subcommand. With no default the image prints its usage and exits
		// non-zero, which is the right answer to "run hearsay" with no service.
		WithoutDefaultArgs(), nil
}

// Postgres starts a throwaway Postgres with pgvector, the database the
// integration tests and the dev stack run against.
//
//	dagger call postgres up --ports 5432:5432
func (h *Hearsay) Postgres() *dagger.Service {
	return dag.Container().
		From(postgresImage).
		WithEnvVariable("POSTGRES_USER", pgUser).
		WithEnvVariable("POSTGRES_PASSWORD", pgPassword).
		WithEnvVariable("POSTGRES_DB", pgDatabase).
		WithExposedPort(5432).
		AsService(dagger.ContainerAsServiceOpts{UseEntrypoint: true})
}

// Migrate applies the schema migrations to a database (ADR-0006).
//
//	dagger call migrate --database-url=env:HEARSAY_DATABASE_URL
//
// Every action exits non-zero with "not implemented yet" until goose and the
// embedded migrations land with the L0 store.
func (h *Hearsay) Migrate(ctx context.Context,
	// Postgres connection URL, for example
	// postgres://hearsay:hearsay@localhost:5432/hearsay.
	databaseURL *dagger.Secret,
	// up, status, up-to or down.
	//
	// +optional
	// +default="up"
	action string,
	// The target version, for up-to.
	//
	// +optional
	arg string,
) (string, error) {
	// ADR-0006 runs migrations as a job from the image that is being deployed,
	// so this runs the same image rather than a container of its own.
	img, err := h.Image(ctx, "", "")
	if err != nil {
		return "", err
	}
	args := []string{"migrate", action}
	if arg != "" {
		args = append(args, arg)
	}
	out, err := img.
		WithSecretVariable(databaseEnv, databaseURL).
		WithExec(args, dagger.ContainerWithExecOpts{UseEntrypoint: true}).
		Stdout(ctx)
	if err != nil {
		return "", fmt.Errorf("hearsay migrate %s: %w", action, err)
	}
	return out, nil
}

// Dev runs the local stack: Postgres with pgvector, and all four services in one
// process (ADR-0003's `hearsay all`).
//
//	dagger call dev up
//
// There is no file watcher. Restarting is the reload: stop the command and run
// it again, and the rebuild is a cached one.
func (h *Hearsay) Dev(ctx context.Context) (*dagger.Service, error) {
	img, err := h.Image(ctx, "", "")
	if err != nil {
		return nil, err
	}
	return img.
		WithServiceBinding(pgHost, h.Postgres()).
		WithEnvVariable(databaseEnv, databaseURL()).
		// The API is a stub and listens on nothing, so the port is here for the
		// tunnel rather than for a server. Drop the health check exemption when
		// the API grows a listener.
		WithExposedPort(apiPort, dagger.ContainerWithExposedPortOpts{
			Description:                 "hearsay API (HTTP and MCP)",
			ExperimentalSkipHealthcheck: true,
		}).
		AsService(dagger.ContainerAsServiceOpts{
			UseEntrypoint: true,
			Args:          []string{"all", "--log-level", "debug", "--log-format", "text"},
		}), nil
}

// Check runs everything CI runs: lint, the tidy check, the whole test suite and
// the image build. It is the one command a pull request has to pass.
func (h *Hearsay) Check(ctx context.Context) (string, error) {
	var out strings.Builder

	steps := []struct {
		name string
		run  func(context.Context) (string, error)
	}{
		{"lint", h.Lint},
		{"tidy", h.TidyCheck},
		{"test", h.Test},
	}
	for _, step := range steps {
		res, err := step.run(ctx)
		if err != nil {
			return out.String(), fmt.Errorf("%s: %w", step.name, err)
		}
		fmt.Fprintf(&out, "=== %s ===\n%s\n", step.name, res)
	}

	img, err := h.Image(ctx, "", "")
	if err != nil {
		return out.String(), fmt.Errorf("image: %w", err)
	}
	if _, err := img.Sync(ctx); err != nil {
		return out.String(), fmt.Errorf("image: %w", err)
	}
	fmt.Fprint(&out, "=== image ===\nbuilt\n")

	return out.String(), nil
}

// goBase is the container every Go command runs in: the toolchain go.mod asks
// for, the caches, and the source at /src.
func (h *Hearsay) goBase(ctx context.Context) (*dagger.Container, error) {
	version, err := h.goVersion(ctx)
	if err != nil {
		return nil, err
	}
	return h.withGoCaches(dag.Container().From("golang:"+version)).
		WithMountedDirectory(workdir, h.Source).
		WithWorkdir(workdir), nil
}

// lintBase is golangci-lint's own image, sharing the Go caches so that the
// toolchain it fetches is fetched once.
func (h *Hearsay) lintBase() *dagger.Container {
	return h.withGoCaches(dag.Container().From(golangciImage)).
		WithEnvVariable("GOLANGCI_LINT_CACHE", "/cache/golangci-lint").
		WithMountedCache("/cache/golangci-lint", dag.CacheVolume("hearsay-golangci-lint")).
		WithMountedDirectory(workdir, h.Source).
		WithWorkdir(workdir)
}

func (h *Hearsay) withGoCaches(ctr *dagger.Container) *dagger.Container {
	return ctr.
		WithEnvVariable("GOPATH", "/go").
		WithEnvVariable("GOCACHE", "/cache/go-build").
		WithEnvVariable("GOTOOLCHAIN", "auto").
		WithMountedCache("/go/pkg/mod", dag.CacheVolume("hearsay-go-mod")).
		WithMountedCache("/cache/go-build", dag.CacheVolume("hearsay-go-build"))
}

// goDirective matches the `go` line of a go.mod.
var goDirective = regexp.MustCompile(`(?m)^go\s+(\d+\.\d+(?:\.\d+)?)\s*$`)

// goVersion is the toolchain go.mod asks for. Reading it rather than pinning it
// here is what keeps the containers from drifting from the module: raising the
// floor stays the one-line commit CONTRIBUTING.md describes.
func (h *Hearsay) goVersion(ctx context.Context) (string, error) {
	mod, err := h.Source.File("go.mod").Contents(ctx)
	if err != nil {
		return "", fmt.Errorf("reading go.mod: %w", err)
	}
	m := goDirective.FindStringSubmatch(mod)
	if m == nil {
		return "", errors.New("go.mod has no go directive")
	}
	return m[1], nil
}

// databaseURL is the connection string for the Postgres this module starts,
// reachable under the service binding alias.
func databaseURL() string {
	return fmt.Sprintf("postgres://%s:%s@%s:5432/%s?sslmode=disable", pgUser, pgPassword, pgHost, pgDatabase)
}
