// Copyright 2024 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package webui implements siso webui.
package webui

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"text/template"
	"time"

	"go.chromium.org/build/siso/build"
	mwc "go.chromium.org/build/siso/third_party/material_web_components"
)

//go:embed *.html css/*.css
var content embed.FS

var (
	templates     = make(map[string]*template.Template)
	baseFunctions = template.FuncMap{
		"pathEscape": func(s string) string {
			return url.PathEscape(s)
		},
		"urlPathEq": func(url *url.URL, path string) bool {
			return url.Path == path
		},
		"urlPathHasPrefix": func(url *url.URL, prefix string) bool {
			return strings.HasPrefix(url.Path, prefix)
		},
		"urlParamGet": func(url *url.URL, key string) string {
			return url.Query().Get(key)
		},
		"urlParamEq": func(url *url.URL, key, value string) bool {
			return url.Query().Get(key) == value
		},
		"urlHasParam": func(url *url.URL, key, value string) bool {
			return slices.Contains(url.Query()[key], value)
		},
		"urlParamIsSet": func(url *url.URL, key string) bool {
			return len(url.Query()[key]) > 0
		},
		"urlParamReplace": func(url *url.URL, key, value string) *url.URL {
			query := url.Query()
			query.Set(key, value)
			url.RawQuery = query.Encode()
			return url
		},
		"divIntervalsScaled": func(a, b build.IntervalMetric, scale float64) float64 {
			return float64(a) / float64(b) * scale
		},
		"addIntervals": func(a, b build.IntervalMetric) build.IntervalMetric {
			return a + b
		},
		"subIntervals": func(a, b build.IntervalMetric) build.IntervalMetric {
			return a - b
		},
		"trimPrefix": func(s, prefix string) string {
			return strings.TrimPrefix(s, prefix)
		},
		"formatIntervalMetricTimestamp": func(i build.IntervalMetric) string {
			d := time.Duration(i)
			minute := int(d.Minutes())
			second := int(d.Seconds()) % 60
			ms := d.Milliseconds() % 1000
			// Reduce precision to 2 digits
			ms = int64(math.Round(float64(ms) / 10))
			return fmt.Sprintf("%2dm%02d.%02ds", minute, second, ms)
		},
		"formatIntervalMetricHuman": func(i build.IntervalMetric) string {
			var sb strings.Builder
			d := time.Duration(i)
			ms := d.Milliseconds() % 1000
			if ms > 10 {
				d = d.Round(10 * time.Millisecond)
				mins := d.Truncate(1 * time.Minute)
				d = d - mins
				if mins > 0 {
					fmt.Fprintf(&sb, "%s", strings.TrimSuffix(mins.String(), "0s"))
					if d < 10*time.Second {
						fmt.Fprint(&sb, "0")
					}
				}
				fmt.Fprintf(&sb, "%2.02fs", d.Seconds())
			} else {
				d = d.Round(10 * time.Microsecond)
				us := d.Microseconds() % 1000
				// Reduce precision to 2 digits
				us = int64(math.Round(float64(us) / 10))
				fmt.Fprintf(&sb, "%d.%02dms", ms, us)
			}
			return sb.String()
		},
		"buildTimeHumanReadable": func(m *buildMetrics) (string, error) {
			local, err := time.LoadLocation("Local")
			if err != nil {
				return "", fmt.Errorf("couldn't get local time location")
			}
			now := time.Now()
			buildTimeLocal := m.Mtime.In(local)
			nowY, nowM, nowD := now.Date()
			buildY, buildM, buildD := buildTimeLocal.Date()
			if buildY == nowY && buildM == nowM && buildD == nowD {
				return buildTimeLocal.Format("Today 15:04"), nil
			} else if buildY == nowY && buildM == nowM && buildD == (nowD-1) {
				return buildTimeLocal.Format("Yesterday 15:04"), nil
			} else if buildY == nowY ||
				(buildY == nowY-1 && buildM > nowM) {
				return buildTimeLocal.Format("Jan _2 15:04"), nil
			}
			return buildTimeLocal.Format("Jan _2 2006 15:04"), nil
		},
		"timeRFC3339": func(t time.Time) string {
			return t.Format(time.RFC3339)
		},
	}
	sisoMetricsRe = regexp.MustCompile(`siso_metrics.(\d+).json`)
	sortParamRe   = regexp.MustCompile(`^(?P<sortBy>[a-z]+?)(?P<order>Asc|Dsc)$`)
)

type WebuiServer struct {
	sisoVersion       string
	localDevelopment  bool
	port              int
	templatesFS       fs.FS
	sseServer         *sseServer
	execRoot          string
	defaultOutdir     string
	defaultOutdirRoot string
	defaultOutdirSub  string
	outsubs           []string
	runbuildState

	metricsMu       sync.Mutex
	outdirMetrics   map[string]*outdirInfo
	uploadedMetrics []*buildMetrics
}

type runningStepInfo struct {
	stepOut  string
	stepType string
	started  time.Time
}

// ErrExecrootNotExist represents error when exec root was not found.
type ErrExecrootNotExist struct {
	err error
}

func (f ErrExecrootNotExist) Unwrap() error {
	return f.err
}

func (f ErrExecrootNotExist) Error() string {
	return fmt.Sprintf("failed to find execroot: %v", f.err)
}

// ErrManifestNotExist represents error when build manifest was not found.
type ErrManifestNotExist struct {
	outdirPath   string
	manifestPath string
}

func (f ErrManifestNotExist) Error() string {
	return fmt.Sprintf("%s not found in %s", f.manifestPath, f.outdirPath)
}

// loadView lazy-parses a view once, or parses every time if in local development mode.
func (s *WebuiServer) loadView(view string) (*template.Template, error) {
	if template, ok := templates[view]; ok {
		return template, nil
	}
	template, err := template.New("").Funcs(baseFunctions).ParseFS(s.templatesFS, "base.html", view)
	if err != nil {
		return nil, fmt.Errorf("failed to parse view: %w", err)
	}
	if !s.localDevelopment {
		templates[view] = template
	}
	return template, nil
}

func didRequestUploadedMetrics(r *http.Request) bool {
	return r.PathValue("outroot") == "uploads" && r.PathValue("outsub") == "view"
}

// baseURLFromRequest gets the base URL from context.
// This is a HARDCODED assumption that siso webui only has routes that start with outdir.
func outdirBaseURL(r *http.Request) string {
	return fmt.Sprintf("/%s/%s", url.PathEscape(r.PathValue("outroot")), url.PathEscape(r.PathValue("outsub")))
}

// renderBuildView renders a build-related view.
// TODO(b/361703735): return data instead of write to response writer? https://chromium-review.googlesource.com/c/infra/infra/+/5803123/comment/4ce69ada_31730349/
func (s *WebuiServer) renderBuildView(wr http.ResponseWriter, r *http.Request, tmpl *template.Template, data map[string]any) error {
	rev := r.PathValue("rev")
	if didRequestUploadedMetrics(r) {
		data["viewingUploaded"] = true
	} else if outdirInfo, err := s.getOutdirForRequest(r); err == nil {
		if rev == "" {
			rev = outdirInfo.latestRevID
		}
		data["outroot"] = outdirInfo.outroot
		data["outsub"] = outdirInfo.outsub
		outdirAbbrev := outdirInfo.path
		// Showing the full path is too long in the webui so abbreviate home dir to ~.
		// TODO(b/361703735): refactor https://chromium-review.googlesource.com/c/infra/infra/+/5804478/comment/dcfb372d_f21e4cc5/
		if home, err := os.UserHomeDir(); err == nil {
			outdirAbbrev = strings.Replace(outdirAbbrev, home, "~", 1)
		}
		data["outdirAbbrev"] = outdirAbbrev
		data["outdirRel"] = outdirInfo.pathRel
		data["revs"] = outdirInfo.metrics
	}
	data["outsubs"] = s.outsubs
	data["versionID"] = s.sisoVersion
	data["currentURL"] = r.URL
	data["currentRev"] = rev
	data["outdirBaseURL"] = outdirBaseURL(r)
	if rev != "" {
		data["outdirRevBaseURL"] = fmt.Sprintf("%s/builds/%s", data["outdirBaseURL"], rev)
	}
	err := tmpl.ExecuteTemplate(wr, "base", data)
	if err != nil {
		return fmt.Errorf("failed to execute template: %w", err)
	}
	return nil
}

// renderBuildViewError renders a build-related error.
func (s *WebuiServer) renderBuildViewError(status int, message string, w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(status)
	tmpl, err := s.loadView("_error.html")
	if err != nil {
		fmt.Fprintf(w, "failed to load error view: %s\n", err)
		return
	}
	err = s.renderBuildView(w, r, tmpl, map[string]any{
		"errorTitle":   http.StatusText(status),
		"errorMessage": message,
	})
	if err != nil {
		fmt.Fprintf(w, "failed to render error view: %s\n", err)
	}
}

// NewServer inits a webui server.
func NewServer(version string, localDevelopment bool, port int, defaultOutdir, configRepoDir, manifestPath string) (*WebuiServer, error) {
	s := WebuiServer{
		sisoVersion:      version,
		localDevelopment: localDevelopment,
		templatesFS:      fs.FS(content),
		sseServer:        newSseServer(),
		defaultOutdir:    defaultOutdir,
		outdirMetrics:    make(map[string]*outdirInfo),
		port:             port,
	}
	if localDevelopment {
		s.templatesFS = os.DirFS("webui/")
	}

	// Get execroot.
	var err error
	s.execRoot, err = build.DetectExecRoot(defaultOutdir, configRepoDir)
	if err != nil {
		return nil, &ErrExecrootNotExist{err}
	}

	// Preload default outdir.
	defaultOutdirInfo, err := loadOutdirInfo(s.execRoot, defaultOutdir, manifestPath)
	if err != nil {
		return nil, fmt.Errorf("failed to preload outdir: %w", err)
	}
	s.outdirMetrics[defaultOutdir] = defaultOutdirInfo
	s.defaultOutdirRoot = defaultOutdirInfo.outroot
	s.defaultOutdirSub = defaultOutdirInfo.outsub

	// Find other outdirs.
	// TODO: support out*/*
	// TODO(b/361703735): can use defaultOutdirParent?
	matches, err := filepath.Glob(filepath.Join(s.execRoot, "out/*"))
	if err != nil {
		return nil, fmt.Errorf("failed to glob %s: %w", s.execRoot, err)
	}
	for _, match := range matches {
		m, err := os.Stat(match)
		if err != nil {
			return nil, fmt.Errorf("failed to stat outdir %s: %w", match, err)
		}
		if m.IsDir() {
			outsub, err := filepath.Rel(filepath.Join(s.execRoot, defaultOutdirInfo.outroot), match)
			if err != nil {
				return nil, fmt.Errorf("failed to make %s execroot relative: %w", match, err)
			}
			s.outsubs = append(s.outsubs, outsub)
		}
	}

	return &s, nil
}

func (s *WebuiServer) LoadStandaloneMetrics(metricsPath string) error {
	metrics, err := loadBuildMetrics(metricsPath)
	if err != nil {
		return fmt.Errorf("failed to load metrics: %w", err)
	}
	s.metricsMu.Lock()
	s.uploadedMetrics = append(s.uploadedMetrics, metrics)
	s.metricsMu.Unlock()
	return nil
}

func (s *WebuiServer) staticFileHandler(h http.Handler) http.Handler {
	if s.localDevelopment {
		return h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Cache-Control", "max-age=86400, private")
		h.ServeHTTP(w, r)
	})
}

func (s *WebuiServer) Serve() int {
	s.sseServer.Start()
	http.Handle("/events/", s.sseServer)

	// Subrouter for all outdir related URLs.
	// This is set up on a separate mux because it's too generic and would otherwise cause panic:
	//     /css/ and /{outroot}/{outsub}/ both match some paths, like "/css/outsub/".
	//     But neither is more specific than the other.
	//     /css/ matches "/css/", but /{outroot}/{outsub}/ doesn't.
	//     /{outroot}/{outsub}/ matches "/outroot/outsub/", but /css/ doesn't.
	outdirRouter := http.NewServeMux()
	outdirRouter.HandleFunc("/{outroot}/{outsub}/", s.handleOutdirRoot)
	outdirRouter.HandleFunc("/{outroot}/{outsub}/builds/{rev}/logs/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, fmt.Sprintf("%s/builds/%s/logs/.siso_config", outdirBaseURL(r), url.PathEscape(r.PathValue("rev"))), http.StatusTemporaryRedirect)
	})
	outdirRouter.HandleFunc("/{outroot}/{outsub}/targets/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, fmt.Sprintf("%s/targets/all/", outdirBaseURL(r)), http.StatusTemporaryRedirect)
	})
	outdirRouter.HandleFunc("/{outroot}/{outsub}/runbuild/", s.handleRunbuildGet)
	outdirRouter.HandleFunc("POST /{outroot}/{outsub}/runbuild/", s.handleRunbuildPost)
	outdirRouter.HandleFunc("/{outroot}/{outsub}/reload", s.handleOutdirReload)
	outdirRouter.HandleFunc("/{outroot}/{outsub}/builds/{rev}/logs/{file}", s.handleOutdirViewLog)
	outdirRouter.HandleFunc("/{outroot}/{outsub}/builds/{rev}/aggregates/", s.handleOutdirAggregates)
	outdirRouter.HandleFunc("POST /{outroot}/{outsub}/builds/{rev}/steps/{id}/recall/", s.handleOutdirDoRecall)
	outdirRouter.HandleFunc("/{outroot}/{outsub}/builds/{rev}/steps/{id}/", s.handleOutdirViewStep)
	outdirRouter.HandleFunc("/{outroot}/{outsub}/builds/{rev}/steps/", s.handleOutdirListSteps)
	outdirRouter.HandleFunc("/{outroot}/{outsub}/targets/{target}/", s.handleOutdirListTargets)

	// Handlers for uploaded metrics.
	// We define these explicitly by catching all URLs starting with /uploads/view/, and defining a subset
	// of the /{outroot}/{outsub}/ routes so that only supported routes will load, and all others will 404.
	// (For example, it doesn't make sense to support /runbuild/ or /reload/ for uploaded metrics.)
	// Siso webui was created assuming /{outroot}/{outsub}/ with on-disk metrics rather than uploaded.
	// Furthermore, we also have hardcoded links built around this URL structure.
	// It would probably be more ideal to have just "/uploads/{rev}/builds/steps/", but it would require more refactoring.
	uploadsRouter := http.NewServeMux()
	uploadsRouter.HandleFunc("/uploads/view/", s.handleOutdirRoot)
	uploadsRouter.HandleFunc("/uploads/view/builds/{rev}/aggregates/", s.handleOutdirAggregates)
	uploadsRouter.HandleFunc("POST /uploads/view/builds/{rev}/steps/{id}/recall/", s.handleOutdirDoRecall)
	uploadsRouter.HandleFunc("/uploads/view/builds/{rev}/steps/{id}/", s.handleOutdirViewStep)
	uploadsRouter.HandleFunc("/uploads/view/builds/{rev}/steps/", s.handleOutdirListSteps)
	http.HandleFunc("/uploads/view/", func(w http.ResponseWriter, r *http.Request) {
		// This is how we hack around the hardcoded assumption that URLs are /{outroot}/{outsub}/.
		// Common code paths will then have checks to handle this special case.
		r.SetPathValue("outroot", "uploads")
		r.SetPathValue("outsub", "view")
		uploadsRouter.ServeHTTP(w, r)
	})

	// Default catch-all handler.
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Redirect root to default outdir.
		if r.URL.Path == "/" {
			http.Redirect(w, r, fmt.Sprintf("/%s/%s/", s.defaultOutdirRoot, s.defaultOutdirSub), http.StatusTemporaryRedirect)
			return
		}

		// Delegate all other requests to the outdir subrouter.
		outdirRouter.ServeHTTP(w, r)
	})

	http.Handle("/css/", s.staticFileHandler(http.FileServerFS(s.templatesFS)))

	// Serve third party JS. No other third party libraries right now, so just serve Material Design node_modules root.
	http.Handle("/third_party/", http.StripPrefix("/third_party/", s.staticFileHandler(http.FileServerFS(mwc.NodeModulesFS))))

	fmt.Printf("listening on http://localhost:%d/...\n", s.port)
	// Hack for now to make loading external siso_metrics.json more usable, until we can have the homepage automatically show "here's all the loaded siso_metrics"
	if len(s.uploadedMetrics) > 0 {
		fmt.Printf("for provided siso_metrics.json:\n")
		for _, metrics := range s.uploadedMetrics {
			fmt.Printf("- http://localhost:%d/uploads/view/builds/%s/steps/\n", s.port, metrics.Rev)
		}
	}
	err := http.ListenAndServe(fmt.Sprintf(":%d", s.port), nil)
	if errors.Is(err, http.ErrServerClosed) {
		fmt.Printf("server closed\n")
	} else if err != nil {
		fmt.Printf("error starting server: %s\n", err)
		return 1
	}
	return 0
}
