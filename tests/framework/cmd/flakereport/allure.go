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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// allureVersion pins the Allure 3 CLI that npx runs.
const allureVersion = "3.33.0"

type allureLabel struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type allureLink struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	Type string `json:"type"`
}

type allureResult struct {
	UUID          string        `json:"uuid"`
	HistoryID     string        `json:"historyId"`
	TestCaseID    string        `json:"testCaseId"`
	FullName      string        `json:"fullName"`
	Name          string        `json:"name"`
	Status        string        `json:"status"`
	Stage         string        `json:"stage"`
	Start         int64         `json:"start"`
	Stop          int64         `json:"stop"`
	StatusDetails *allureStatus `json:"statusDetails,omitempty"`
	Labels        []allureLabel `json:"labels"`
	Links         []allureLink  `json:"links"`
}

type allureStatus struct {
	Message string `json:"message,omitempty"`
	Trace   string `json:"trace,omitempty"`
	Flaky   bool   `json:"flaky,omitempty"`
}

// writeAllure writes one Allure result per execution, all executions of a scenario sharing
// one identity: Allure 3 then shows the scenario once, at its latest run, with every other
// run listed under it with its status and error. Our flaky verdict rides along as the
// result's flaky flag, which Allure keeps when it has no history of its own.
func writeAllure(out string, execs []execution, a *analysis) error {
	dir := filepath.Join(out, "allure-results")
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	flaky := map[string]bool{}
	for _, s := range a.Scenarios {
		flaky[s.Key] = s.flaky()
	}
	for i, e := range execs {
		if e.Incident {
			continue // summarised below, one result per incident
		}
		key, name, suite, sub := e.key(), display(e.Scenario), e.Block, e.Runner
		if e.Kind == "infra" {
			key = strings.Join([]string{e.Suite, e.Leg, e.Block, "infra", e.Category}, "|")
			name, suite, sub = "Failed before any scenario: "+e.Category, "(infrastructure)", e.Block
		}
		parent := e.Suite
		if e.Leg != "" {
			parent += " · " + e.Leg
		}
		start := e.Started.UnixMilli() + int64(i) // keeps two executions of one job ordered
		r := allureResult{
			UUID: hash(fmt.Sprintf("%d|%s|%d", e.ID, key, i)), HistoryID: hash(key), TestCaseID: hash(key),
			FullName: key, Name: name, Status: "passed", Stage: "finished",
			Start: start, Stop: start + int64(e.Elapsed*1000),
			Labels: []allureLabel{
				{"parentSuite", parent}, {"suite", suite}, {"subSuite", sub}, {"tag", e.Branch},
			},
			Links: []allureLink{{"job: " + e.Branch, e.URL, "link"}, {"run and artifacts", e.RunURL, "link"}},
		}
		if e.Failed {
			r.Status = "failed"
			top, _, _ := strings.Cut(e.Category, "/")
			r.Labels = append(r.Labels, allureLabel{"failureCategory", top})
			r.StatusDetails = &allureStatus{Message: clip("["+e.Category+"] "+e.Message, 400), Trace: trace(e), Flaky: flaky[e.key()]}
		} else if flaky[e.key()] {
			r.StatusDetails = &allureStatus{Flaky: true}
		}
		b, err := json.Marshal(r)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, r.UUID+"-result.json"), b, 0o644); err != nil {
			return err
		}
	}
	// An incident's victims would each carry the others as retries, which is most of the
	// report's size and none of its value: one result says what happened.
	for _, in := range a.Incidents {
		parent := in.Suite
		if in.Leg != "" {
			parent += " · " + in.Leg
		}
		category, _, _ := strings.Cut(in.Signature, ":")
		top, _, _ := strings.Cut(category, "/")
		key := fmt.Sprintf("incident|%d|%s", in.ID, in.Block)
		r := allureResult{
			UUID: hash(key), HistoryID: hash(key), TestCaseID: hash(key), FullName: key,
			Name:   fmt.Sprintf("%d scenarios failed together (%s)", in.Scenarios, in.Started.Format("Jan 2 15:04")),
			Status: "failed", Stage: "finished", Start: in.Started.UnixMilli(), Stop: in.Started.UnixMilli(),
			StatusDetails: &allureStatus{Message: clip(in.Signature, 400)},
			Labels: []allureLabel{
				{"parentSuite", "(incidents)"}, {"suite", parent}, {"subSuite", in.Block}, {"tag", in.Branch},
				{"failureCategory", top},
			},
			Links: []allureLink{{"job: " + in.Branch, in.URL, "link"}, {"run and artifacts", in.RunURL, "link"}},
		}
		b, err := json.Marshal(r)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, r.UUID+"-result.json"), b, 0o644); err != nil {
			return err
		}
	}
	return writeAllureConfig(out)
}

// writeAllureConfig maps our failure categories to Allure categories.
func writeAllureConfig(out string) error {
	seen := map[string]bool{}
	var cats []map[string]any
	for _, r := range rules {
		if !seen[r.category] {
			seen[r.category] = true
			cats = append(cats, map[string]any{
				"name": r.category, "matchers": map[string]any{"labels": map[string]string{"failureCategory": r.category}},
			})
		}
	}
	cats = append(cats, map[string]any{
		"name": "other", "matchers": map[string]any{"labels": map[string]string{"failureCategory": "other"}},
	})
	b, err := json.MarshalIndent(map[string]any{
		"name": "Flaky test report", "output": "allure-report", "categories": cats,
		// One self-contained index.html: opens from disk, so it can be shared as a file.
		"plugins": map[string]any{"awesome": map[string]any{"options": map[string]any{
			"singleFile": true, "reportName": "Flaky test report",
		}}},
	}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(out, "allurerc.json"), b, 0o644)
}

func trace(e execution) string {
	var b strings.Builder
	if e.Step != "" {
		b.WriteString("Failed step: " + e.Step + "\n")
	}
	if e.Locator != "" {
		b.WriteString("Waiting for: " + e.Locator + "\n")
	}
	b.WriteString("\n")
	// Allure copies a failure's text into every related result, so it stays short: the error's
	// first lines, without the page snapshot Playwright prints after "Actual value:".
	out, _, cut := strings.Cut(e.output, "Actual value:")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) > 15 {
		lines, cut = lines[:15], true
	}
	b.WriteString(strings.Join(lines, "\n"))
	if cut {
		b.WriteString("\n… the rest is in the job log")
	}
	return b.String()
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func hash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:16])
}
