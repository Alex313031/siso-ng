// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package version provides version subcommand.
package version

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/google/subcommands"

	"go.chromium.org/build/siso/version"
)

const cipdServiceURL = "https://chrome-infra-packages.appspot.com"

// Cmd returns the Command for the `version` subcommand.
func Cmd(ver string) *Command {
	return &Command{
		version: ver,
	}
}

func (*Command) Name() string {
	return "version"
}

func (*Command) Synopsis() string {
	return "prints the executable version"
}

func (*Command) Usage() string {
	return "Prints the executable version and the CIPD package the executable was installed from (if it was installed via CIPD)."
}

// Command implements version subcommand.
type Command struct {
	version string
	cipdURL string
}

func (c *Command) SetFlags(flagSet *flag.FlagSet) {
	flagSet.StringVar(&c.cipdURL, "cipd_url", "", "show version info for this cipd URL.")
}

func (c *Command) Execute(ctx context.Context, flagSet *flag.FlagSet, _ ...any) subcommands.ExitStatus {
	if flagSet.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "position arguments not expected\n")
		return subcommands.ExitUsageError
	}
	fmt.Println(c.version)
	cipdURL := c.cipdURL
	if cipdURL == "" {
		ver, err := version.Current()
		if err != nil {
			// Note: this is some sort of catastrophic error. If the binary is not
			// installed via CIPD, err == nil && ver.InstanceID == "".
			fmt.Fprintf(os.Stderr, "%s\n", err)
			return 1
		}
		if ver.CIPD == nil && ver.Build != nil {
			fmt.Printf("go\t%s\n", ver.Build.GoVersion)
			fmt.Printf("mod\t%s\t%s\t%s\n", ver.Build.Main.Path, ver.Build.Main.Version, ver.Build.Main.Sum)
			bs := ver.BuildSettings()
			for _, k := range slices.Sorted(maps.Keys(bs)) {
				v := bs[k]
				fmt.Printf("build\t%s=%s\n", k, v)
			}
			return 0
		}
		if ver.CIPD != nil {
			fmt.Println()
			fmt.Printf("CIPD package name: %s\n", ver.CIPD.PackageName)
			fmt.Printf("CIPD instance ID:  %s\n", ver.CIPD.InstanceID)
			cipdURL = fmt.Sprintf("%s/p/%s/+/%s", cipdServiceURL, ver.CIPD.PackageName, ver.CIPD.InstanceID)
		}
	}
	fmt.Printf("CIPD URL: %s\n", cipdURL)

	// already identified the cipd instance id.
	// rest are just informational, so not fail by the following failures.

	// TODO(crbug.com//1451715): cleanup once cipd tag can easily identify the revision of the binary.
	repo, rev, err := parseCIPDGitRepoRevision(ctx, cipdURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to get git_repository and git_revision in %s: %v\n", cipdURL, err)
		return 0
	}
	fmt.Printf("%s/+/%s\n", repo, rev)
	var dir string
	switch repo {
	case "https://chromium.googlesource.com/infra/infra_superproject":
		rev, err = parseInfraSuperprojectDEPS(ctx, rev)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to get infra revision in infra_superproject.git@%s/DEPS: %v\n", rev, err)
			return 0
		}
		repo = "https://chromium.googlesource.com/infra/infra"
		dir = "go/src/infra/build/siso"
		fmt.Printf("https://chromium.googlesource.com/infra/infra/+/%s\n", rev)
	case "https://chromium.googlesource.com/infra/infra":
		dir = "go/src/infra/build/siso"
	case "https://chromium.googlesource.com/build":
		dir = "siso"
	default:
		fmt.Fprintf(os.Stderr, "unknown git_repository: %s\n", repo)
		return 0
	}
	sisoCommit, err := getSisoCommit(ctx, repo, dir, rev)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to get siso commit in infra.git@%s: %v\n", rev, err)
		return 0
	}
	fmt.Println(sisoCommit)
	return 0
}

func parseCIPDGitRepoRevision(ctx context.Context, cipdURL string) (repo, rev string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cipdURL, nil)
	if err != nil {
		return "", "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusBadRequest {
		return "", "", fmt.Errorf("http=%d %s", resp.StatusCode, resp.Status)
	}
	s := bufio.NewScanner(resp.Body)
	var repository, revision string
	for s.Scan() {
		line := s.Bytes()
		if bytes.Contains(line, []byte("git_repository:")) {
			repository = string(bytes.TrimPrefix(bytes.TrimSpace(line), []byte("git_repository:")))
		}
		if bytes.Contains(line, []byte("git_revision:")) {
			revision = string(bytes.TrimPrefix(bytes.TrimSpace(line), []byte("git_revision:")))
		}
	}
	err = s.Err()
	if err != nil {
		return "", "", err
	}
	if repository == "" || revision == "" {
		return "", "", fmt.Errorf("git_repository, git_revision not found in %s", cipdURL)
	}
	return repository, revision, nil
}

func parseInfraSuperprojectDEPS(ctx context.Context, rev string) (string, error) {
	depsURL := fmt.Sprintf("https://chromium.googlesource.com/infra/infra_superproject/+/%s/DEPS?format=TEXT", rev)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, depsURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusBadRequest {
		return "", fmt.Errorf("http=%d %s", resp.StatusCode, resp.Status)
	}
	s := bufio.NewScanner(base64.NewDecoder(base64.StdEncoding, resp.Body))
	var revision string
	for s.Scan() {
		line := s.Bytes()
		if !bytes.Contains(line, []byte("go.chromium.org/infra/infra.git@")) {
			continue
		}
		i := bytes.IndexByte(line, '@')
		if i < 0 {
			continue
		}
		revision = string(line[i+1:])
		i = strings.IndexByte(revision, '"')
		if i >= 0 {
			revision = revision[:i]
		}
	}
	err = s.Err()
	if err != nil {
		return "", err
	}
	if revision == "" {
		return "", fmt.Errorf("go.chromium.org/infra/infra.git not found in %s", depsURL)
	}
	return revision, nil
}

type commit struct {
	revision string
	summary  string
	author   string
	date     time.Time
}

func (c commit) String() string {
	return fmt.Sprintf("%s %s\n %s by %s", c.revision[:10], c.summary, c.date.Format(time.RFC3339), c.author)
}

func getSisoCommit(ctx context.Context, repo, dir, rev string) (commit, error) {
	sisoLogURL := fmt.Sprintf("%s/+log/%s/%s?format=JSON", repo, rev, dir)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sisoLogURL, nil)
	if err != nil {
		return commit{}, nil
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return commit{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusBadRequest {
		return commit{}, fmt.Errorf("http=%d %s", resp.StatusCode, resp.Status)
	}
	buf, err := io.ReadAll(resp.Body)
	if err != nil {
		return commit{}, err
	}
	buf = bytes.TrimPrefix(buf, []byte(")]}'\n"))

	type userInfo struct {
		Name  string `json:"name"`
		Email string `json:"email"`
		Time  string `json:"time"`
	}
	type commitInfo struct {
		Commit  string   `json:"commit"`
		Author  userInfo `json:"author"`
		Message string   `json:"message"`
	}
	type logInfo struct {
		Log []commitInfo `json:"log"`
	}
	var commitLog logInfo
	err = json.Unmarshal(buf, &commitLog)
	if err != nil {
		return commit{}, fmt.Errorf("%w\n%s", err, buf)
	}
	if len(commitLog.Log) == 0 {
		return commit{}, fmt.Errorf("no log\n%#v\n%s", commitLog, buf)
	}
	summary := commitLog.Log[0].Message
	i := strings.IndexByte(summary, '\n')
	if i > 0 {
		summary = summary[:i]
	}
	dateStr := commitLog.Log[0].Author.Time
	// "Tue May 09 04:38:13 2023
	date, err := time.Parse("Mon Jan 02 15:04:06 2006", dateStr)
	if err != nil {
		return commit{}, err
	}
	r := commit{
		revision: commitLog.Log[0].Commit,
		summary:  summary,
		author:   commitLog.Log[0].Author.Email,
		date:     date,
	}
	return r, nil
}
