// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package query

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/google/subcommands"
	"golang.org/x/sync/errgroup"
	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/hashfs"
	fspb "go.chromium.org/build/siso/hashfs/proto"
	"go.chromium.org/build/siso/o11y/clog"
	"go.chromium.org/build/siso/scandeps"
	pb "go.chromium.org/build/siso/toolsupport/ciderutil/proto"
	"go.chromium.org/build/siso/toolsupport/gccutil"
	"go.chromium.org/build/siso/toolsupport/makeutil"
	"go.chromium.org/build/siso/toolsupport/ninjautil"
	"go.chromium.org/build/siso/toolsupport/shutil"
	"go.chromium.org/build/siso/ui"
)

// go/reqs-for-peep

type ideAnalysisCommand struct {
	execRoot string
	dir      string
	stateDir string
	fname    string
	fsopt    *hashfs.Option
	format   string
	output   string
}

const ideAnalysisUsage = `query ninja build graph for Cider-G.

 $ siso query ideanalysis -C <dir> [--format <format>] [--output <output>] [targets...]

format: proto, prototext or json
output: pathname. "" or "-" is stdout.
`

func (*ideAnalysisCommand) Name() string {
	return "ideanalysis"
}

func (*ideAnalysisCommand) Synopsis() string {
	return "query ninja build graph ofor Cider-G"
}

func (*ideAnalysisCommand) Usage() string {
	return ideAnalysisUsage
}
func (c *ideAnalysisCommand) SetFlags(flagSet *flag.FlagSet) {
	flagSet.StringVar(&c.dir, "C", ".", "ninja running directory to find build.ninja")
	flagSet.StringVar(&c.stateDir, "state_dir", ".", "state directory (relative to -C)")
	flagSet.StringVar(&c.fname, "f", "build.ninja", "input build filename (relative to -C)")
	c.fsopt = new(hashfs.Option)
	c.fsopt.StateFile = ".siso_fs_state"
	c.fsopt.RegisterFlags(flagSet)
	flagSet.StringVar(&c.format, "format", "proto", `output format. "proto", "prototext" or "json"`)
	flagSet.StringVar(&c.output, "output", "", `output path. "" or "-" is stdout`)
}

func (c *ideAnalysisCommand) Execute(ctx context.Context, flagSet *flag.FlagSet, _ ...any) subcommands.ExitStatus {
	err := c.run(ctx, flagSet.Args())
	if err != nil {
		switch {
		case errors.Is(err, flag.ErrHelp):
			fmt.Fprintf(os.Stderr, "%v\n%s\n", err, ideAnalysisUsage)
			return subcommands.ExitUsageError
		default:
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return subcommands.ExitFailure
		}
	}
	return subcommands.ExitSuccess
}

func (c *ideAnalysisCommand) run(ctx context.Context, args []string) error {
	started := time.Now()
	switch c.format {
	case "proto", "prototext", "json":
	default:
		return fmt.Errorf(`unknown format %q: "proto", "prototext" or "json": %w`, c.format, flag.ErrHelp)
	}

	// TODO: use ninja's initWorkdirs?
	if c.fsopt.StateFile != "" {
		c.fsopt.StateFile = filepath.Join(c.stateDir, c.fsopt.StateFile)
	}

	// don't use $PWD for current directory
	// to avoid symlink issue. b/286779149
	pwd := os.Getenv("PWD")
	_ = os.Unsetenv("PWD") // no error for safe env key name.

	var err error
	c.execRoot, err = os.Getwd()
	if pwd != "" {
		_ = os.Setenv("PWD", pwd) // no error to reset env with valid value.
	}
	if err != nil {
		return err
	}

	analysis, err := c.analyze(ctx, args)
	if err != nil {
		analysis.Error = &pb.AnalysisError{
			ErrorMessage: err.Error(),
		}
	}
	var buf []byte
	switch c.format {
	case "proto":
		buf, err = proto.Marshal(analysis)
	case "prototext":
		buf, err = prototext.MarshalOptions{
			Multiline: true,
			Indent:    " ",
		}.Marshal(analysis)
	case "json":
		buf, err = json.MarshalIndent(analysis, "", " ")
	}
	if err != nil {
		return err
	}
	output := c.output
	switch c.output {
	case "", "-":
		output = "/dev/stdout"
		_, err = os.Stdout.Write(buf)
	default:
		err = os.WriteFile(output, buf, 0644)
	}
	fmt.Fprintf(os.Stderr, "siso query ideanalysis to %s in %s: %v\n", output, ui.FormatDuration(time.Since(started)), err)
	return err
}

func (c *ideAnalysisCommand) analyze(ctx context.Context, args []string) (*pb.IdeAnalysis, error) {
	analysis := &pb.IdeAnalysis{
		BuildOutDir: c.dir,
		WorkingDir:  c.dir,
	}
	if len(args) == 0 {
		return analysis, errors.New("no target given")
	}
	wd, err := os.Getwd()
	if err != nil {
		return analysis, err
	}
	err = os.Chdir(c.dir)
	if err != nil {
		return analysis, err
	}
	defer os.Chdir(wd)
	analyzer := &ideAnalyzer{
		path: build.NewPath(c.execRoot, c.dir),
	}
	defer analyzer.Close(ctx)
	hashFS, err := hashfs.New(ctx, hashfs.Option{})
	if err != nil {
		return analysis, err
	}
	analyzer.hashFS = hashFS

	eg, gctx := errgroup.WithContext(ctx)
	eg.Go(func() (finalErr error) {
		started := time.Now()
		defer func() {
			clog.Infof(gctx, "hashfs in %s: %v", time.Since(started), finalErr)
		}()
		fsstate, err := hashfs.Load(gctx, hashfs.Option{StateFile: c.fsopt.StateFile})
		if err != nil {
			return err
		}
		// hashFS.SetState ?
		analyzer.fsm = hashfs.StateMap(fsstate)
		fmt.Fprintf(os.Stderr, "load hashfs state in %s\n", ui.FormatDuration(time.Since(started)))
		return nil
	})

	eg.Go(func() error {
		started := time.Now()
		analyzer.state = ninjautil.NewState()
		p := ninjautil.NewManifestParser(analyzer.state)
		err := p.Load(gctx, c.fname)
		clog.Infof(gctx, "ninja build in %s: %v", time.Since(started), err)
		fmt.Fprintf(os.Stderr, "load ninja build in %s\n", ui.FormatDuration(time.Since(started)))
		return err
	})
	err = eg.Wait()
	if err != nil {
		return analysis, err
	}
	analyzer.scanDeps = scandeps.New(hashFS, nil, nil)
	buildableUnits := make(map[string]*pb.BuildableUnit)
	for _, arg := range args {
		result, bus := analyzer.analyzeTarget(ctx, arg)
		analysis.Results = append(analysis.Results, result)
		maps.Copy(buildableUnits, bus)
	}
	var buIDs []string
	for k := range buildableUnits {
		buIDs = append(buIDs, k)
	}
	sort.Strings(buIDs)
	for _, id := range buIDs {
		analysis.Units = append(analysis.Units, buildableUnits[id])
	}
	return analysis, nil
}

type ideAnalyzer struct {
	path     *build.Path
	hashFS   *hashfs.HashFS
	fsm      map[string]*fspb.Entry
	state    *ninjautil.State
	scanDeps *scandeps.ScanDeps
}

func (a *ideAnalyzer) Close(ctx context.Context) {
	clog.Infof(ctx, "close ideAnalys")
	err := a.hashFS.Close(ctx)
	if err != nil {
		clog.Warningf(ctx, "close hashFS: %v", err)
	}
}

func (a *ideAnalyzer) analyzeTarget(ctx context.Context, target string) (*pb.AnalysisResult, map[string]*pb.BuildableUnit) {
	result := &pb.AnalysisResult{}
	if strings.HasSuffix(target, "^") {
		result.SourceFilePath = strings.TrimSuffix(target, "^")
	}
	nodes, err := a.targetForCPP(ctx, target)
	if err != nil {
		result.Status = &pb.AnalysisResult_Status{
			Code:          pb.AnalysisResult_Status_CODE_NOT_FOUND,
			StatusMessage: proto.String(err.Error()),
		}
		return result, nil
	}
	if len(nodes) == 0 {
		result.Status = &pb.AnalysisResult_Status{
			Code:          pb.AnalysisResult_Status_CODE_NOT_FOUND,
			StatusMessage: proto.String(fmt.Sprintf("missing target=%q", target)),
		}
		return result, nil
	}
	node := nodes[0] // first node by default.
	var nodeEnt *fspb.Entry
	switch filepath.Ext(result.SourceFilePath) {
	case ".c", ".cc", ".cxx", ".cpp", ".m", ".mm", ".S", ".h", ".hxx", ".hpp", ".inc":
		// prefer *.o or *.obj
		node = selectNodeByExt(nodes, ".o", ".obj")
	case ".java":
		// prefer *.jar or *.srcjar
		node = selectNodeByExt(nodes, ".jar", ".srcjar")

		// need to check build succeeded for .java
		// for cxx, we don't compile *.o with
		// `SISO_EXPERIMENTS=prepare-header-only`, so *.o may not exist.
		var ok bool
		nodeEnt, ok = a.fsm[filepath.ToSlash(filepath.Join(a.path.ExecRoot, a.path.Dir, node.Path()))]
		if !ok {
			result.Status = &pb.AnalysisResult_Status{
				Code:          pb.AnalysisResult_Status_CODE_BUILD_FAILED,
				StatusMessage: proto.String(fmt.Sprintf("file not found: %q", node.Path())),
			}
			return result, nil
		}
	}

	inEdge, ok := node.InEdge()
	if !ok {
		result.Status = &pb.AnalysisResult_Status{
			Code:          pb.AnalysisResult_Status_CODE_NOT_FOUND,
			StatusMessage: proto.String(fmt.Sprintf("no input edge for %s", node.Path())),
		}
		return result, nil
	}
	if len(inEdge.Inputs()) == 0 {
		result.Status = &pb.AnalysisResult_Status{
			Code:          pb.AnalysisResult_Status_CODE_NOT_FOUND,
			StatusMessage: proto.String(fmt.Sprintf("no inputs for %s", node.Path())),
		}
		return result, nil
	}
	for _, input := range inEdge.Inputs() {
		e, ok := input.InEdge()
		if ok && e.IsPhony() {
			// TODO: check phony's inputs?
			continue
		}
		ent, ok := a.fsm[filepath.ToSlash(filepath.Join(a.path.ExecRoot, a.path.Dir, input.Path()))]
		if !ok {
			result.Status = &pb.AnalysisResult_Status{
				Code:          pb.AnalysisResult_Status_CODE_BUILD_FAILED,
				StatusMessage: proto.String(fmt.Sprintf("input of %q not found: %q", node.Path(), input.Path())),
			}
			return result, nil
		}
		if nodeEnt != nil && nodeEnt.GetUpdatedTime() < ent.GetId().GetModTime() {
			result.Status = &pb.AnalysisResult_Status{
				Code:          pb.AnalysisResult_Status_CODE_BUILD_FAILED,
				StatusMessage: proto.String(fmt.Sprintf("target is not up-to-date: %q(%d) is older than %q(%d)", node.Path(), nodeEnt.GetUpdatedTime(), input.Path(), ent.GetId().GetModTime())),
			}
			return result, nil
		}
	}
	if result.SourceFilePath == "" {
		result.SourceFilePath = inEdge.Inputs()[0].Path()
	}

	switch filepath.Ext(result.SourceFilePath) {
	case ".c", ".cc", ".cxx", ".cpp", ".m", ".mm", ".S", ".h", ".hxx", ".hpp", ".inc":
		return a.analyzeCPP(ctx, inEdge, result)
	case ".java":
		return a.analyzeJava(ctx, inEdge, result)
	default:
		clog.Infof(ctx, "analyze unknown %s", result.SourceFilePath)
	}
	// TODO: what should we return?
	return result, nil
}

func (a *ideAnalyzer) targetForCPP(ctx context.Context, target string) ([]*ninjautil.Node, error) {
	nodes, err := a.state.Targets([]string{target})
	if err == nil {
		return nodes, nil
	}

	// err != nil
	switch filepath.Ext(target) {
	case ".h^", ".hxx^", ".hpp^", ".inc^":
		hname := strings.TrimSuffix(target, "^")
		_, serr := os.Stat(hname)
		if serr != nil {
			clog.Warningf(ctx, "file not exist: %v", serr)
			return nil, err
		}
		// fallback to any *.cc file in the directory.
		// language service need to know include directories and
		// flags that affects compiler's behavior, but doesn't care
		// *.cc includes given header.
		clog.Infof(ctx, "fallback. target not found: %v", err)
		dirname := filepath.Dir(target)
		dirents, derr := os.ReadDir(dirname)
		if derr != nil {
			clog.Warningf(ctx, "readdir %q: %v", dirname, derr)
			return nil, err
		}
		files := make(map[string]*ninjautil.Node)
		for _, dirent := range dirents {
			fname := dirent.Name()
			switch filepath.Ext(fname) {
			case ".c", ".cc", "cxx", ".cpp", ".m", ".mm", ".S":
				fpath := filepath.ToSlash(filepath.Join(dirname, fname))
				nodes, err := a.state.Targets([]string{fpath + "^"})
				if err != nil {
					continue
				}
				files[fpath] = nodes[0]
				clog.Infof(ctx, "candidate: %s", fpath)
			}
		}
		if len(files) == 0 {
			return nil, fmt.Errorf("no candidate in %s: %w", dirname, err)
		}
		var filenames []string
		for fname := range files {
			filenames = append(filenames, fname)
		}
		sort.Strings(filenames)
		fmt.Fprintf(os.Stderr, "target %q not found: fallback to %q\n", target, filenames[0]+"^")
		return []*ninjautil.Node{files[filenames[0]]}, nil
	}
	return nil, err
}

func selectNodeByExt(nodes []*ninjautil.Node, exts ...string) *ninjautil.Node {
	for _, n := range nodes {
		if slices.Contains(exts, filepath.Ext(n.Path())) {
			return n
		}
	}
	return nodes[0]
}

func (a *ideAnalyzer) analyzeCPP(ctx context.Context, edge *ninjautil.Edge, result *pb.AnalysisResult) (*pb.AnalysisResult, map[string]*pb.BuildableUnit) {
	clog.Infof(ctx, "analyze cpp %s", result.SourceFilePath)
	deps := map[string]*pb.BuildableUnit{}

	// get command line from edge
	command := edge.Binding("command")
	cmdArgs, err := shutil.Split(command)
	if err != nil {
		result.Status = &pb.AnalysisResult_Status{
			Code:          pb.AnalysisResult_Status_CODE_BUILD_FAILED,
			StatusMessage: proto.String(fmt.Sprintf("failed to parse command=%q: %v", command, err)),
		}
		return result, nil
	}
	// scandeps
	params, err := gccutil.ExtractScanDepsParams(ctx, cmdArgs, nil, a.hashFS.FileSystem(ctx, filepath.Join(a.path.ExecRoot, a.path.Dir)))
	if err != nil {
		result.Status = &pb.AnalysisResult_Status{
			Code:          pb.AnalysisResult_Status_CODE_BUILD_FAILED,
			StatusMessage: proto.String(fmt.Sprintf("failed to extract scandeps params for %q: %v", cmdArgs, err)),
		}
		return result, nil
	}
	for i := range params.Sources {
		params.Sources[i] = a.path.MaybeFromWD(ctx, params.Sources[i])
	}
	// no need to canonicalize path for Includes.
	// it should be used as is for `#include "pathname.h"`
	for i := range params.Files {
		params.Files[i] = a.path.MaybeFromWD(ctx, params.Files[i])
	}
	for i := range params.Dirs {
		params.Dirs[i] = a.path.MaybeFromWD(ctx, params.Dirs[i])
	}
	for i := range params.Frameworks {
		params.Frameworks[i] = a.path.MaybeFromWD(ctx, params.Frameworks[i])
	}
	for i := range params.Sysroots {
		params.Sysroots[i] = a.path.MaybeFromWD(ctx, params.Sysroots[i])
	}
	req := scandeps.Request{
		Defines:    params.Defines,
		Sources:    params.Sources,
		Includes:   params.Includes,
		Dirs:       params.Dirs,
		Frameworks: params.Frameworks,
		Sysroots:   params.Sysroots,
	}
	started := time.Now()
	clog.Infof(ctx, "scandeps %#v", req)
	incs, err := a.scanDeps.Scan(ctx, a.path.ExecRoot, req)
	if err != nil {
		result.Status = &pb.AnalysisResult_Status{
			Code:          pb.AnalysisResult_Status_CODE_BUILD_FAILED,
			StatusMessage: proto.String(fmt.Sprintf("failed to scandeps %#v: %v", req, err)),
		}
		return result, nil
	}
	clog.Infof(ctx, "scandeps results: %q", incs)
	fmt.Fprintf(os.Stderr, "%s scandeps in %s\n", result.SourceFilePath, ui.FormatDuration(time.Since(started)))
	started = time.Now()

	for _, inc := range incs {
		incTarget := a.path.MaybeToWD(ctx, inc)
		node, ok := a.state.LookupNodeByPath(incTarget)
		if !ok {
			clog.Infof(ctx, "not in build graph: %s", incTarget)
			continue
		}
		inEdge, ok := node.InEdge()
		if !ok {
			clog.Infof(ctx, "not generated file: %s", incTarget)
			continue
		}
		// for each generated files
		var generatedFiles []*pb.GeneratedFile
		for _, out := range inEdge.Outputs() {
			path := out.Path()
			buf, err := a.hashFS.ReadFile(ctx, a.path.ExecRoot, a.path.MaybeFromWD(ctx, path))
			if err != nil {
				clog.Infof(ctx, "not exist generated file %q: %v", path, err)
				continue
			}
			generatedFiles = append(generatedFiles, &pb.GeneratedFile{
				Path:     path,
				Contents: buf,
			})
		}
		// add buildable unit for the inEdge
		bu := a.buildableUnit(ctx, inEdge, pb.Language_LANGUAGE_UNSPECIFIED, edgeCmdArgs(inEdge), generatedFiles, nil)
		deps[bu.Id] = bu
	}
	depIDs := make([]string, 0, len(deps))
	for k := range deps {
		depIDs = append(depIDs, k)
	}
	sort.Strings(depIDs)
	buildableUnit := a.buildableUnit(ctx, edge, pb.Language_LANGUAGE_CPP, cmdArgs, nil, depIDs)
	result.UnitId = buildableUnit.Id
	result.Invalidation = a.invalidation(ctx)

	deps[buildableUnit.Id] = buildableUnit
	result.Status = &pb.AnalysisResult_Status{
		Code: pb.AnalysisResult_Status_CODE_OK,
	}
	fmt.Fprintf(os.Stderr, "%s analysis result in %s\n", result.SourceFilePath, ui.FormatDuration(time.Since(started)))
	return result, deps
}

func edgeCmdArgs(edge *ninjautil.Edge) []string {
	command := edge.Binding("command")
	cmdArgs, err := shutil.Split(command)
	if err != nil {
		cmdArgs = []string{"/bin/sh", "-c", command}
	}
	return cmdArgs
}

func (a *ideAnalyzer) buildableUnit(ctx context.Context, edge *ninjautil.Edge, language pb.Language, cmdArgs []string, outputs []*pb.GeneratedFile, depIDs []string) *pb.BuildableUnit {
	outPath := edge.Outputs()[0].Path()
	clog.Infof(ctx, "buildableUnit for %s: lang=%s", outPath, language)

	seen := make(map[string]bool)
	var sourceFiles []string
	for _, in := range edge.Inputs() {
		if seen[in.Path()] {
			continue
		}
		seen[in.Path()] = true
		_, ok := in.InEdge()
		if ok {
			clog.Infof(ctx, "generated source file: %s", in.Path())
			continue
		}
		switch language {
		case pb.Language_LANGUAGE_JAVA:
			switch filepath.Ext(in.Path()) {
			case ".java", ".class":
			case ".jar":
				// precomputed jar files are represented
				// as a buildable unit to reduce the size
				// of computed index b/392977625
				continue
			default:
				clog.Infof(ctx, "ignore non java source: %s", in.Path())
				continue
			}
		}
		sourceFiles = append(sourceFiles, in.Path())
	}

	return &pb.BuildableUnit{
		Id:                outPath,
		Language:          language,
		SourceFilePaths:   sourceFiles,
		CompilerArguments: cmdArgs,
		GeneratedFiles:    outputs,
		DependencyIds:     depIDs,
	}
}

func (a *ideAnalyzer) invalidation(ctx context.Context) *pb.Invalidation {
	inv := &pb.Invalidation{
		Wildcards: []*pb.Invalidation_Wildcard{
			{
				Suffix:         proto.String(".gn"),
				CanCrossFolder: proto.Bool(true),
			},
			{
				Suffix:         proto.String(".gni"),
				CanCrossFolder: proto.Bool(true),
			},
		},
	}
	buf, err := a.hashFS.ReadFile(ctx, a.path.ExecRoot, filepath.Join(a.path.Dir, "build.ninja.d"))
	if err != nil {
		clog.Warningf(ctx, "failed to read build.ninja.d: %v", err)
		return inv
	}
	bdeps, err := makeutil.ParseDeps(ctx, buf)
	if err != nil {
		clog.Warningf(ctx, "failed to parse build.ninja.d: %v", err)
		return inv
	}
	for _, d := range bdeps {
		switch filepath.Ext(d) {
		case ".gn", ".gni":
			continue
		}
		// ignore generated files?
		// e.g. *.build_metadata, *.typemap_config etc
		if filepath.IsLocal(d) {
			continue
		}
		inv.FilePaths = append(inv.FilePaths, filepath.ToSlash(filepath.Join(a.path.Dir, d)))
	}
	return inv
}
