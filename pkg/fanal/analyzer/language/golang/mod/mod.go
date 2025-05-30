package mod

import (
	"context"
	"errors"
	"fmt"
	"go/build"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"unicode"

	"github.com/samber/lo"
	"golang.org/x/xerrors"

	"github.com/aquasecurity/trivy/pkg/dependency/parser/golang/mod"
	"github.com/aquasecurity/trivy/pkg/dependency/parser/golang/sum"
	"github.com/aquasecurity/trivy/pkg/fanal/analyzer"
	"github.com/aquasecurity/trivy/pkg/fanal/analyzer/language"
	"github.com/aquasecurity/trivy/pkg/fanal/types"
	"github.com/aquasecurity/trivy/pkg/licensing"
	"github.com/aquasecurity/trivy/pkg/log"
	"github.com/aquasecurity/trivy/pkg/utils/fsutils"
	xio "github.com/aquasecurity/trivy/pkg/x/io"
	xpath "github.com/aquasecurity/trivy/pkg/x/path"
)

func init() {
	analyzer.RegisterPostAnalyzer(analyzer.TypeGoMod, newGoModAnalyzer)
}

const version = 2

var (
	requiredFiles = []string{
		types.GoMod,
		types.GoSum,
	}
	licenseRegexp = regexp.MustCompile(`^(?i)((UN)?LICEN(S|C)E|COPYING|README|NOTICE).*$`)
)

type gomodAnalyzer struct {
	// root go.mod/go.sum
	modParser language.Parser
	sumParser language.Parser

	// go.mod/go.sum in dependencies
	leafModParser language.Parser

	licenseClassifierConfidenceLevel float64

	logger *log.Logger
}

func newGoModAnalyzer(opt analyzer.AnalyzerOptions) (analyzer.PostAnalyzer, error) {
	return &gomodAnalyzer{
		modParser:                        mod.NewParser(true, opt.DetectionPriority == types.PriorityComprehensive), // Only the root module should replace
		sumParser:                        sum.NewParser(),
		leafModParser:                    mod.NewParser(false, false), // Don't detect stdlib for non-root go.mod files
		licenseClassifierConfidenceLevel: opt.LicenseScannerOption.ClassifierConfidenceLevel,
		logger:                           log.WithPrefix("golang"),
	}, nil
}

func (a *gomodAnalyzer) PostAnalyze(_ context.Context, input analyzer.PostAnalysisInput) (*analyzer.AnalysisResult, error) {
	var apps []types.Application

	required := func(path string, _ fs.DirEntry) bool {
		return filepath.Base(path) == types.GoMod || input.FilePatterns.Match(path)
	}

	err := fsutils.WalkDir(input.FS, ".", required, func(path string, _ fs.DirEntry, _ io.Reader) error {
		// Parse go.mod
		gomod, err := parse(input.FS, path, a.modParser)
		if err != nil {
			return xerrors.Errorf("parse error: %w", err)
		} else if gomod == nil {
			return nil
		}

		if lessThanGo117(gomod) {
			// e.g. /app/go.mod => /app/go.sum
			sumPath := filepath.Join(filepath.Dir(path), types.GoSum)
			gosum, err := parse(input.FS, sumPath, a.sumParser)
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return xerrors.Errorf("parse error: %w", err)
			}
			mergeGoSum(gomod, gosum)
		}

		apps = append(apps, *gomod)
		return nil
	})
	if err != nil {
		return nil, xerrors.Errorf("walk error: %w", err)
	}

	if err = a.fillAdditionalData(input.FS, apps); err != nil {
		a.logger.Warn("Unable to collect additional info", log.Err(err))
	}

	// Add orphan indirect dependencies under the main module
	a.addOrphanIndirectDepsUnderRoot(apps)

	return &analyzer.AnalysisResult{
		Applications: apps,
	}, nil
}

func (a *gomodAnalyzer) Required(filePath string, _ os.FileInfo) bool {
	fileName := filepath.Base(filePath)

	// Save required files (go.mod/go.sum)
	// Note: vendor directory doesn't contain these files, so we can skip checking for this.
	// See: https://github.com/aquasecurity/trivy/issues/8527#issuecomment-2777848027
	if slices.Contains(requiredFiles, fileName) {
		return true
	}

	// Save license files from vendor directory
	if licenseRegexp.MatchString(fileName) && xpath.Contains(filePath, "vendor") {
		return true
	}

	return false
}

func (a *gomodAnalyzer) Type() analyzer.Type {
	return analyzer.TypeGoMod
}

func (a *gomodAnalyzer) Version() int {
	return version
}

// fillAdditionalData collects licenses and dependency relationships, then update applications.
func (a *gomodAnalyzer) fillAdditionalData(fsys fs.FS, apps []types.Application) error {
	gopath := os.Getenv("GOPATH")
	if gopath == "" {
		gopath = build.Default.GOPATH
	}

	// $GOPATH/pkg/mod
	modPath := filepath.Join(gopath, "pkg", "mod")
	gopathModDirFound := fsutils.DirExists(modPath)

	licenses := make(map[string][]string)
	for i, app := range apps {
		// Actually used dependencies
		usedPkgs := lo.SliceToMap(app.Packages, func(pkg types.Package) (string, types.Package) {
			return pkg.Name, pkg
		})

		// vendor directory is in the same directory as go.mod
		vendorDir := filepath.Join(filepath.Dir(app.FilePath), "vendor")

		// Check if the vendor directory exists and is not empty
		entries, err := fs.ReadDir(fsys, vendorDir)
		vendorDirFound := err == nil && len(entries) > 0
		if vendorDirFound {
			a.logger.Debug("Vendor directory found", log.String("path", vendorDir))
			modPath = vendorDir
		}

		if !gopathModDirFound && !vendorDirFound {
			a.logger.Debug("GOPATH and vendor directory not found. Need 'go mod download' or 'go mod vendor' for license scanning",
				log.String("GOPATH", modPath))
			return nil
		}

		for j, lib := range app.Packages {
			if l, ok := licenses[lib.ID]; ok {
				// Fill licenses
				apps[i].Packages[j].Licenses = l
				continue
			}

			// Package dir from `vendor` dir doesn't have version suffix.
			modDirName := normalizeModName(lib.Name)
			if !vendorDirFound {
				// Add version suffix for packages from $GOPATH
				// e.g. $GOPATH/pkg/mod/github.com/aquasecurity/go-dep-parser@v1.0.0
				modDirName = fmt.Sprintf("%s@%s", modDirName, lib.Version)
			}
			modDir := filepath.Join(modPath, modDirName)

			// Collect licenses
			if licenseNames, err := findLicense(fsys, vendorDirFound, modDir, a.licenseClassifierConfidenceLevel); err != nil {
				return xerrors.Errorf("unable to collect license: %w", err)
			} else {
				// Cache the detected licenses
				licenses[lib.ID] = licenseNames

				// Fill licenses
				apps[i].Packages[j].Licenses = licenseNames
			}

			// `vendor` dir doesn't contain `go.mod` file
			// cf. https://github.com/aquasecurity/trivy/issues/8527#issuecomment-2777848027
			if !gopathModDirFound {
				continue
			}

			// Collect dependencies of the direct dependency from $GOPATH/pkg/mod because the vendor directory doesn't have go.mod files.
			dep, err := a.collectDeps(modDir, lib.ID)
			if err != nil {
				return xerrors.Errorf("dependency graph error: %w", err)
			} else if dep.ID == "" {
				// go.mod not found
				continue
			}
			// Filter out unused dependencies and convert module names to module IDs
			apps[i].Packages[j].DependsOn = lo.FilterMap(dep.DependsOn, func(modName string, _ int) (string, bool) {
				m, ok := usedPkgs[modName]
				if !ok {
					return "", false
				}
				return m.ID, true
			})
		}
	}
	return nil
}

func (a *gomodAnalyzer) collectDeps(modDir, pkgID string) (types.Dependency, error) {
	// e.g. $GOPATH/pkg/mod/github.com/aquasecurity/go-dep-parser@v0.0.0-20220406074731-71021a481237/go.mod
	modPath := filepath.Join(modDir, "go.mod")
	f, err := os.Open(modPath)
	if errors.Is(err, fs.ErrNotExist) {
		a.logger.Debug("Unable to identify dependencies as it doesn't support Go modules",
			log.String("module", pkgID))
		return types.Dependency{}, nil
	} else if err != nil {
		return types.Dependency{}, xerrors.Errorf("file open error: %w", err)
	}
	defer f.Close()

	// Parse go.mod under $GOPATH/pkg/mod
	pkgs, _, err := a.leafModParser.Parse(f)
	if err != nil {
		return types.Dependency{}, xerrors.Errorf("%s parse error: %w", modPath, err)
	}

	// Filter out indirect dependencies
	dependsOn := lo.FilterMap(pkgs, func(lib types.Package, _ int) (string, bool) {
		return lib.Name, lib.Relationship == types.RelationshipDirect
	})

	return types.Dependency{
		ID:        pkgID,
		DependsOn: dependsOn,
	}, nil
}

// addOrphanIndirectDepsUnderRoot handles indirect dependencies that have no identifiable parent packages in the dependency tree.
// This situation can occur when:
// - $GOPATH/pkg directory doesn't exist
// - Module cache is incomplete
// - etc.
//
// In such cases, indirect packages become "orphaned" - they exist in the dependency list
// but have no connection to the dependency tree. This function resolves this issue by:
// 1. Finding the root (main) module
// 2. Identifying all indirect dependencies that have no parent packages
// 3. Adding these orphaned indirect dependencies under the main module
//
// This ensures that all packages remain visible in the dependency tree, even when the complete
// dependency chain cannot be determined.
func (a *gomodAnalyzer) addOrphanIndirectDepsUnderRoot(apps []types.Application) {
	for _, app := range apps {
		// Find the main module
		_, rootIdx, found := lo.FindIndexOf(app.Packages, func(pkg types.Package) bool {
			return pkg.Relationship == types.RelationshipRoot
		})
		if !found {
			continue
		}

		// Collect all orphan indirect dependencies that are unable to identify parents
		parents := app.Packages.ParentDeps()
		orphanDeps := lo.FilterMap(app.Packages, func(pkg types.Package, _ int) (string, bool) {
			return pkg.ID, pkg.Relationship == types.RelationshipIndirect && len(parents[pkg.ID]) == 0
		})
		// Add orphan indirect dependencies under the main module
		app.Packages[rootIdx].DependsOn = append(app.Packages[rootIdx].DependsOn, orphanDeps...)
	}
}

func parse(fsys fs.FS, path string, parser language.Parser) (*types.Application, error) {
	f, err := fsys.Open(path)
	if err != nil {
		return nil, xerrors.Errorf("file open error: %w", err)
	}
	defer f.Close()

	file, ok := f.(xio.ReadSeekCloserAt)
	if !ok {
		return nil, xerrors.Errorf("type assertion error: %w", err)
	}

	// Parse go.mod or go.sum
	return language.Parse(types.GoModule, path, file, parser)
}

func lessThanGo117(gomod *types.Application) bool {
	for _, lib := range gomod.Packages {
		// The indirect field is populated only in Go 1.17+
		if lib.Relationship == types.RelationshipIndirect {
			return false
		}
	}
	return true
}

func mergeGoSum(gomod, gosum *types.Application) {
	if gomod == nil || gosum == nil {
		return
	}
	uniq := make(map[string]types.Package)
	for _, lib := range gomod.Packages {
		// It will be used for merging go.sum.
		uniq[lib.Name] = lib
	}

	// For Go 1.16 or less, we need to merge go.sum into go.mod.
	for _, lib := range gosum.Packages {
		// Skip dependencies in go.mod so that go.mod should be preferred.
		if _, ok := uniq[lib.Name]; ok {
			continue
		}

		// This dependency doesn't exist in go.mod, so it must be an indirect dependency.
		lib.Indirect = true
		lib.Relationship = types.RelationshipIndirect
		uniq[lib.Name] = lib
	}

	gomod.Packages = lo.Values(uniq)
}

func findLicense(fsys fs.FS, vendorDirFound bool, dir string, classifierConfidenceLevel float64) ([]string, error) {
	var license *types.LicenseFile

	open := func(fsys fs.FS, path string) (fs.File, error) {
		if vendorDirFound {
			return fsys.Open(path)
		}
		return os.Open(path)
	}

	walkDirFunc := func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		} else if !d.Type().IsRegular() {
			return nil
		}

		// For `vendor`, the `fsys` directory contains only license files, so we don't need to check the file name again.
		if !vendorDirFound {
			if !licenseRegexp.MatchString(filepath.Base(path)) {
				return nil
			}
		}

		// e.g. $GOPATH/pkg/mod/github.com/aquasecurity/go-dep-parser@v0.0.0-20220406074731-71021a481237/LICENSE
		f, err := open(fsys, path)
		if err != nil {
			return xerrors.Errorf("file (%s) open error: %w", path, err)
		}
		defer f.Close()

		l, err := licensing.Classify(path, f, classifierConfidenceLevel)
		if err != nil {
			return xerrors.Errorf("license classify error: %w", err)
		}
		// License found
		if l != nil && len(l.Findings) > 0 {
			license = l
		}
		return nil
	}

	var err error
	if vendorDirFound {
		err = fs.WalkDir(fsys, dir, walkDirFunc)
	} else {
		err = filepath.WalkDir(dir, walkDirFunc)
	}

	switch {
	// The module path may not exist
	case errors.Is(err, os.ErrNotExist):
		return nil, nil
	case err != nil && !errors.Is(err, io.EOF):
		return nil, fmt.Errorf("finding a known open source license: %w", err)
	case license == nil || len(license.Findings) == 0:
		return nil, nil
	}

	return license.Findings.Names(), nil
}

// normalizeModName escapes upper characters
// e.g. 'github.com/BurntSushi/toml' => 'github.com/!burnt!sushi'
func normalizeModName(name string) string {
	var newName []rune
	for _, c := range name {
		if unicode.IsUpper(c) {
			// 'A' => '!a'
			newName = append(newName, '!', unicode.ToLower(c))
		} else {
			newName = append(newName, c)
		}
	}
	return string(newName)
}
