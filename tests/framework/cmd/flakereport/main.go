/*
 * Copyright (c) 2026, WSO2 LLC. (https://www.wso2.com).
 *
 * WSO2 LLC. licenses this file to you under the Apache License,
 * Version 2.0 (the "License"); you may not use this file except
 * in compliance with the License.
 * You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied.  See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

// Command flakereport reads the UI and integration suite job logs of the Platform Tests
// workflow over a time window and reports which scenarios fail, why, and whether they are
// flaky or genuinely broken. It writes report.md, executions.jsonl and an Allure 3 report.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func main() {
	since := flag.String("since", "30d", "window start: a duration in days (30d) or a date (2026-10-01)")
	until := flag.String("until", "", "window end date (inclusive); empty means now")
	suites := flag.String("suites", "ui,it", "suites to read: ui, it")
	repo := flag.String("repo", "wso2/api-platform", "GitHub repository")
	workflow := flag.String("workflow", "platform-tests.yml", "workflow file whose jobs are read")
	out := flag.String("out", "flakereport-out", "output directory")
	cache := flag.String("cache", "", "log cache directory (default <out>/cache)")
	allure := flag.Bool("allure", true, "generate the Allure 3 report with npx")
	parallel := flag.Int("parallel", 4, "concurrent log downloads")
	flag.Parse()

	if err := run(*since, *until, *suites, *repo, *workflow, *out, *cache, *allure, *parallel); err != nil {
		fmt.Fprintln(os.Stderr, "flakereport:", err)
		os.Exit(1)
	}
}

func run(since, until, suiteList, repo, workflow, out, cache string, allure bool, parallel int) error {
	from, to, err := window(since, until, time.Now().UTC())
	if err != nil {
		return err
	}
	wanted := map[string]bool{}
	for _, s := range strings.Split(suiteList, ",") {
		s = strings.TrimSpace(s)
		if s != "ui" && s != "it" {
			return fmt.Errorf("unknown suite %q (want ui or it)", s)
		}
		wanted[s] = true
	}
	if cache == "" {
		cache = filepath.Join(out, "cache")
	}
	if err := os.MkdirAll(cache, 0o755); err != nil {
		return err
	}
	token, err := githubToken()
	if err != nil {
		return err
	}

	gh := &github{repo: repo, token: token, cache: cache}
	jobs, err := gh.suiteJobs(workflow, from, to, wanted)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "reading %d suite jobs from %s to %s\n", len(jobs), from.Format("2006-01-02"), to.Format("2006-01-02"))

	var execs []execution
	var missing []job
	for _, r := range gh.results(jobs, parallel) {
		if r.err != nil {
			fmt.Fprintf(os.Stderr, "  job %d: %v\n", r.job.ID, r.err)
			missing = append(missing, r.job)
			continue
		}
		execs = append(execs, r.execs...)
	}

	a := analyze(execs)
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	// Only what failed somewhere: every run of a scenario that failed at least once, and the
	// failures above scenarios. All-green scenarios stay counted in the report only.
	failing := map[string]bool{}
	for _, s := range a.Scenarios {
		failing[s.Key] = true
	}
	var kept []execution
	for _, e := range execs {
		if e.Kind == "infra" || e.Failed || failing[e.key()] {
			kept = append(kept, e)
		}
	}
	if err := writeExecutions(filepath.Join(out, "executions.jsonl"), kept); err != nil {
		return err
	}
	if err := writeMarkdown(filepath.Join(out, "report.md"), a, reportMeta{
		Repo: repo, Workflow: workflow, From: from, To: to, Jobs: jobs, Missing: missing,
	}); err != nil {
		return err
	}
	// Allure gets the failures only: a scenario's thousands of green runs would drown the UI,
	// and the counts are in the report.
	var failed []execution
	for _, e := range kept {
		if e.Failed {
			failed = append(failed, e)
		}
	}
	if err := writeAllure(out, failed, a); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "wrote %s and %s\n", filepath.Join(out, "report.md"), filepath.Join(out, "executions.jsonl"))
	if allure {
		cmd := exec.Command("npx", "-y", "allure@"+allureVersion, "generate", "allure-results", "-o", "allure-report")
		cmd.Dir, cmd.Stdout, cmd.Stderr = out, os.Stderr, os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("generating the Allure report: %w", err)
		}
		fmt.Fprintf(os.Stderr, "open it with: (cd %s && npx allure@%s open allure-report)\n", out, allureVersion)
	}
	return nil
}

// window resolves -since and -until into a [from, to] range of whole UTC days.
func window(since, until string, now time.Time) (time.Time, time.Time, error) {
	to := now
	if until != "" {
		t, err := time.Parse("2006-01-02", until)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("-until: %w", err)
		}
		to = t.Add(24*time.Hour - time.Second)
	}
	var from time.Time
	if days, ok := strings.CutSuffix(since, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n <= 0 {
			return time.Time{}, time.Time{}, fmt.Errorf("-since %q: want a positive number of days such as 30d", since)
		}
		from = to.AddDate(0, 0, -n)
	} else {
		t, err := time.Parse("2006-01-02", since)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("-since: %w", err)
		}
		from = t
	}
	if !from.Before(to) {
		return time.Time{}, time.Time{}, fmt.Errorf("-since must be before -until")
	}
	return from, to, nil
}

func githubToken() (string, error) {
	if t := os.Getenv("GITHUB_TOKEN"); t != "" {
		return t, nil
	}
	b, err := exec.Command("gh", "auth", "token").Output()
	if err != nil {
		return "", fmt.Errorf("no GITHUB_TOKEN and `gh auth token` failed: %w", err)
	}
	return strings.TrimSpace(string(b)), nil
}
