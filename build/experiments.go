// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package build

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"

	log "github.com/golang/glog"

	"go.chromium.org/build/siso/ui"
)

// experiment id -> hint for the experiment (to check more details).
var knownExperiments = map[string]string{
	// check-deps checks deps (e.g. *.d) has source or direct/indirect deps only.
	"check-deps":                  "",
	"fail-on-stdouterr":           "",
	"fail-on-bad-deps":            "",
	"fallback-on-exec-error":      "",
	"fast-deps":                   "",
	"file-access-trace":           "",
	"gvisor":                      "",
	"ignore-missing-local-inputs": "",

	// TODO(b/430486641): remove this once bad *.d is fixed
	"ignore-missing-out-in-depfile": "",

	// TODO(b/374179435): remove this once BUILD_BROKEN_MISSING_OUTPUTS is removed.
	"ignore-missing-outputs": "",

	"ignore-missing-targets":  "",
	"keep-going-handle-error": "",
	"keep-going-impure":       "check siso_localexec",
	"oom-score-adj":           "",
	"no-fallback":             "",
	"no-fast-deps":            "",
	"no-fast-deps-fallback":   "",
	"prepare-header-only":     "",
}

type experimentFeature struct {
	once sync.Once
}

// Experiments manages experimental features.
// Experiments are enabled by SISO_EXPERIMENTS environment variable.
// We don't guarantee experiment id in future versions.
// Unknown experiment id will be ignored.
type Experiments struct {
	once sync.Once
	m    map[string]*experimentFeature
}

var experiments Experiments

// SetExperimentForTest sets experiments for testing.
func SetExperimentForTest(v string) {
	experiments.set(v)
}

const experimentEnv = "SISO_EXPERIMENTS"

func (e *Experiments) init() {
	if e.m != nil {
		return
	}
	env := os.Getenv(experimentEnv)
	if env == "" {
		return
	}
	e.set(env)
}

func (e *Experiments) set(env string) {
	e.m = make(map[string]*experimentFeature)
	for v := range strings.SplitSeq(env, ",") {
		if _, ok := knownExperiments[v]; !ok {
			log.Warningf("unknown experiment %q. ignored", v)
			continue
		}
		e.m[v] = &experimentFeature{}
	}
}

// ShowOnce shows once about enabled experimental features.
func (e *Experiments) ShowOnce() {
	e.init()
	e.once.Do(func() {
		s := e.String()
		if s != "" {
			s = ui.SGR(ui.Yellow, s)
			ui.Default.PrintLines(s)
		}
	})
}

func (e *Experiments) String() string {
	var sb strings.Builder
	keys := make([]string, 0, len(e.m))
	for key := range e.m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		fmt.Fprintf(&sb, "%s=%s enabled\n", experimentEnv, key)
	}
	return sb.String()
}

// Enabled returns true if experimental feature k is enabled, and
// log error once with its hint if so.
func (e *Experiments) Enabled(k, format string, args ...any) bool {
	ex, ok := e.m[k]
	if !ok {
		return false
	}
	ex.once.Do(func() {
		ui.Default.PrintLines(fmt.Sprintf(format+" %s\n", append(args, e.Hint(k))...))
	})
	return true
}

// Hint shows hint message for experimental feature k.
func (e *Experiments) Hint(k string) string {
	return knownExperiments[k]
}

// Suggest returns suggest message to enable experimental feature k.
func (e *Experiments) Suggest(k string) string {
	hint := knownExperiments[k]
	if hint != "" {
		return fmt.Sprintf("need %s=%s or %s", experimentEnv, k, hint)
	}
	return fmt.Sprintf("need %s=%s", experimentEnv, k)
}
