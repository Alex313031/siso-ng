// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package query

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	"go.chromium.org/build/siso/o11y/clog"
	pb "go.chromium.org/build/siso/toolsupport/ciderutil/proto"
	"go.chromium.org/build/siso/toolsupport/ninjautil"
	"go.chromium.org/build/siso/toolsupport/shutil"
	"go.chromium.org/build/siso/ui"
)

func (a *ideAnalyzer) analyzeJava(ctx context.Context, edge *ninjautil.Edge, result *pb.AnalysisResult) (*pb.AnalysisResult, map[string]*pb.BuildableUnit) {
	clog.Infof(ctx, "analyze java %s", result.SourceFilePath)
	started := time.Now()
	deps := map[string]*pb.BuildableUnit{}
	seen := make(map[string]bool)
	depIDs, err := a.appendIndirectJavaBuildableUnits(ctx, edge, deps, seen)
	if err != nil {
		result.Status = &pb.AnalysisResult_Status{
			Code:          pb.AnalysisResult_Status_CODE_BUILD_FAILED,
			StatusMessage: proto.String(fmt.Sprintf("failed for java buildableUnit: %v", err)),
		}
		return result, deps
	}
	if len(depIDs) == 0 {
		result.Status = &pb.AnalysisResult_Status{
			Code:          pb.AnalysisResult_Status_CODE_BUILD_FAILED,
			StatusMessage: proto.String(fmt.Sprintf("failed for java buildableUnit: missing buildableUnit for %q", result.SourceFilePath)),
		}
		return result, deps
	}
	result.UnitId = depIDs[0]
	result.Invalidation = a.invalidation(ctx)
	result.Status = &pb.AnalysisResult_Status{
		Code: pb.AnalysisResult_Status_CODE_OK,
	}
	fmt.Fprintf(os.Stderr, "%s analysis result in %s\n", result.SourceFilePath, ui.FormatDuration(time.Since(started)))
	return result, deps
}

func (a *ideAnalyzer) appendIndirectJavaBuildableUnits(ctx context.Context, edge *ninjautil.Edge, buildableUnits map[string]*pb.BuildableUnit, seen map[string]bool) ([]string, error) {
	if seen[edge.Outputs()[0].Path()] {
		return nil, nil
	}
	seen[edge.Outputs()[0].Path()] = true
	clog.Infof(ctx, "check edge for %q", edge.Outputs()[0].Path())
	isFinalCommand := len(buildableUnits) == 0
	var nextEdges []*ninjautil.Edge
	depIDseen := map[string]bool{}
	for _, in := range edge.Inputs() {
		if _, ok := buildableUnits[in.Path()]; ok {
			continue
		}
		edge, ok := in.InEdge()
		if ok {
			nextEdges = append(nextEdges, edge)
			continue
		}
		// move precomputed jar file to dedicated buildable unit
		// to reduce the size of the computed index b/392977625
		switch filepath.Ext(in.Path()) {
		case ".jar":
			bu := &pb.BuildableUnit{
				Id:              in.Path(),
				Language:        pb.Language_LANGUAGE_JAVA,
				SourceFilePaths: []string{in.Path()},
			}
			buildableUnits[in.Path()] = bu
			depIDseen[in.Path()] = true
		}
	}
	for _, edge := range nextEdges {
		deps, err := a.appendIndirectJavaBuildableUnits(ctx, edge, buildableUnits, seen)
		if err != nil {
			return nil, err
		}
		for _, dep := range deps {
			depIDseen[dep] = true
		}
	}
	var depIDs []string
	for dep := range depIDseen {
		depIDs = append(depIDs, dep)
	}
	sort.Strings(depIDs)
	need := needJavaBuildableUnit(edge)
	if !need {
		return depIDs, nil
	}
	var args []string
	var generatedFiles []*pb.GeneratedFile
	if isFinalCommand {
		// command line for the final step.
		cmdArgs := edgeCmdArgs(edge)
		if len(cmdArgs) > 2 && strings.Contains(cmdArgs[0], "python") && cmdArgs[1] == "../../build/android/gyp/compile_java.py" {
			// chromium specific handling
			// drop --java-srcjars
			// TODO(b/337982520): need to use srcjars?
			var queryArgs []string
			for _, arg := range cmdArgs {
				if strings.HasPrefix(arg, "--java-srcjars=") {
					continue
				}
				queryArgs = append(queryArgs, arg)
			}
			queryArgs = append(queryArgs, "--print-javac-command-line")
			cmd := exec.CommandContext(ctx, queryArgs[0], queryArgs[1:]...)
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			err := cmd.Run()
			if err != nil {
				return nil, fmt.Errorf("failed to get javac command line %q\nstdout:\n%s\nstderr:\n%s\nerr: %w", queryArgs, stdout.String(), stderr.String(), err)
			}
			javacCommand := stdout.String()
			javacArgs, err := shutil.Split(javacCommand)
			if err != nil {
				return nil, fmt.Errorf("failed to parse javac command=%q: %w", javacCommand, err)
			}
			args = javacArgs
			if strings.HasPrefix(javacArgs[len(javacArgs)-1], "@") {
				rspfile := javacArgs[len(javacArgs)-1][1:]
				// tempdir is created by compile_java.py.
				// it is normally removed by compile_java.py, but
				// preserved when --print-javac-command-line.
				tempdir := filepath.Dir(rspfile)
				buf, err := os.ReadFile(rspfile)
				if err != nil {
					clog.Warningf(ctx, "failed to read javac rsp file %q: %v", rspfile, err)
				}
				err = os.RemoveAll(tempdir)
				if err != nil {
					clog.Warningf(ctx, "failed to remove tempdir %q: %v", tempdir, err)
				}
				generatedFiles = append(generatedFiles, &pb.GeneratedFile{
					Path:     rspfile,
					Contents: buf,
				})
			}
		} else {
			// use command line as is if not compile_java.py
			// TODO(b/337982520): need special handling for other steps?
			args = cmdArgs
		}
		// b/391878665: compiler_arguments doesn't need to have
		// - compiler binary path (i.e. javac) in args[0]
		// - `-d` flag and its argument
		// - `-classpath` flag and its argument
		// - `-encoding` flag and its argument
		// - `-J-XX:+PerfDisableSharedMem` flag
		// - last argument, @**/files_list.txt
		var compilerArgs []string
		var skipArg bool
		for _, arg := range args[1:] {
			if skipArg {
				skipArg = false
				continue
			}
			switch arg {
			case "-d", "-classpath", "-encoding":
				skipArg = true
				continue
			case "-J-XX:+PerfDisableSharedMem":
				continue
			}
			compilerArgs = append(compilerArgs, arg)
		}
		if lastArg := compilerArgs[len(compilerArgs)-1]; strings.HasPrefix(lastArg, "@") && filepath.Base(lastArg) == "files_list.txt" {
			compilerArgs = compilerArgs[:len(compilerArgs)-1]
		}
		args = compilerArgs

		// need generated jar files. b/392972300
		for _, out := range edge.Outputs() {
			path := out.Path()
			switch filepath.Ext(path) {
			case ".jar":
				buf, err := a.hashFS.ReadFile(ctx, a.path.ExecRoot, a.path.MaybeFromWD(ctx, path))
				if err != nil {
					clog.Warningf(ctx, "not exist generated file %q: %v", path, err)
					continue
				}
				generatedFiles = append(generatedFiles, &pb.GeneratedFile{
					Path:     path,
					Contents: buf,
				})
			}
		}
	} else {
		// no command line is needed for non-final step.
		// just add generated files (.java/.jar/.class).
		// ignore other generated files.
		for _, out := range edge.Outputs() {
			path := out.Path()
			switch filepath.Ext(path) {
			case ".java", ".jar", ".class":
				buf, err := a.hashFS.ReadFile(ctx, a.path.ExecRoot, a.path.MaybeFromWD(ctx, path))
				if err != nil {
					clog.Warningf(ctx, "not exist generated file %q: %v", path, err)
					continue
				}
				generatedFiles = append(generatedFiles, &pb.GeneratedFile{
					Path:     path,
					Contents: buf,
				})
			}
		}
	}
	buildableUnit := a.buildableUnit(ctx, edge, pb.Language_LANGUAGE_JAVA, args, generatedFiles, depIDs)
	buildableUnits[buildableUnit.Id] = buildableUnit

	return []string{buildableUnit.Id}, nil
}

func needJavaBuildableUnit(edge *ninjautil.Edge) bool {
	if edge.IsPhony() {
		return false
	}
	for _, out := range edge.Outputs() {
		switch filepath.Ext(out.Path()) {
		case ".java", ".jar", ".class":
			return true
		}
	}
	return false
}
