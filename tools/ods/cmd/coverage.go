package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"

	"github.com/onyx-dot-app/onyx/tools/ods/internal/coverage"
	"github.com/onyx-dot-app/onyx/tools/ods/internal/git"
	"github.com/onyx-dot-app/onyx/tools/ods/internal/paths"
	"github.com/onyx-dot-app/onyx/tools/ods/internal/testsuite"
)

// CoverageOptions holds options for the coverage command.
type CoverageOptions struct {
	Check     bool
	Update    bool
	Profile   string
	HTML      string
	Markdown  string
	Tolerance float64
	// Base reports the run against the coverage snapshot of this commit-ish
	// instead of the floors. The gate keeps using the floors.
	Base           string
	Publish        bool
	SnapshotBucket string
}

// NewCoverageCommand creates a command that measures statement coverage for a
// Go suite and compares it against the committed baseline.
func NewCoverageCommand() *cobra.Command {
	opts := &CoverageOptions{}

	cmd := &cobra.Command{
		Use:   "coverage <suite|module-dir>",
		Short: "Measure Go test coverage and hold it against a baseline",
		Long:  coverageHelpDescription(),
		Args:  cobra.ExactArgs(1),
		ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			if len(args) > 0 {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			return testsuite.Names(), cobra.ShellCompDirectiveNoFileComp
		},
		Run: func(cmd *cobra.Command, args []string) {
			if code := runCoverage(args[0], opts); code != 0 {
				os.Exit(code)
			}
		},
	}

	cmd.Flags().BoolVar(&opts.Check, "check", false, "Fail when a package drops below its baseline floor")
	cmd.Flags().BoolVar(&opts.Update, "update", false, "Rewrite the baseline from this run")
	cmd.Flags().StringVar(&opts.Profile, "profile", "", "Keep the coverage profile at this path, for go tool cover -html")
	cmd.Flags().StringVar(&opts.HTML, "html", "", "Render the profile as a browsable page at this path")
	cmd.Flags().StringVar(&opts.Markdown, "markdown", "", "Write the changed packages as a markdown table at this path, for a PR comment")
	cmd.Flags().Float64Var(&opts.Tolerance, "tolerance", coverage.DefaultTolerance,
		"Percentage points a package may drop below its floor without failing")
	cmd.Flags().StringVar(&opts.Base, "base", "",
		"Report against the coverage snapshot of this commit, or its nearest recorded ancestor, instead of the floors")
	cmd.Flags().BoolVar(&opts.Publish, "publish", false,
		"Record this run as the coverage snapshot of HEAD (needs AWS credentials)")
	cmd.Flags().StringVar(&opts.SnapshotBucket, "snapshot-bucket", DefaultS3Bucket,
		"S3 bucket that holds the coverage snapshots")

	return cmd
}

// runCoverage returns the process exit code rather than exiting, so the
// temporary profile directory is always removed on the way out.
func runCoverage(target string, opts *CoverageOptions) int {
	if opts.Check && opts.Update {
		log.Fatal("--check and --update do the opposite of each other; pass only one")
	}
	if opts.Base != "" && opts.Update {
		log.Fatal("--base reports against a snapshot, --update rewrites the floors; pass only one")
	}
	if opts.Publish && opts.Update {
		log.Fatal("--publish records this run as a snapshot, --update rewrites the floors; pass only one")
	}
	if err := coverage.ValidateTolerance(opts.Tolerance); err != nil {
		log.Fatalf("Invalid --tolerance: %v", err)
	}

	root, err := paths.GitRoot()
	if err != nil {
		log.Fatalf("Failed to find git root: %v", err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		log.Fatalf("Failed to determine the working directory: %v", err)
	}

	suite := coverageSuite(root, cwd, target)
	moduleDir := filepath.Join(root, suite.Dir)

	profilePath, cleanup := profileTarget(opts.Profile)
	defer cleanup()

	log.Infof("Measuring %s coverage...", suite.Name)
	profile, err := coverage.Run(coverage.RunOptions{
		ModuleDir:   moduleDir,
		ProfilePath: profilePath,
		Args:        suite.DefaultArgs,
		Stdout:      os.Stdout,
		Stderr:      os.Stderr,
	})
	var exitErr *coverage.ExitError
	if errors.As(err, &exitErr) {
		// The tests failed, and their output is already on the terminal.
		// Coverage from a failed run is not worth reporting.
		return exitErr.Code
	}
	if err != nil {
		log.Errorf("Failed to measure coverage: %v", err)
		return 1
	}

	if opts.HTML != "" {
		htmlPath, err := filepath.Abs(opts.HTML)
		if err != nil {
			log.Errorf("Failed to resolve the html path %q: %v", opts.HTML, err)
			return 1
		}
		if err := coverage.WriteHTML(moduleDir, profilePath, htmlPath); err != nil {
			log.Errorf("Failed to render the html report: %v", err)
			return 1
		}
		log.Infof("HTML report written to %s", htmlPath)
	}

	baselinePath := coverage.BaselinePath(moduleDir)

	if opts.Update {
		return writeBaseline(baselinePath, profile)
	}

	floorReference, code := loadFloorReference(baselinePath, suite.Name)
	if code != 0 {
		return code
	}

	// The gate always compares against the committed floors. --base only
	// changes what the report shows.
	gateReport := coverage.Compare(profile, floorReference, opts.Tolerance)
	report := gateReport

	store := coverage.NewS3SnapshotStore(opts.SnapshotBucket, suite.Dir)
	if opts.Base != "" {
		baseReference, code := locateBaseReference(opts.Base, store)
		if code != 0 {
			return code
		}
		if baseReference != nil {
			report = coverage.Compare(profile, baseReference, opts.Tolerance)
		}
	}

	if err := coverage.WriteReport(os.Stdout, report); err != nil {
		log.Errorf("Failed to write the report: %v", err)
		return 1
	}

	if opts.Markdown != "" {
		if err := writeMarkdown(opts.Markdown, suite.Dir, report); err != nil {
			log.Errorf("Failed to write the markdown report: %v", err)
			return 1
		}
		log.Infof("Markdown report written to %s", opts.Markdown)
	}

	if opts.Profile != "" {
		log.Infof("Coverage profile written to %s", profilePath)
		log.Infof("Browse it with: go tool cover -html=%s", profilePath)
	}

	if improvements := gateReport.Improvements(); len(improvements) > 0 {
		log.Infof("%d package(s) rose above the baseline. Lock the gain in with: ods coverage %s --update",
			len(improvements), suite.Name)
	}

	if code := gateCoverage(opts, suite.Name, baselinePath, floorReference, gateReport); code != 0 {
		return code
	}

	// Publishing runs last: a snapshot describes a run whose tests and gate
	// both passed.
	if opts.Publish {
		return publishSnapshot(store, profile, suite.Dir)
	}
	return 0
}

// loadFloorReference reads the committed floors. A module opts into the gate
// by committing a baseline. Without one the tests still run and the report
// still prints, but nothing can regress.
func loadFloorReference(baselinePath, suiteName string) (*coverage.Reference, int) {
	baseline, err := coverage.LoadBaseline(baselinePath)
	if errors.Is(err, os.ErrNotExist) {
		log.Warnf("No baseline at %s, so nothing is gated. Opt in with: ods coverage %s --update", baselinePath, suiteName)
		return nil, 0
	}
	if err != nil {
		log.Errorf("Failed to read the baseline: %v", err)
		return nil, 1
	}
	return baseline.Reference(), 0
}

// locateBaseReference finds the snapshot to report against. A missing snapshot
// is normal, for example on a fork pull request that holds no credentials, so
// it warns and returns a nil reference to keep the floors.
func locateBaseReference(rev string, store coverage.SnapshotStore) (*coverage.Reference, int) {
	match, err := coverage.LocateBaseSnapshot(rev, coverage.GitCommitHistory{}, store, coverage.DefaultBaseWalkLimit)
	if errors.Is(err, coverage.ErrBaseSnapshotUnavailable) {
		log.Warnf("%v; reporting against the floors", err)
		return nil, 0
	}
	if err != nil {
		log.Errorf("Failed to look up the base coverage snapshot: %v", err)
		return nil, 1
	}

	log.Infof("Reporting against the snapshot of %s", coverage.ShortCommit(match.Commit))
	if match.Distance > 0 {
		log.Infof("The base %s has no snapshot; the nearest recorded ancestor is %d commit(s) back",
			coverage.ShortCommit(match.Base), match.Distance)
	}
	return match.Snapshot.Reference(), 0
}

// gateCoverage fails the run when a package fell below its floor.
func gateCoverage(opts *CoverageOptions, suiteName, baselinePath string, floorReference *coverage.Reference, gateReport *coverage.Report) int {
	if !opts.Check || floorReference == nil {
		return 0
	}
	regressions := gateReport.Regressions()
	if len(regressions) == 0 {
		log.Infof("Coverage holds at or above the baseline in %s", baselinePath)
		return 0
	}
	for _, regression := range regressions {
		log.Errorf("%s fell to %.1f%%, below its %.1f%% floor", regression.Package, regression.Percent, regression.Reference)
	}
	log.Errorf("Coverage regressed in %d package(s). Add tests, or justify the drop and run: ods coverage %s --update",
		len(regressions), suiteName)
	return 1
}

// publishSnapshot records this run as the coverage snapshot of HEAD.
func publishSnapshot(store *coverage.S3SnapshotStore, profile *coverage.Profile, module string) int {
	// A snapshot is keyed by commit, so it must describe that commit alone.
	if git.HasUncommittedChanges() {
		log.Errorf("Refusing to publish a snapshot: the working tree has uncommitted changes")
		return 1
	}
	commit, err := git.ResolveCommit("HEAD")
	if err != nil {
		log.Errorf("Failed to resolve HEAD: %v", err)
		return 1
	}
	if err := store.Publish(coverage.NewSnapshot(profile, commit, module)); err != nil {
		log.Errorf("Failed to publish the coverage snapshot: %v", err)
		return 1
	}
	log.Infof("Published the coverage snapshot of %s to %s", coverage.ShortCommit(commit), store.ObjectURL(commit))
	return 0
}

func writeBaseline(baselinePath string, profile *coverage.Profile) int {
	baseline := coverage.NewBaseline(profile)
	if err := baseline.Save(baselinePath); err != nil {
		log.Errorf("Failed to write the baseline: %v", err)
		return 1
	}
	// Report the floor that was recorded, not the raw measurement, so the
	// number here matches the file.
	log.Infof("Wrote %s with a %.1f%% total coverage floor across %d packages",
		baselinePath, baseline.Total, len(baseline.Packages))
	return 0
}

func writeMarkdown(path, name string, report *coverage.Report) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return coverage.WriteMarkdown(f, name, report)
}

// profileTarget resolves where the coverage profile is written. Without an
// explicit path it goes to a temporary file that is removed afterwards. A
// requested path is made absolute, since go test writes it relative to the
// module directory while we read it relative to the caller's.
func profileTarget(requested string) (string, func()) {
	if requested != "" {
		absolute, err := filepath.Abs(requested)
		if err != nil {
			log.Fatalf("Failed to resolve the profile path %q: %v", requested, err)
		}
		return absolute, func() {}
	}
	dir, err := os.MkdirTemp("", "ods-coverage")
	if err != nil {
		log.Fatalf("Failed to create a temporary directory: %v", err)
	}
	return filepath.Join(dir, "coverage.out"), func() { _ = os.RemoveAll(dir) }
}

// coverageSuite resolves a suite from a suite name or a module directory,
// reusing the routing `ods test` uses. Accepting a directory lets CI pass the
// module it is iterating over without a second name-to-path table.
func coverageSuite(root, cwd, target string) *testsuite.Suite {
	suite, args, err := testsuite.Resolve(root, cwd, []string{target})
	if err != nil {
		log.Fatalf("%v", err)
	}
	// Coverage is measured for a whole module, since a baseline covers every
	// package in it. A path pointing deeper would silently measure less.
	if len(args) > 0 && args[0] != "./..." {
		log.Fatalf("Coverage runs a whole module; %q points inside %s. Use: ods coverage %s",
			target, suite.Dir, suite.Name)
	}
	return suite
}

func coverageHelpDescription() string {
	var b strings.Builder
	b.WriteString(`Measure Go statement coverage and hold it against a committed baseline.

The baseline is a ` + coverage.BaselineFile + ` at the module root recording each
package's floor. --check fails when a package drops below its floor, which is how
CI keeps coverage from regressing. After adding tests, --update raises the floors.

Coverage is per package: a package's number counts only its own tests, so it is a
number that package's owner can act on.

--base reports against the coverage snapshot of a commit, or of its nearest
recorded ancestor, instead of against the floors, which shows what a branch
changed. A missing snapshot only warns: the report falls back to the floors.
--publish records a successful run as the snapshot of HEAD. Neither flag
changes what --check gates on.

Examples:
  ods coverage ods                  # report where each package stands
  ods coverage ods --check          # fail on a regression (what CI runs)
  ods coverage ods --update         # record today's numbers as the new floors
  ods coverage ods --profile /tmp/cover.out
  ods coverage ods --base origin/main   # report what this branch changed

Suites:`)
	for _, suite := range testsuite.All() {
		fmt.Fprintf(&b, "\n  %-12s %s", suite.Name, suite.Short)
	}
	return b.String()
}
