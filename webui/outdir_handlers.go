// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package webui

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.chromium.org/build/siso/build"
	"go.chromium.org/build/siso/toolsupport/ninjautil"
)

const (
	DefaultItemsPerPage = 100
)

type outdirInfo struct {
	path         string
	pathRel      string
	manifestPath string
	outroot      string
	outsub       string
	metrics      []*buildMetrics
	latestRevID  string

	mu            sync.Mutex
	manifestMtime time.Time
	ninjaState    *ninjautil.State
}

type fieldAggregate struct {
	Key   string
	Count int
}

// buildMetrics represents data for a single build revision.
// (Exported fields are accessible from Go templates.)
type buildMetrics struct {
	Mtime         time.Time
	Rev           string
	buildDuration build.IntervalMetric
	lastStepID    string
	ruleCounts    []fieldAggregate
	actionCounts  []fieldAggregate
	// buildMetrics contains build.StepMetric related to overall build e.g. regenerate ninja files.
	buildMetrics []*build.StepMetric
	// StepMetrics contains build.StepMetric related to ninja executions.
	StepMetrics []*build.StepMetric
	// stepByStepID keys step ID to *build.StepMetric for faster lookup.
	stepByStepID map[string]*build.StepMetric
	// stepByOutput keys output to *build.StepMetric for faster lookup.
	stepByOutput map[string]*build.StepMetric
}

// aggregateMetric represents data for the aggregates page.
// (All fields are exported to be usable in Go templates.)
type aggregateMetric struct {
	AggregateBy           string
	Count                 int
	TotalUtime            build.IntervalMetric
	TotalDuration         build.IntervalMetric
	TotalWeightedDuration build.IntervalMetric
}

func loadBuildMetrics(metricsPath string) (*buildMetrics, error) {
	f, err := os.Open(metricsPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read metrics: %w", err)
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("failed to stat metrics: %w", err)
	}

	metricsData := &buildMetrics{
		Mtime:        stat.ModTime(),
		buildMetrics: []*build.StepMetric{},
		StepMetrics:  []*build.StepMetric{},
		stepByStepID: make(map[string]*build.StepMetric),
		stepByOutput: make(map[string]*build.StepMetric),
		ruleCounts:   []fieldAggregate{},
		actionCounts: []fieldAggregate{},
	}

	d := json.NewDecoder(f)
	for {
		var m build.StepMetric
		err := d.Decode(&m)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("parse error in %s:%d: %w", metricsPath, d.InputOffset(), err)
		}
		if m.BuildID != "" {
			metricsData.buildMetrics = append(metricsData.buildMetrics, &m)
			// The last build metric found has the actual build duration.
			metricsData.buildDuration = m.Duration
		} else if m.StepID != "" {
			metricsData.StepMetrics = append(metricsData.StepMetrics, &m)
			metricsData.stepByStepID[m.StepID] = &m
			metricsData.stepByOutput[m.Output] = &m
			metricsData.lastStepID = m.StepID
		} else {
			return nil, fmt.Errorf("unexpected metric found %v", m)
		}
	}

	if len(metricsData.buildMetrics) == 0 || metricsData.buildMetrics[0].BuildID == "" {
		return nil, fmt.Errorf("need at least one build_id in %s", metricsPath)
	}
	metricsData.Rev = metricsData.buildMetrics[0].BuildID

	actionCounts := make(map[string]int)
	for _, metric := range metricsData.StepMetrics {
		if metric.Action != "" {
			actionCounts[metric.Action]++
		}
	}
	// TODO(b/349287453): use maps.Keys once go 1.23
	for action := range actionCounts {
		metricsData.actionCounts = append(metricsData.actionCounts, fieldAggregate{
			Key:   action,
			Count: actionCounts[action],
		})
	}
	slices.SortFunc(metricsData.actionCounts, func(a, b fieldAggregate) int {
		return cmp.Compare(b.Count, a.Count)
	})

	ruleCounts := make(map[string]int)
	for _, metric := range metricsData.StepMetrics {
		if metric.Rule != "" {
			ruleCounts[metric.Rule]++
		}
	}
	// TODO(b/349287453): use maps.Keys once go 1.23
	for rule := range ruleCounts {
		metricsData.ruleCounts = append(metricsData.ruleCounts, fieldAggregate{
			Key:   rule,
			Count: ruleCounts[rule],
		})
	}
	slices.SortFunc(metricsData.ruleCounts, func(a, b fieldAggregate) int {
		return cmp.Compare(b.Count, a.Count)
	})

	return metricsData, nil
}

// loadOutdirInfo attempts to load all metrics found in the outdir.
func loadOutdirInfo(execRoot, outdirPath, manifestPath string) (*outdirInfo, error) {
	start := time.Now()
	fmt.Fprintf(os.Stderr, "load data at %s...", outdirPath)
	defer func() {
		fmt.Fprintf(os.Stderr, " returned in %v\n", time.Since(start))
	}()

	// Get path relative to execroot.
	// TODO: support paths non-relative to execroot?
	execRel, err := filepath.Rel(execRoot, outdirPath)
	if err != nil {
		return nil, fmt.Errorf("couldn't get outdir relative to execdir: %w", err)
	}

	// Validate manifest path.
	_, err = os.Stat(filepath.Join(outdirPath, manifestPath))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, &ErrManifestNotExist{outdirPath, manifestPath}
	}

	// TODO(b/361703735): make sure this works on windows? https://chromium-review.googlesource.com/c/infra/infra/+/5803123/comment/502308d3_ac05bf91/
	outroot, outsub := filepath.Split(execRel)
	if outroot == "" || strings.Contains(outsub, "/") {
		return nil, fmt.Errorf("outdir must match pattern `execroot/outroot/outsub`, others are not supported yet")
	}
	outroot = filepath.Clean(outroot)

	outdirInfo := &outdirInfo{
		path:         outdirPath,
		pathRel:      execRel,
		manifestPath: manifestPath,
		outroot:      outroot,
		outsub:       outsub,
		mu:           sync.Mutex{},
	}

	// Attempt to load latest metrics first.
	// Only silently ignore if it doesn't exist, otherwise always return error.
	// TODO(b/349287453): consider tolerate fail, so frontend can show error?
	latestMetricsPath := filepath.Join(outdirPath, "siso_metrics.json")
	_, err = os.Stat(latestMetricsPath)
	if err == nil {
		latestMetrics, err := loadBuildMetrics(latestMetricsPath)
		if err != nil {
			return nil, fmt.Errorf("failed to load latest metrics: %w", err)
		}
		outdirInfo.metrics = append(outdirInfo.metrics, latestMetrics)
		outdirInfo.latestRevID = latestMetrics.Rev
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("failed to stat latest metrics: %w", err)
	}

	// Then load revisions if available.
	// Always return error if loading any fails.
	// TODO(b/349287453): consider tolerate fail, so frontend can show error?
	revPaths, err := filepath.Glob(filepath.Join(outdirPath, "siso_metrics.*.json"))
	if err != nil {
		return nil, fmt.Errorf("failed to glob revs: %w", err)
	}
	for _, revPath := range revPaths {
		baseName := filepath.Base(revPath)
		matches := sisoMetricsRe.FindStringSubmatch(baseName)
		if matches == nil {
			fmt.Fprintf(os.Stderr, "ignoring invalid %s\n", revPath)
			continue
		}
		revMetrics, err := loadBuildMetrics(revPath)
		if err != nil {
			return nil, fmt.Errorf("failed to load %s: %w", revPath, err)
		}
		outdirInfo.metrics = append(outdirInfo.metrics, revMetrics)
	}
	return outdirInfo, nil
}

// getOutdirForRequest lazy-loads outdir for the request, returning cached result if possible.
func (s *WebuiServer) getOutdirForRequest(r *http.Request) (*outdirInfo, error) {
	abs := filepath.Join(s.execRoot, r.PathValue("outroot"), r.PathValue("outsub"))
	s.metricsMu.Lock()
	defer s.metricsMu.Unlock()
	outdirInfo, ok := s.outdirMetrics[abs]
	if !ok {
		var err error
		// TODO: support override manifest path (i.e. other than build.ninja?)
		outdirInfo, err = loadOutdirInfo(s.execRoot, abs, "build.ninja")
		if err != nil {
			return nil, fmt.Errorf("couldn't load outdir %s: %w", abs, err)
		}
		s.outdirMetrics[abs] = outdirInfo
	}
	return outdirInfo, nil
}

func (s *WebuiServer) handleOutdirReload(w http.ResponseWriter, r *http.Request) {
	outdirInfo, err := s.getOutdirForRequest(r)
	if err != nil {
		s.renderBuildViewError(http.StatusNotFound, fmt.Sprintf("outdir failed to load for request %s: %v", r.URL, err), w, r)
		return
	}

	// loadOutdirInfo will always override existing cached data.
	newOutdirInfo, err := loadOutdirInfo(s.execRoot, outdirInfo.path, outdirInfo.manifestPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to reload outdir: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	s.metricsMu.Lock()
	s.outdirMetrics[outdirInfo.path] = newOutdirInfo
	s.metricsMu.Unlock()

	// Then redirect to root page.
	http.Redirect(w, r, outdirBaseURL(r), http.StatusTemporaryRedirect)
}

// handleOutdirRoot redirects from outdir root URL to `./builds/{latestRev}/steps/`.
func (s *WebuiServer) handleOutdirRoot(w http.ResponseWriter, r *http.Request) {
	outdirInfo, err := s.getOutdirForRequest(r)
	if err != nil {
		s.renderBuildViewError(http.StatusNotFound, fmt.Sprintf("outdir failed to load for request %s: %v", r.URL, err), w, r)
		return
	}

	if outdirInfo.latestRevID == "" {
		s.renderBuildViewError(http.StatusNotFound, "outdir has no metrics", w, r)
		return
	}

	http.Redirect(w, r, fmt.Sprintf("%s/builds/%s/steps/", outdirBaseURL(r), outdirInfo.latestRevID), http.StatusTemporaryRedirect)
}

func (s *WebuiServer) handleOutdirViewLog(w http.ResponseWriter, r *http.Request) {
	outdirInfo, err := s.getOutdirForRequest(r)
	if err != nil {
		s.renderBuildViewError(http.StatusNotFound, fmt.Sprintf("outdir failed to load for request %s: %v", r.URL, err), w, r)
		return
	}

	tmpl, err := s.loadView("_logs.html")
	if err != nil {
		s.renderBuildViewError(http.StatusInternalServerError, fmt.Sprintf("failed to load view: %s", err), w, r)
		return
	}

	allowedFilesMap := map[string]string{
		".siso_config":     ".siso_config%.0s",
		".siso_filegroups": ".siso_filegroups%.0s",
		"siso_localexec":   "siso_localexec.%s",
		"siso_output":      "siso_output.%s",
		"siso_trace.json":  "siso_trace.%s.json",
	}

	// Make sure this file is allowed.
	requestedFile := r.PathValue("file")
	revFileFormatter, ok := allowedFilesMap[requestedFile]
	if !ok {
		s.renderBuildViewError(http.StatusNotFound, fmt.Sprintf("unknown file: %s", requestedFile), w, r)
		return
	}

	revID := r.PathValue("rev")
	// Webui identifies builds using build ID, but build ID is only known in siso_metrics.json.
	// We need to find the siso_metrics.json that contains the build ID to figure out the suffix.
	// Do this every page load because we don't know if another build has happened since past reload.
	// If this has happened, then the suffix would have changed.
	// (The alternative is either store all files in-memory, or refactor Webui to watch for files changing.)
	matches, err := filepath.Glob(filepath.Join(outdirInfo.path, "siso_metrics*.json"))
	if err != nil {
		s.renderBuildViewError(http.StatusInternalServerError, fmt.Sprintf("failed to glob siso_metrics*.json: %v", err), w, r)
		return
	}
	buildSuffix := "unknown"
	// The loop to try reading every siso_metrics.json.
	// Use a buffer with small cap because these files are huge.
	// We only use the first build ID in the file which should be in the first few lines.
	buffer := make([]byte, 4096)
	for _, match := range matches {
		file, err := os.Open(match)
		if err != nil {
			continue
		}
		defer file.Close() // OK to ignore error, because we're just reading.
		n, err := file.Read(buffer)
		if err != nil && !errors.Is(err, io.EOF) {
			// TODO: log?
			continue
		}
		// Use n returned from file.Read so that we don't read more than the actual file.
		if strings.Contains(string(buffer[:n]), revID) {
			withoutExt := strings.TrimSuffix(filepath.Base(match), ".json")
			if withoutExt == "siso_metrics" {
				buildSuffix = ""
				break
			}
			buildSuffix = strings.TrimPrefix(withoutExt, "siso_metrics.")
			break
		}
	}
	if buildSuffix == "unknown" {
		if err != nil {
			s.renderBuildViewError(http.StatusInternalServerError, fmt.Sprintf("failed to read siso_metrics*.json: %v", err), w, r)
			return
		}
		s.renderBuildViewError(http.StatusNotFound, fmt.Sprintf("couldn't find siso_metrics identifying build %s", revID), w, r)
		return
	}

	// Now that correct suffix is known then can get the right file.
	actualFile := requestedFile
	if buildSuffix != "" {
		actualFile = fmt.Sprintf(revFileFormatter, buildSuffix)
	}

	fileContents, err := os.ReadFile(filepath.Join(s.defaultOutdir, actualFile))
	if err != nil {
		s.renderBuildViewError(http.StatusInternalServerError, fmt.Sprintf("failed to open file: %v", err), w, r)
		return
	}

	if r.URL.Query().Get("raw") == "true" {
		w.Header().Add("Content-Type", "text/plain; charset=UTF-8")
		_, err := w.Write(fileContents)
		if err != nil {
			s.renderBuildViewError(http.StatusInternalServerError, fmt.Sprintf("failed to write file contents: %v", err), w, r)
		}
		return
	}

	// TODO(b/349287453): use maps.Keys once go 1.23
	var allowedFiles []string
	for allowedFile := range allowedFilesMap {
		allowedFiles = append(allowedFiles, allowedFile)
	}
	slices.Sort(allowedFiles)

	err = s.renderBuildView(w, r, tmpl, map[string]any{
		"allowedFiles": allowedFiles,
		"file":         requestedFile,
		"fileContents": string(fileContents),
	})
	if err != nil {
		s.renderBuildViewError(http.StatusInternalServerError, fmt.Sprintf("failed to render view: %v", err), w, r)
	}
}

func (s *WebuiServer) handleOutdirAggregates(w http.ResponseWriter, r *http.Request) {
	outdirInfo, err := s.getOutdirForRequest(r)
	if err != nil {
		s.renderBuildViewError(http.StatusNotFound, fmt.Sprintf("outdir failed to load for request %s: %v", r.URL, err), w, r)
		return
	}

	var metrics *buildMetrics
	for _, m := range outdirInfo.metrics {
		if m.Rev == r.PathValue("rev") {
			metrics = m
			break
		}
	}
	if metrics == nil {
		s.renderBuildViewError(http.StatusNotFound, fmt.Sprintf("no metrics found for request %s", r.URL), w, r)
		return
	}

	aggregates := make(map[string]aggregateMetric)
	for _, m := range metrics.StepMetrics {
		// Aggregate by rule if exists otherwise action.
		aggregateBy := m.Action
		if len(m.Rule) > 0 {
			aggregateBy = m.Rule
		}
		entry, ok := aggregates[aggregateBy]
		if !ok {
			entry.AggregateBy = aggregateBy
		}
		entry.Count++
		entry.TotalUtime += m.Utime
		entry.TotalDuration += m.Duration
		entry.TotalWeightedDuration += m.WeightedDuration
		aggregates[aggregateBy] = entry
	}

	// Sort by utime descending.
	// TODO(b/349287453): use maps.Values once go 1.23
	var sortedAggregates []aggregateMetric
	for _, v := range aggregates {
		sortedAggregates = append(sortedAggregates, v)
	}
	slices.SortFunc(sortedAggregates, func(a, b aggregateMetric) int {
		return cmp.Compare(b.TotalUtime, a.TotalUtime)
	})

	tmpl, err := s.loadView("_aggregates.html")
	if err != nil {
		s.renderBuildViewError(http.StatusInternalServerError, fmt.Sprintf("failed to load view: %s", err), w, r)
		return
	}

	err = s.renderBuildView(w, r, tmpl, map[string]any{
		"aggregates": sortedAggregates,
	})
	if err != nil {
		s.renderBuildViewError(http.StatusInternalServerError, fmt.Sprintf("failed to render view: %v", err), w, r)
	}
}

func (s *WebuiServer) handleOutdirDoRecall(w http.ResponseWriter, r *http.Request) {
	outdirInfo, err := s.getOutdirForRequest(r)
	if err != nil {
		s.renderBuildViewError(http.StatusNotFound, fmt.Sprintf("outdir failed to load for request %s: %v", r.URL, err), w, r)
		return
	}

	var metrics *buildMetrics
	for _, m := range outdirInfo.metrics {
		if m.Rev == r.PathValue("rev") {
			metrics = m
			break
		}
	}
	if metrics == nil {
		s.renderBuildViewError(http.StatusNotFound, fmt.Sprintf("no metrics found for request %s", r.URL), w, r)
		return
	}

	tmpl, err := s.loadView("_recall.html")
	if err != nil {
		s.renderBuildViewError(http.StatusInternalServerError, fmt.Sprintf("failed to load view: %s", err), w, r)
		return
	}

	metric, ok := metrics.stepByStepID[r.PathValue("id")]
	if !ok {
		s.renderBuildViewError(http.StatusNotFound, fmt.Sprintf("stepID %s not found", r.PathValue("id")), w, r)
		return
	}

	err = s.renderBuildView(w, r, tmpl, map[string]any{
		"stepID":        metric.StepID,
		"digest":        metric.Digest,
		"project":       r.FormValue("project"),
		"reapiInstance": r.FormValue("reapi_instance"),
	})
	if err != nil {
		s.renderBuildViewError(http.StatusInternalServerError, fmt.Sprintf("failed to render view: %v", err), w, r)
	}
}

func (s *WebuiServer) handleOutdirViewStep(w http.ResponseWriter, r *http.Request) {
	var metrics *buildMetrics
	var outdirInfo *outdirInfo
	var err error
	if didRequestUploadedMetrics(r) {
		for _, m := range s.uploadedMetrics {
			if m.Rev == r.PathValue("rev") {
				metrics = m
				break
			}
		}
	} else {
		outdirInfo, err = s.getOutdirForRequest(r)
		if err != nil {
			s.renderBuildViewError(http.StatusNotFound, fmt.Sprintf("outdir failed to load for request %s: %v", r.URL, err), w, r)
			return
		}
		for _, m := range outdirInfo.metrics {
			if m.Rev == r.PathValue("rev") {
				metrics = m
				break
			}
		}
	}
	if metrics == nil {
		s.renderBuildViewError(http.StatusNotFound, fmt.Sprintf("no metrics found for request %s", r.URL), w, r)
		return
	}

	tmpl, err := s.loadView("_step.html")
	if err != nil {
		s.renderBuildViewError(http.StatusInternalServerError, fmt.Sprintf("failed to load view: %s", err), w, r)
		return
	}

	// Load the step.
	stepData, ok := metrics.stepByStepID[r.PathValue("id")]
	if !ok {
		s.renderBuildViewError(http.StatusNotFound, fmt.Sprintf("stepID %s not found", r.PathValue("id")), w, r)
		return
	}

	// Find steps with the same output in other revs.
	// (A step could have multiple outputs, and we only log the first output as the output name.
	// So if it changes across builds it won't work. But it's expected to be stable for most builds.)
	inOtherRevs := make(map[string]build.StepMetric)
	if outdirInfo != nil {
		for _, m := range outdirInfo.metrics {
			if step, ok := m.stepByOutput[stepData.Output]; ok {
				inOtherRevs[m.Rev] = *step
			}
		}
	}

	// We will only have special handling for a subset of stats, so provide the raw step for everything else.
	var stepRaw map[string]any
	asJSON, err := json.Marshal(stepData)
	if err != nil {
		s.renderBuildViewError(http.StatusInternalServerError, fmt.Sprintf("failed to marshal metrics: %v", err), w, r)
	}
	err = json.Unmarshal(asJSON, &stepRaw)
	if err != nil {
		s.renderBuildViewError(http.StatusInternalServerError, fmt.Sprintf("failed to unmarshal metrics: %v", err), w, r)
	}

	err = s.renderBuildView(w, r, tmpl, map[string]any{
		"step":        stepData,
		"stepRaw":     stepRaw,
		"inOtherRevs": inOtherRevs,
	})
	if err != nil {
		s.renderBuildViewError(http.StatusInternalServerError, fmt.Sprintf("failed to render view: %v", err), w, r)
	}
}

func (s *WebuiServer) handleOutdirListSteps(w http.ResponseWriter, r *http.Request) {
	var metrics *buildMetrics
	var outdirInfo *outdirInfo
	var err error
	if didRequestUploadedMetrics(r) {
		for _, m := range s.uploadedMetrics {
			if m.Rev == r.PathValue("rev") {
				metrics = m
				break
			}
		}
	} else {
		outdirInfo, err = s.getOutdirForRequest(r)
		if err != nil {
			s.renderBuildViewError(http.StatusNotFound, fmt.Sprintf("outdir failed to load for request %s: %v", r.URL, err), w, r)
			return
		}
		for _, m := range outdirInfo.metrics {
			if m.Rev == r.PathValue("rev") {
				metrics = m
				break
			}
		}
	}
	if metrics == nil {
		s.renderBuildViewError(http.StatusNotFound, fmt.Sprintf("no metrics found for request %s", r.URL), w, r)
		return
	}

	tmpl, err := s.loadView("_steps.html")
	if err != nil {
		s.renderBuildViewError(http.StatusInternalServerError, fmt.Sprintf("failed to load view: %s", err), w, r)
		return
	}

	actionsWanted := r.URL.Query()["action"]
	rulesWanted := r.URL.Query()["rule"]
	view := r.URL.Query().Get("view")
	outputSearch := r.URL.Query().Get("q")

	sortBy := "ready"
	sortDescending := false
	sortSupported := true
	sortParam := r.URL.Query().Get("sort")
	if sortParamRe.MatchString(sortParam) {
		matches := sortParamRe.FindStringSubmatch(sortParam)
		sortBy = matches[sortParamRe.SubexpIndex("sortBy")]
		if matches[sortParamRe.SubexpIndex("order")] == "Dsc" {
			sortDescending = true
		}
	} else if len(sortParam) > 0 {
		s.renderBuildViewError(http.StatusBadRequest, fmt.Sprintf("invalid sort param: %s", sortParam), w, r)
		return
	}

	var filteredSteps []*build.StepMetric
	switch view {
	case "criticalPath":
		sortSupported = false
		// We assume the last step is on the critical path.
		// Build the critical path backwards then reverse it.
		critStepID := metrics.lastStepID
		for critStepID != "" {
			if step, ok := metrics.stepByStepID[critStepID]; ok {
				filteredSteps = append(filteredSteps, step)
				critStepID = step.PrevStepID
			} else {
				// TODO(b/349287453): add some sort of error to indicate prev step was not found
				break
			}
		}
		slices.Reverse(filteredSteps)
	default:
		for _, m := range metrics.StepMetrics {
			if len(actionsWanted) > 0 && !slices.Contains(actionsWanted, m.Action) {
				continue
			}
			if len(rulesWanted) > 0 && !slices.Contains(rulesWanted, m.Rule) {
				continue
			}
			if outputSearch != "" && !strings.Contains(m.Output, outputSearch) {
				continue
			}
			if view == "localOnly" && !m.IsLocal {
				continue
			}
			filteredSteps = append(filteredSteps, m)
		}
		switch sortBy {
		case "ready":
			slices.SortFunc(filteredSteps, func(a, b *build.StepMetric) int {
				return cmp.Compare(a.Ready, b.Ready)
			})
		case "duration":
			slices.SortFunc(filteredSteps, func(a, b *build.StepMetric) int {
				return cmp.Compare(a.Duration, b.Duration)
			})
		case "completion":
			slices.SortFunc(filteredSteps, func(a, b *build.StepMetric) int {
				return cmp.Compare(a.Ready+a.Duration, b.Ready+b.Duration)
			})
		default:
			s.renderBuildViewError(http.StatusBadRequest, fmt.Sprintf("unknown sort column: %s", sortBy), w, r)
			return
		}
		if sortDescending {
			slices.Reverse(filteredSteps)
		}
	}

	itemsPerPage, err := strconv.Atoi(r.URL.Query().Get("items_per_page"))
	if err != nil {
		itemsPerPage = DefaultItemsPerPage
	}
	requestedPage, err := strconv.Atoi(r.URL.Query().Get("page"))
	if err != nil {
		requestedPage = 0
	}
	pageCount := len(filteredSteps) / itemsPerPage
	if len(filteredSteps)%itemsPerPage > 0 {
		pageCount++
	}

	pageFirst := 0
	pageLast := max(pageCount-1, 0)
	pageIndex := max(0, min(requestedPage, pageLast))
	pageNext := min(pageIndex+1, pageLast)
	pagePrev := max(0, pageIndex-1)
	itemsFirst := pageIndex * itemsPerPage
	itemsLast := max(0, min(itemsFirst+itemsPerPage, len(filteredSteps)))
	subset := filteredSteps[itemsFirst:itemsLast]

	data := map[string]any{
		"subset":           subset,
		"outputSearch":     outputSearch,
		"page":             requestedPage,
		"pageIndex":        pageIndex,
		"pageFirst":        pageFirst,
		"pageNext":         pageNext,
		"pagePrev":         pagePrev,
		"pageLast":         pageLast,
		"pageCount":        pageCount,
		"itemFirstLogical": itemsFirst + 1,
		"itemLastLogical":  itemsFirst + len(subset),
		"itemsLen":         len(filteredSteps),
		"actionCounts":     metrics.actionCounts,
		"ruleCounts":       metrics.ruleCounts,
		"buildDuration":    metrics.buildDuration,
		"sortSupported":    sortSupported,
	}
	err = s.renderBuildView(w, r, tmpl, data)
	if err != nil {
		s.renderBuildViewError(http.StatusInternalServerError, fmt.Sprintf("failed to render view: %v", err), w, r)
	}
}

func (s *WebuiServer) handleOutdirListTargets(w http.ResponseWriter, r *http.Request) {
	outdirInfo, err := s.getOutdirForRequest(r)
	if err != nil {
		s.renderBuildViewError(http.StatusNotFound, fmt.Sprintf("outdir failed to load for request %s: %v", r.URL, err), w, r)
		return
	}

	target := r.PathValue("target")
	if target == "" {
		s.renderBuildViewError(http.StatusNotFound, "missing target", w, r)
		return
	}

	// Check manifest or manifest.stamp is newer to reload.
	// Don't continue if failed to stat manifest, but ignore if manifest.stamp failed to stat.
	stat, err := os.Stat(filepath.Join(outdirInfo.path, outdirInfo.manifestPath))
	if err != nil {
		s.renderBuildViewError(http.StatusInternalServerError, fmt.Sprintf("failed to stat %s: %v", outdirInfo.manifestPath, err), w, r)
		return
	}
	buildNinjaMtime := stat.ModTime()
	stat, err = os.Stat(filepath.Join(outdirInfo.path, fmt.Sprintf("%s.stamp", outdirInfo.manifestPath)))
	if err == nil && stat.ModTime().After(buildNinjaMtime) {
		buildNinjaMtime = stat.ModTime()
	}
	outdirInfo.mu.Lock()
	defer outdirInfo.mu.Unlock()
	if buildNinjaMtime.After(outdirInfo.manifestMtime) {
		state := ninjautil.NewState()
		p := ninjautil.NewManifestParser(state)
		p.SetWd(outdirInfo.path)
		err = p.Load(r.Context(), outdirInfo.manifestPath)
		if err != nil {
			s.renderBuildViewError(http.StatusInternalServerError, fmt.Sprintf("failed to load manifest: %v", err), w, r)
			return
		}
		outdirInfo.ninjaState = state
		outdirInfo.manifestMtime = stat.ModTime()
	}

	// Use cached *ninjautil.State to read info.
	nodes, err := outdirInfo.ninjaState.Targets([]string{target})
	if err != nil {
		s.renderBuildViewError(http.StatusInternalServerError, fmt.Sprintf("failed to get node for target %s: %v", target, err), w, r)
		return
	}
	if len(nodes) != 1 {
		s.renderBuildViewError(http.StatusInternalServerError, fmt.Sprintf("unexpectedly got %d nodes querying target %s: %v", len(nodes), target, err), w, r)
		return
	}
	targetNode := nodes[0]
	var rule string
	var inputs []string
	var outputs []string
	inputType := make(map[string]string)
	if edge, ok := targetNode.InEdge(); ok {
		rule = edge.RuleName()
		for _, input := range edge.Inputs() {
			inputs = append(inputs, input.Path())
			// n = len(inputs), this is 2*O(n*2) check. but seems acceptable speed even for target="all"?
			if !slices.Contains(edge.Ins(), input) {
				if slices.Contains(edge.TriggerInputs(), input) {
					inputType[input.Path()] = "implicit"
				} else {
					inputType[input.Path()] = "order-only"
				}
			}
		}
	}
	for _, edge := range targetNode.OutEdges() {
		outs := edge.Outputs()
		if len(outs) == 0 {
			continue
		}
		outputs = append(outputs, outs[0].Path())
	}
	slices.Sort(inputs)
	slices.Sort(outputs)

	tmpl, err := s.loadView("_targets.html")
	if err != nil {
		s.renderBuildViewError(http.StatusInternalServerError, fmt.Sprintf("failed to load view: %s", err), w, r)
		return
	}

	err = s.renderBuildView(w, r, tmpl, map[string]any{
		"target":    target,
		"rule":      rule,
		"inputs":    inputs,
		"inputType": inputType,
		"outputs":   outputs,
	})
	if err != nil {
		s.renderBuildViewError(http.StatusInternalServerError, fmt.Sprintf("failed to render view: %v", err), w, r)
	}
}
