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

package main

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// job is one suite job of one run attempt: the unit a log belongs to.
type job struct {
	ID       int64     `json:"jobId"`
	Name     string    `json:"jobName"`
	Suite    string    `json:"suite"`
	Leg      string    `json:"leg,omitempty"`
	RunID    int64     `json:"runId"`
	Attempt  int       `json:"attempt"`
	HeadSHA  string    `json:"headSha"`
	Branch   string    `json:"branch"`
	Title    string    `json:"title"`
	PR       int       `json:"pr,omitempty"`
	Started  time.Time `json:"started"`
	URL      string    `json:"url"`
	RunURL   string    `json:"runUrl"`
	Canceled bool      `json:"canceled,omitempty"`
}

var (
	uiJobName = regexp.MustCompile(`^UI suite$`)
	itJobName = regexp.MustCompile(`^Integration suite \((.+)\)$`)
)

type github struct {
	repo, token, cache string
}

func (g *github) get(url string, into any) error {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(into)
}

type apiRun struct {
	ID           int64     `json:"id"`
	Attempt      int       `json:"run_attempt"`
	HeadSHA      string    `json:"head_sha"`
	HeadBranch   string    `json:"head_branch"`
	DisplayTitle string    `json:"display_title"`
	Status       string    `json:"status"`
	HTMLURL      string    `json:"html_url"`
	CreatedAt    time.Time `json:"created_at"`
	HeadRepo     struct {
		Owner struct {
			Login string `json:"login"`
		} `json:"owner"`
	} `json:"head_repository"`
	PullRequests []struct {
		Number int `json:"number"`
	} `json:"pull_requests"`
}

type apiJob struct {
	ID         int64     `json:"id"`
	Name       string    `json:"name"`
	Status     string    `json:"status"`
	Conclusion string    `json:"conclusion"`
	StartedAt  time.Time `json:"started_at"`
	HTMLURL    string    `json:"html_url"`
}

// suiteJobs lists every completed suite job, across all attempts, of the workflow's runs
// created in the window.
func (g *github) suiteJobs(workflow string, from, to time.Time, suites map[string]bool) ([]job, error) {
	var runs []apiRun
	created := from.Format("2006-01-02") + ".." + to.Format("2006-01-02")
	for page := 1; ; page++ {
		var resp struct {
			Runs []apiRun `json:"workflow_runs"`
		}
		url := fmt.Sprintf("https://api.github.com/repos/%s/actions/workflows/%s/runs?created=%s&per_page=100&page=%d",
			g.repo, workflow, created, page)
		if err := g.get(url, &resp); err != nil {
			return nil, err
		}
		runs = append(runs, resp.Runs...)
		if len(resp.Runs) < 100 {
			break
		}
	}

	var jobs []job
	for _, r := range runs {
		if r.Status != "completed" {
			continue
		}
		for attempt := 1; attempt <= r.Attempt; attempt++ {
			apiJobs, err := g.attemptJobs(r.ID, attempt)
			if err != nil {
				return nil, err
			}
			for _, j := range apiJobs {
				suite, leg := "", ""
				switch {
				case uiJobName.MatchString(j.Name):
					suite = "ui"
				case itJobName.MatchString(j.Name):
					suite, leg = "it", itJobName.FindStringSubmatch(j.Name)[1]
				}
				if suite == "" || !suites[suite] || j.Conclusion == "skipped" || j.Status != "completed" {
					continue
				}
				pr := 0
				if len(r.PullRequests) > 0 {
					pr = r.PullRequests[0].Number
				}
				jobs = append(jobs, job{
					ID: j.ID, Name: j.Name, Suite: suite, Leg: leg, RunID: r.ID, Attempt: attempt,
					HeadSHA: r.HeadSHA, Branch: r.HeadRepo.Owner.Login + ":" + r.HeadBranch,
					Title: r.DisplayTitle, PR: pr, Started: j.StartedAt, URL: j.HTMLURL, RunURL: r.HTMLURL,
					Canceled: j.Conclusion == "cancelled",
				})
			}
		}
	}
	sort.Slice(jobs, func(i, k int) bool { return jobs[i].Started.Before(jobs[k].Started) })
	return jobs, nil
}

// attemptJobs lists one attempt's jobs. A completed attempt never changes, so it is cached.
func (g *github) attemptJobs(runID int64, attempt int) ([]apiJob, error) {
	path := filepath.Join(g.cache, "jobs", fmt.Sprintf("%d-%d.json", runID, attempt))
	var resp struct {
		Jobs []apiJob `json:"jobs"`
	}
	if b, err := os.ReadFile(path); err == nil && json.Unmarshal(b, &resp) == nil {
		return resp.Jobs, nil
	}
	url := fmt.Sprintf("https://api.github.com/repos/%s/actions/runs/%d/attempts/%d/jobs?per_page=100", g.repo, runID, attempt)
	if err := g.get(url, &resp); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err == nil {
		if b, err := json.Marshal(resp); err == nil {
			_ = os.WriteFile(path, b, 0o644)
		}
	}
	return resp.Jobs, nil
}

type logResult struct {
	job   job
	execs []execution
	err   error
}

// results fetches and parses every non-cancelled job's log, from the gzip cache when it is
// there, keeping the jobs' order. A log is parsed as soon as it arrives and then dropped:
// holding a month of logs at once takes gigabytes.
func (g *github) results(jobs []job, parallel int) []logResult {
	results := make([]logResult, len(jobs))
	sem := make(chan struct{}, max(parallel, 1))
	var wg sync.WaitGroup
	var done atomic.Int64
	for i, j := range jobs {
		results[i].job = j
		if j.Canceled {
			results[i].err = fmt.Errorf("cancelled")
			continue
		}
		wg.Add(1)
		go func(i int, j job) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			raw, err := g.jobLog(j.ID)
			if err == nil {
				results[i].execs, err = parseLog(j, raw)
			}
			results[i].err = err
			if n := done.Add(1); n%50 == 0 {
				fmt.Fprintf(os.Stderr, "  %d logs read\n", n)
			}
		}(i, j)
	}
	wg.Wait()
	return results
}

func (g *github) jobLog(id int64) ([]byte, error) {
	path := filepath.Join(g.cache, "logs", strconv.FormatInt(id, 10)+".log.gz")
	if f, err := os.Open(path); err == nil {
		defer f.Close()
		zr, err := gzip.NewReader(f)
		if err == nil {
			return io.ReadAll(zr)
		}
	}
	req, err := http.NewRequest(http.MethodGet,
		fmt.Sprintf("https://api.github.com/repos/%s/actions/jobs/%d/logs", g.repo, id), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+g.token)
	// The API redirects to signed blob storage; net/http drops the token on that redirect.
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("log download: %s", resp.Status)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err == nil {
		if f, err := os.Create(path); err == nil {
			zw := gzip.NewWriter(f)
			_, werr := zw.Write(b)
			cerr := zw.Close()
			_ = f.Close()
			if werr != nil || cerr != nil {
				_ = os.Remove(path)
			}
		}
	}
	return b, nil
}
