// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Siso is a Ninja-compatible build system optimized for remote execution.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"runtime/pprof"
	"runtime/trace"
	"syscall"

	log "github.com/golang/glog"
	"github.com/google/subcommands"

	"go.chromium.org/build/siso/auth/cred"
	"go.chromium.org/build/siso/hashfs/osfs"
	"go.chromium.org/build/siso/subcmd/alex313031"
	"go.chromium.org/build/siso/subcmd/auth"
	"go.chromium.org/build/siso/subcmd/fetch"
	"go.chromium.org/build/siso/subcmd/fscmd"
	"go.chromium.org/build/siso/subcmd/isolate"
	"go.chromium.org/build/siso/subcmd/metricscmd"
	"go.chromium.org/build/siso/subcmd/ninja"
	"go.chromium.org/build/siso/subcmd/ninjafrontend"
	"go.chromium.org/build/siso/subcmd/proxy"
	"go.chromium.org/build/siso/subcmd/ps"
	"go.chromium.org/build/siso/subcmd/query"
	"go.chromium.org/build/siso/subcmd/recall"
	"go.chromium.org/build/siso/subcmd/report"
	"go.chromium.org/build/siso/subcmd/scandeps"
	"go.chromium.org/build/siso/subcmd/version"
	"go.chromium.org/build/siso/subcmd/webui"
	"go.chromium.org/build/siso/ui"

	_ "net/http/pprof" // import to let pprof register its HTTP handlers
)

var (
	pprofAddr     string
	cpuprofile    string
	memprofile    string
	mutexprofile  string
	blockprofile  string
	blockprofRate int
	mutexprofFrac int
	traceFile     string
)

const versionID = "v1.4.19"
var versionStr = alex313031.GetExecutableName() + " " + versionID

func main() {
	// Wraps sisoMain() because os.Exit() doesn't wait defers.
	os.Exit(sisoMain())
}

func sisoMain() int {
	flag.CommandLine.Usage = func() {
		w := flag.CommandLine.Output()
		fmt.Fprint(w, versionStr)
		if alex313031.IsNG() {
			fmt.Fprint(w, `

Usage: siso-ng [flags] [command] [arguments]

e.g.
 $ siso-ng ninja -C out/Default

`)
		} else {
			fmt.Fprint(w, `
Usage: siso [flags] [command] [arguments]

e.g.
 $ siso ninja -C out/Default

`)
		}
		fmt.Fprintf(w, "important flags of %s:\n", os.Args[0])

		f := flag.Lookup("credential_helper")
		fmt.Fprintf(w, `  -credential_helper path
    %s
    (default %q)
`, f.Usage, f.DefValue)
		f = flag.Lookup("version")
		fmt.Fprintf(w, `  -version
   %s
`, f.Usage)
		if alex313031.IsNG() {
			fmt.Fprintf(flag.CommandLine.Output(), `
Use "siso-ng help" to display commands.
Use "siso-ng help [command]" for more information about a command.
Use "siso-ng flags" to display all flags.
`)
		} else {
			fmt.Fprintf(flag.CommandLine.Output(), `
Use "siso help" to display commands.
Use "siso help [command]" for more information about a command.
Use "siso flags" to display all flags.
`)
		}
	}

	flag.StringVar(&pprofAddr, "pprof_addr", "", `listen address for "go tool pprof". e.g. "localhost:6060"`)
	flag.StringVar(&cpuprofile, "cpuprofile", "", "write cpu profile to this file")
	flag.StringVar(&memprofile, "memprofile", "", "write memory profile to this file")
	flag.StringVar(&mutexprofile, "mutexprofile", "", "write mutex profile to this file")
	flag.StringVar(&blockprofile, "blockprofile", "", "write block profile to this file")
	flag.IntVar(&blockprofRate, "blockprof_rate", 0, "block profile rate")
	flag.IntVar(&mutexprofFrac, "mutexprof_frac", 0, "mutex profile fraction")
	flag.StringVar(&traceFile, "trace", "", `go trace output for "go tool trace"`)

	credHelper := cred.DefaultCredentialHelper()
	if h, ok := os.LookupEnv("SISO_CREDENTIAL_HELPER"); ok {
		credHelper = h
	}
	flag.StringVar(&credHelper, "credential_helper", credHelper, `path to a credential helper.
    see https://github.com/EngFlow/credential-helper-spec/blob/main/spec.md
    "luci-auth" uses luci-auth.
    "gcloud" uses gcloud.
    "google-application-default" or "" uses Google Application Default Credentials.
    "mTLS" disables per RPC credentials.
    environment variable SISO_CREDENTIAL_HELPER sets default value.`)

	var printVersion bool
	flag.BoolVar(&printVersion, "version", false, "print version")
	flag.Parse()

	ctx := context.Background()
	// Flush the log on exit to not lose any messages.
	defer log.Flush()

	// Print a stack trace when a panic occurs.
	defer func() {
		if r := recover(); r != nil {
			const size = 64 << 10
			buf := make([]byte, size)
			buf = buf[:runtime.Stack(buf, false)]
			log.Fatalf("panic: %v\n%s", r, buf)
		}
	}()

	if printVersion {
		return int(version.Cmd(versionStr).Execute(ctx, flag.CommandLine))
	}
	if blockprofile != "" && blockprofRate == 0 {
		blockprofRate = 1
	}
	if mutexprofile != "" && mutexprofFrac == 0 {
		mutexprofFrac = 1
	}
	if blockprofRate > 0 {
		runtime.SetBlockProfileRate(blockprofRate)
	}
	if mutexprofFrac > 0 {
		runtime.SetMutexProfileFraction(mutexprofFrac)
	}

	// Start an HTTP server that can be used to profile Siso during runtime.
	if pprofAddr != "" {
		// https://pkg.go.dev/net/http/pprof
		fmt.Fprintf(os.Stderr, "pprof is enabled, listening at http://%s/debug/pprof/\n", pprofAddr)
		go func() {
			log.Infof("pprof http listener: %v", http.ListenAndServe(pprofAddr, nil))
		}()
		defer func() {
			fmt.Fprintf(os.Stderr, "pprof is still listening at http://%s/debug/pprof/\n", pprofAddr)
			fmt.Fprintln(os.Stderr, "Press Ctrl-C to terminate the process")
			sigch := make(chan os.Signal, 1)
			signal.Notify(sigch, os.Interrupt, syscall.SIGTERM)
			<-sigch
		}()
	}

	// Save a CPU profile to disk on exit.
	if cpuprofile != "" {
		f, err := os.Create(cpuprofile)
		if err != nil {
			log.Fatalf("failed to create cpuprofile file: %v", err)
		}
		err = pprof.StartCPUProfile(f)
		if err != nil {
			log.Errorf("failed to start CPU profiler: %v", err)
		}
		defer pprof.StopCPUProfile()
	}

	// Save a heap profile to disk on exit.
	if memprofile != "" {
		f, err := os.Create(memprofile)
		if err != nil {
			log.Fatalf("failed to create memprofile file: %v", err)
		}
		defer func() {
			err := pprof.WriteHeapProfile(f)
			if err != nil {
				log.Errorf("failed to write heap profile: %v", err)
			}
		}()
	}

	// Save a mutex profile to disk on exit.
	if mutexprofile != "" {
		f, err := os.Create(mutexprofile)
		if err != nil {
			log.Fatalf("failed to create mutexprofile file: %v", err)
		}
		defer func() {
			if err := pprof.Lookup("mutex").WriteTo(f, 0); err != nil {
				log.Errorf("failed to write mutex profile: %v", err)
			}
			if err := f.Close(); err != nil {
				log.Errorf("failed to close mutexprofile file: %v", err)
			}
		}()
	}

	// Save a block profile to disk on exit.
	if blockprofile != "" {
		f, err := os.Create(blockprofile)
		if err != nil {
			log.Fatalf("failed to create blockprofile file: %v", err)
		}
		defer func() {
			if err := pprof.Lookup("block").WriteTo(f, 0); err != nil {
				log.Errorf("failed to write block profile: %v", err)
			}
			if err := f.Close(); err != nil {
				log.Errorf("failed to close blockprofile file: %v", err)
			}
		}()
	}

	// Save a go trace to disk during execution.
	if traceFile != "" {
		fmt.Fprintf(os.Stderr, "enable go trace in %q\n", traceFile)
		f, err := os.Create(traceFile)
		if err != nil {
			log.Fatalf("Failed to create go trace output file: %v", err)
		}
		defer func() {
			fmt.Fprintf(os.Stderr, "go trace: go tool trace %s\n", traceFile)
			cerr := f.Close()
			if cerr != nil {
				log.Fatalf("Failed to close go trace output file: %v", cerr)
			}
		}()
		if err := trace.Start(f); err != nil {
			log.Fatalf("Failed to start go trace: %v", err)
		}
		defer trace.Stop()
	}

	// Initialize the UI and ensure we restore the state of the terminal upon exit.
	ui.Init()
	defer ui.Restore()

	authOpts := cred.AuthOpts(credHelper)
	subcommands.Register(ninja.Cmd(authOpts, versionID), "")

	subcommands.Register(recall.Cmd(authOpts), "reapi")
	subcommands.Register(fetch.Cmd(authOpts), "reapi")
	subcommands.Register(isolate.Cmd(authOpts), "reapi")
	subcommands.Register(proxy.Cmd(authOpts), "reapi")

	subcommands.Register(fscmd.Cmd(authOpts), "investigation")
	subcommands.Register(metricscmd.Cmd(), "investigation")
	subcommands.Register(ps.Cmd(), "investigation")
	subcommands.Register(query.Cmd(), "investigation")
	subcommands.Register(report.Cmd(), "investigation")
	subcommands.Register(webui.Cmd(versionID), "investigation")

	subcommands.Register(auth.CheckCmd(authOpts), "auth")
	subcommands.Register(auth.LoginCmd(authOpts), "auth")
	subcommands.Register(auth.LogoutCmd(authOpts), "auth")

	subcommands.Register(ninjafrontend.Cmd(), "debugging")
	subcommands.Register(scandeps.Cmd(), "debugging")

	subcommands.Register(osfs.HelperCmd(), "internal-helper")

	subcommands.Register(subcommands.FlagsCommand(), "command-help")
	subcommands.Register(subcommands.HelpCommand(), "command-help")
	subcommands.Register(version.Cmd(versionStr), "command-help")

	return int(subcommands.Execute(ctx))
}
