// Copyright 2026 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package webui

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/PuerkitoBio/goquery"
	"github.com/google/go-cmp/cmp"
)

func TestServer_MissingManifest(t *testing.T) {
	dir := t.TempDir()
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	_, err = NewServer(ServerConfig{
		Version:          "test-version",
		LocalDevelopment: false,
		Port:             8080,
		WorkspaceRoot:    dir,
		DefaultOutdir:    "non_existent_dir",
		ManifestPath:     "build.ninja",
	})

	if err == nil {
		t.Fatal("server got err = nil; want error")
	}
	if _, ok := errors.AsType[*ErrManifestNotExist](err); !ok {
		t.Fatalf("server got err = %v; want ErrManifestNotExist", err)
	}
}

func mustServer(t *testing.T) (*WebuiServer, string) {
	t.Helper()
	dir := t.TempDir()
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	if err := os.MkdirAll("build/config/siso", 0755); err != nil {
		t.Fatal(err)
	}

	// Chromium style outdir.
	if err := os.MkdirAll("out/Default", 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("out/Default/build.ninja", []byte(`rule cc
  command = clang++
rule link
  command = ld
build foo.o: cc foo.c
build foo: link foo.o
build all: phony foo`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("out/Default/siso_metrics.json", []byte(`{"build_id": "test-rev"}
{"step_id": "step-1", "rule": "cc", "action": "clang", "outputs": ["out1.o"]}
{"step_id": "step-multi", "rule": "action", "action": "generate", "outputs": ["gen/out1.pak", "gen/out2.pak", "gen/out3.pak"]}
`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("out/Default/siso_metadata.json", []byte(`
{
  "build_id": "test-rev",
  "siso_version": "0.0.1",
  "targets": ["all"],
  "machine": {"platform": {"os": "linux", "architecture": "amd64"}, "cpu": {"brand": "Intel CPU", "logical_cores": 8, "physical_cores": 4}, "memory": {"total": 17179869184}}
}`), 0644); err != nil {
		t.Fatal(err)
	}
	for file, content := range map[string]string{
		"out/Default/.siso_config":     "siso_config default",
		"out/Default/.siso_filegroups": "siso_filegroups default",
		"out/Default/siso_localexec":   "siso_localexec default",
		"out/Default/siso_output":      "siso_output default",
		"out/Default/siso_trace.json":  `{"trace": "default"}`,
	} {
		if err := os.WriteFile(file, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}

	// Chromium style outdir with characters that will be URL-encoded.
	if err := os.MkdirAll("out/Default final v2 (1)", 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("out/Default final v2 (1)/build.ninja", []byte(""), 0644); err != nil {
		t.Fatal(err)
	}

	// ChromiumOS style outdir.
	// https://chromium.googlesource.com/chromium/src/+/18f03121b045467feac4bcc30f51b7dbf28e5a64/docs/chromeos_build_instructions.md#building-for-the-board
	if err := os.MkdirAll("out_amd64-generic/Release", 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("out_amd64-generic/Release/build.ninja", []byte(""), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("out_amd64-generic/Release/siso_metrics.json", []byte(`{"build_id": "cros-rev"}
{"step_id": "step-1", "rule": "cc", "action": "clang", "outputs": ["out2.o"]}
`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("out_amd64-generic/Release/.siso_config", []byte("siso_config cros"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("out_amd64-generic/Release/siso_output", []byte("siso_output cros"), 0644); err != nil {
		t.Fatal(err)
	}

	// Outdir that is not scanned by default (glob for out*/* only).
	if err := os.MkdirAll("tmp-out/Default", 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("tmp-out/Default/build.ninja", []byte(""), 0644); err != nil {
		t.Fatal(err)
	}

	// Now init the server.
	s, err := NewServer(ServerConfig{
		Version:          "test-version",
		LocalDevelopment: false,
		Port:             8080,
		WorkspaceRoot:    dir,
		DefaultOutdir:    "out/Default",
		ManifestPath:     "build.ninja",
	})
	if err != nil {
		t.Fatalf("server err = %v; want nil err", err)
	}
	return s, dir
}

func TestServer_InitialState(t *testing.T) {
	s, tmp := mustServer(t)

	if s.workspaceRoot != tmp {
		t.Errorf("workspaceRoot = %q; want %q", s.workspaceRoot, tmp)
	}
	if s.defaultOutdir != "out/Default" {
		t.Errorf("defaultOutdir = %q; want %q", s.defaultOutdir, "out/Default")
	}
}

func TestRoutes_Outdirs(t *testing.T) {
	s, _ := mustServer(t)

	for _, tc := range []struct {
		path string
		want int
	}{
		{"/out/Default/builds/", http.StatusOK},
		{"/out/Default/builds/test-rev/steps/", http.StatusOK},
		{"/out/Default/builds/test-rev/steps_lit/", http.StatusOK},
		{"/out/Default/builds/test-rev/api/steps", http.StatusOK},
		{"/out/Default/builds/test-rev/api/invocation", http.StatusOK},
		{"/out/Default/builds/test-rev/api/steps/step-1", http.StatusOK},
		{"/out/Default/builds/test-rev/api/steps/step-0", http.StatusNotFound},
		{"/out/Default/builds/test-rev/steps/step-1/", http.StatusOK},
		{"/out/Default/builds/test-rev/steps/step-0/", http.StatusNotFound},
		{"/out/Default/builds/test-rev/steps_lit/step-1/", http.StatusOK},
		{"/out/Default/builds/test-rev/steps_lit/step-0/", http.StatusNotFound},
		{"/out/Default/builds/nonexistent-rev/steps/step-1/", http.StatusNotFound},
		{"/out/Default/builds/cros-rev/steps/step-1/", http.StatusNotFound},
		{"/out/Default/builds/test-rev/details/", http.StatusOK},
		{"/out/Default/builds/test-rev/details_lit/", http.StatusOK},
		{"/out/Default/builds/test-rev/aggregates/", http.StatusOK},
		{"/out/Default/builds/test-rev/aggregates_lit/", http.StatusOK},
		{"/out/Default/builds/test-rev/logs_lit/", http.StatusOK},
		{"/out/Default/targets/all/", http.StatusOK},
		{"/out/Default/targets/foo.o/", http.StatusOK},
		{"/out/Default/targets/nonexistent.o/", http.StatusNotFound},
		{"/out/Default/builds/test-rev/logs/.siso_config", http.StatusOK},
		{"/out/Default/builds/test-rev/logs/.siso_filegroups", http.StatusOK},
		{"/out/Default/builds/test-rev/logs/siso_localexec", http.StatusOK},
		{"/out/Default/builds/test-rev/logs/siso_output", http.StatusOK},
		{"/out/Default/builds/test-rev/logs/siso_trace.json", http.StatusOK},
		{"/out/Default/builds/test-rev/logs/unknown_file", http.StatusNotFound},
		{"/out/Default/builds/nonexistent-rev/logs/.siso_config", http.StatusNotFound},
		{"/out_amd64-generic/Release/builds/cros-rev/steps/step-1/", http.StatusOK},
		{"/out_amd64-generic/Release/builds/cros-rev/details/", http.StatusOK},
		{"/out_amd64-generic/Release/builds/cros-rev/logs/.siso_config", http.StatusOK},
		{"/out_amd64-generic/Release/builds/cros-rev/logs/siso_output", http.StatusOK},
	} {
		rec := httptest.NewRecorder()
		s.mux().ServeHTTP(rec, httptest.NewRequest("GET", tc.path, nil))
		if rec.Code != tc.want {
			t.Errorf("GET %s = %d; want %d", tc.path, rec.Code, tc.want)
		}
	}
}

func TestRoutes_InvocationSeriesList(t *testing.T) {
	s, _ := mustServer(t)

	rec := httptest.NewRecorder()
	s.mux().ServeHTTP(rec, httptest.NewRequest("GET", "/out/Default/builds/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /out/Default/builds/ = %d; want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "test-rev") {
		t.Errorf("body missing test-rev: %s", body)
	}
	if !strings.Contains(body, "Invocations") {
		t.Errorf("body missing Invocations title: %s", body)
	}
}

func TestRoutes_UploadedMetrics(t *testing.T) {
	s, _ := mustServer(t)

	tmp := t.TempDir()
	metricsFile := filepath.Join(tmp, "my_custom_metrics.json")
	if err := os.WriteFile(metricsFile, []byte(`{"build_id": "uploaded-build-id"}
{"step_id": "step-1", "rule": "cc", "action": "clang", "outputs": ["out.o"]}
`), 0644); err != nil {
		t.Fatalf("failed to write standalone metrics: %v", err)
	}
	if err := s.LoadStandaloneMetrics(metricsFile); err != nil {
		t.Fatalf("failed to load standalone metrics: %v", err)
	}

	for _, tc := range []struct {
		path string
		want int
	}{
		{"/uploads/view/builds/", http.StatusOK},
		{"/uploads/view/builds/uploaded-build-id/steps/", http.StatusOK},
		{"/uploads/view/builds/uploaded-build-id/steps_lit/", http.StatusOK},
		{"/uploads/view/builds/uploaded-build-id/api/steps", http.StatusOK},
		{"/uploads/view/builds/uploaded-build-id/api/invocation", http.StatusOK},
		{"/uploads/view/builds/uploaded-build-id/api/steps/step-1", http.StatusOK},
		{"/uploads/view/builds/uploaded-build-id/api/steps/step-0", http.StatusNotFound},
		{"/uploads/view/builds/uploaded-build-id/steps/step-1/", http.StatusOK},
		{"/uploads/view/builds/uploaded-build-id/steps_lit/step-1/", http.StatusOK},
		{"/uploads/view/builds/uploaded-build-id/steps_lit/step-0/", http.StatusNotFound},
		{"/uploads/view/builds/uploaded-build-id/details/", http.StatusOK},
		{"/uploads/view/builds/uploaded-build-id/details_lit/", http.StatusOK},
		{"/uploads/view/builds/nonexistent-rev/steps/", http.StatusNotFound},
		{"/uploads/view/builds/uploaded-build-id/aggregates/", http.StatusOK},
		{"/uploads/view/builds/uploaded-build-id/aggregates_lit/", http.StatusOK},
	} {
		rec := httptest.NewRecorder()
		s.mux().ServeHTTP(rec, httptest.NewRequest("GET", tc.path, nil))
		if rec.Code != tc.want {
			t.Errorf("GET %s = %d; want %d", tc.path, rec.Code, tc.want)
		}
	}
}

func TestRedirects(t *testing.T) {
	s, _ := mustServer(t)

	for _, tc := range []struct {
		path string
		want string
	}{
		{"/", "/out/Default/"},
		{"/out/Default/", "/out/Default/builds/test-rev/steps/"},
		{"/out/Default/targets/", "/out/Default/targets/all/"},
		{"/out/Default/reload", "/out/Default"},
		{"/out/Default/builds/test-rev/logs/", "/out/Default/builds/test-rev/logs/.siso_config"},
		{"/out_amd64-generic/Release/builds/cros-rev/logs/", "/out_amd64-generic/Release/builds/cros-rev/logs/.siso_config"},
	} {
		rec := httptest.NewRecorder()
		s.mux().ServeHTTP(rec, httptest.NewRequest("GET", tc.path, nil))
		if rec.Code != http.StatusTemporaryRedirect {
			t.Errorf("GET %s = %d; want %d", tc.path, rec.Code, http.StatusTemporaryRedirect)
			continue
		}
		if got := rec.Header().Get("Location"); got != tc.want {
			t.Errorf("GET %s redirect = %q; want %q", tc.path, got, tc.want)
		}
	}
}

func TestRoutes_ViewLog(t *testing.T) {
	s, _ := mustServer(t)

	for _, tc := range []struct {
		path, want string
	}{
		{"/out/Default/builds/test-rev/logs/siso_output", "siso_output default"},
		{"/out_amd64-generic/Release/builds/cros-rev/logs/siso_output", "siso_output cros"},
	} {
		// Test the HTML page.
		t.Run(tc.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			s.mux().ServeHTTP(rec, httptest.NewRequest("GET", tc.path, nil))
			if rec.Code != http.StatusOK {
				t.Errorf("GET %s = %d; want %d", tc.path, rec.Code, http.StatusOK)
			}
			if !strings.Contains(rec.Body.String(), tc.want) {
				t.Errorf("GET %s body = %q; want it to contain %q", tc.path, rec.Body.String(), tc.want)
			}
		})

		// Test the raw file.
		t.Run(tc.path+"?raw=true", func(t *testing.T) {
			rec := httptest.NewRecorder()
			s.mux().ServeHTTP(rec, httptest.NewRequest("GET", tc.path+"?raw=true", nil))
			if rec.Code != http.StatusOK {
				t.Errorf("GET %s = %d; want %d", tc.path, rec.Code, http.StatusOK)
			}
			if got := rec.Body.String(); got != tc.want {
				t.Errorf("GET %s body = %q; want %q", tc.path, got, tc.want)
			}
		})
	}
}

// Test that outdir URLs are rendered correctly.
// We apply custom escaping in the template, so unit testing the data model is inadequate.
// TODO: test for android-style outdirs as well?
func TestOutdirMenu_RendersCorrectURLs(t *testing.T) {
	s, _ := mustServer(t)
	rec := httptest.NewRecorder()
	s.mux().ServeHTTP(rec, httptest.NewRequest("GET", "/out/Default/builds/test-rev/steps/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET steps = %d; want 200", rec.Code)
	}

	items := make(map[string]string)
	doc, err := goquery.NewDocumentFromReader(rec.Body)
	if err != nil {
		t.Fatalf("failed to parse page: %v", err)
	}
	doc.Find("#invocation-series-menu md-menu-item").Each(func(_ int, menuItem *goquery.Selection) {
		href, _ := menuItem.Attr("href")
		name := strings.TrimSpace(menuItem.Text())
		items[name] = href
	})

	// The slash between out/Default must be left unescaped.
	want := map[string]string{
		"out/Default":               "/out/Default/",
		"out/Default final v2 (1)":  "/out/Default%20final%20v2%20%281%29/",
		"out_amd64-generic/Release": "/out_amd64-generic/Release/",
		// Other outdirs not included (glob for out*/* only).
	}
	if diff := cmp.Diff(want, items); diff != "" {
		t.Errorf("outdir menu mismatch (-want +got):\n%s", diff)
	}
}

func TestBreadcrumbs(t *testing.T) {
	s, tmp := mustServer(t)

	// Since the outdir path is absolute and dynamically generated via t.TempDir(),
	// we determine the expected outdir abbrev label dynamically.
	outdirAbbrev := filepath.Join(tmp, "out/Default")

	for _, tc := range []struct {
		path string
		want []string
	}{
		{"/out/Default/builds/", []string{outdirAbbrev, "Invocations"}},
		{"/out/Default/builds/test-rev/steps/", []string{outdirAbbrev, "Invocations", "test-rev", "Build Steps"}},
		{"/out/Default/builds/test-rev/steps_lit/", []string{outdirAbbrev, "Invocations", "test-rev", "Build Steps (Lit)"}},
		{"/out/Default/builds/test-rev/steps/step-1/", []string{outdirAbbrev, "Invocations", "test-rev", "Build Steps", "out1.o"}},
		{"/out/Default/builds/test-rev/steps_lit/step-1/", []string{outdirAbbrev, "Invocations", "test-rev", "Build Steps (Lit)", "out1.o"}},
		{"/out/Default/builds/test-rev/details/", []string{outdirAbbrev, "Invocations", "test-rev", "Details"}},
		{"/out/Default/builds/test-rev/details_lit/", []string{outdirAbbrev, "Invocations", "test-rev", "Details (Lit)"}},
		{"/out/Default/builds/test-rev/aggregates/", []string{outdirAbbrev, "Invocations", "test-rev", "Aggregates"}},
		{"/out/Default/builds/test-rev/aggregates_lit/", []string{outdirAbbrev, "Invocations", "test-rev", "Aggregates (Lit)"}},
		{"/out/Default/builds/test-rev/logs_lit/", []string{outdirAbbrev, "Invocations", "test-rev", "Raw Logs (Lit)"}},
		{"/out/Default/targets/all/", []string{outdirAbbrev, "Targets", "all"}},
		{"/out/Default/targets/foo.o/", []string{outdirAbbrev, "Targets", "foo.o"}},
		{"/out/Default/builds/test-rev/logs/.siso_config", []string{outdirAbbrev, "Invocations", "test-rev", "Raw Logs", ".siso_config"}},
		{"/out/Default/runbuild/", []string{outdirAbbrev, "Run Build"}},
		{"/out/Default/watch/", []string{outdirAbbrev, "Watch"}},
	} {
		t.Run(tc.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			s.mux().ServeHTTP(rec, httptest.NewRequest("GET", tc.path, nil))
			doc, err := goquery.NewDocumentFromReader(rec.Body)
			if err != nil {
				t.Fatalf("failed to parse HTML: %v", err)
			}

			var got []string
			doc.Find("#breadcrumbs .breadcrumb-label").Each(func(_ int, s *goquery.Selection) {
				got = append(got, strings.TrimSpace(s.Text()))
			})

			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("breadcrumbs mismatch for %s (-want +got):\n%s", tc.path, diff)
			}
		})
	}
}

func TestCrossOriginProtection(t *testing.T) {
	for _, tc := range []struct {
		name         string
		method       string
		path         string
		secFetchSite string
		origin       string
		host         string
		want         int
	}{
		{
			name:         "safe_methods_allowed_from_cross_origin",
			method:       "GET",
			path:         "/out/Default/builds/test-rev/steps/",
			secFetchSite: "cross-site",
			origin:       "http://attacker.example.com",
			want:         http.StatusOK,
		},
		{
			name:         "post_rejected_with_cross_site_sec_fetch_site",
			method:       "POST",
			path:         "/out/Default/builds/test-rev/steps/step-1/recall/",
			secFetchSite: "cross-site",
			want:         http.StatusForbidden,
		},
		{
			name:         "post_allowed_with_same_origin_sec_fetch_site",
			method:       "POST",
			path:         "/out/Default/builds/test-rev/steps/step-1/recall/",
			secFetchSite: "same-origin",
			want:         http.StatusOK,
		},
		{
			name:   "post_rejected_with_mismatching_origin",
			method: "POST",
			path:   "/out/Default/builds/test-rev/steps/step-1/recall/",
			host:   "localhost:8080",
			origin: "http://attacker.example.com",
			want:   http.StatusForbidden,
		},
		{
			name:   "post_allowed_with_matching_origin",
			method: "POST",
			path:   "/out/Default/builds/test-rev/steps/step-1/recall/",
			host:   "localhost:8080",
			origin: "http://localhost:8080",
			want:   http.StatusOK,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := mustServer(t)

			req := httptest.NewRequest(tc.method, tc.path, nil)
			if tc.secFetchSite != "" {
				req.Header.Set("Sec-Fetch-Site", tc.secFetchSite)
			}
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			if tc.host != "" {
				req.Host = tc.host
			}

			rec := httptest.NewRecorder()
			s.mux().ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Errorf("%s %s = %d; want %d", tc.method, tc.path, rec.Code, tc.want)
			}
		})
	}
}

func TestInvocationStepOutputs(t *testing.T) {
	s, _ := mustServer(t)

	// Single output case.
	{
		rec := httptest.NewRecorder()
		s.mux().ServeHTTP(rec, httptest.NewRequest("GET", "/out/Default/builds/test-rev/steps/step-1/", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET step-1 = %d; want 200", rec.Code)
		}
		doc, err := goquery.NewDocumentFromReader(rec.Body)
		if err != nil {
			t.Fatalf("failed to parse HTML: %v", err)
		}
		text := doc.Find(".step-details").Text()
		if !strings.Contains(text, "1 output(s)") {
			t.Errorf("expected '1 output(s)' in step-details, got: %s", text)
		}
		if strings.Contains(text, "[out1.o]") {
			t.Errorf("step-details should not contain raw slice string '[out1.o]', got: %s", text)
		}
		if doc.Find(".step-outputs-details").Length() != 0 {
			t.Errorf("expected no .step-outputs-details for single output, found %d", doc.Find(".step-outputs-details").Length())
		}
	}

	// Multiple outputs case.
	{
		rec := httptest.NewRecorder()
		s.mux().ServeHTTP(rec, httptest.NewRequest("GET", "/out/Default/builds/test-rev/steps/step-multi/", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET step-multi = %d; want 200", rec.Code)
		}
		doc, err := goquery.NewDocumentFromReader(rec.Body)
		if err != nil {
			t.Fatalf("failed to parse HTML: %v", err)
		}
		text := doc.Find(".step-details").Text()
		if !strings.Contains(text, "3 output(s)") {
			t.Errorf("expected '3 output(s)' in step-details, got: %s", text)
		}
		if strings.Contains(text, "[gen/out1.pak") {
			t.Errorf("step-details should not contain raw slice string, got: %s", text)
		}
		details := doc.Find(".step-outputs-details")
		if details.Length() != 1 {
			t.Fatalf("expected 1 .step-outputs-details for multiple outputs, found %d", details.Length())
		}
		var outputs []string
		details.Find("li code").Each(func(_ int, sel *goquery.Selection) {
			outputs = append(outputs, sel.Text())
		})
		wantOutputs := []string{"gen/out1.pak", "gen/out2.pak", "gen/out3.pak"}
		if !slices.Equal(outputs, wantOutputs) {
			t.Errorf("outputs = %v; want %v", outputs, wantOutputs)
		}
	}
}

func TestAPIInvocationSteps(t *testing.T) {
	s, _ := mustServer(t)
	rec := httptest.NewRecorder()
	s.mux().ServeHTTP(rec, httptest.NewRequest("GET", "/out/Default/builds/test-rev/api/steps", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/steps = %d; want 200", rec.Code)
	}

	contentType := rec.Header().Get("Content-Type")
	if !strings.HasPrefix(contentType, "application/json") {
		t.Errorf("Content-Type = %q; want application/json", contentType)
	}

	var data apiInvocationStepsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &data); err != nil {
		t.Fatalf("failed to decode JSON response: %v", err)
	}

	if len(data.Steps) == 0 {
		t.Errorf("expected non-empty steps in API response")
	}
	if data.BuildDurationNanos < 0 {
		t.Errorf("expected non-negative BuildDurationNanos, got %d", data.BuildDurationNanos)
	}
}

func TestAPIInvocation(t *testing.T) {
	s, _ := mustServer(t)
	rec := httptest.NewRecorder()
	s.mux().ServeHTTP(rec, httptest.NewRequest("GET", "/out/Default/builds/test-rev/api/invocation", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/invocation = %d; want 200", rec.Code)
	}

	contentType := rec.Header().Get("Content-Type")
	if !strings.HasPrefix(contentType, "application/json") {
		t.Errorf("Content-Type = %q; want application/json", contentType)
	}

	var data apiInvocationResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &data); err != nil {
		t.Fatalf("failed to decode JSON response: %v", err)
	}

	if data.BuildID != "test-rev" {
		t.Errorf("BuildID = %q; want %q", data.BuildID, "test-rev")
	}
	if data.TotalSteps != 2 {
		t.Errorf("TotalSteps = %d; want 2", data.TotalSteps)
	}
}
