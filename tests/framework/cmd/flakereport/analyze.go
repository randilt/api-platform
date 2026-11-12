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
	"fmt"
	"regexp"
	"slices"
	"sort"
	"time"
)

// Verdicts, most certain first.
const (
	verdictProvenFlaky  = "proven flaky"
	verdictBrokenWindow = "broken for a period"
	verdictBranchOnly   = "fails on one branch only"
	verdictLikelyFlaky  = "likely flaky"
	verdictOneOff       = "one-off"
)

type scenarioStats struct {
	Key, Suite, Leg, Block, Runner, Scenario string
	Runs, Fails                              int
	IncidentFails                            int // failures inside an incident, not counted in Fails
	FailBranches                             map[string]bool
	FirstFail, LastFail                      time.Time
	LastFailURL                              string
	Signatures                               map[string]int
	Durations                                []float64
	Verdicts                                 []string
	Detail                                   string
	// For a group: how many block/leg variants ran it, and how many of those failed.
	Variants, FailingVariants int
}

func (s *scenarioStats) rate() float64 { return float64(s.Fails) / float64(s.Runs) }

func (s *scenarioStats) flaky() bool {
	for _, v := range s.Verdicts {
		if v == verdictProvenFlaky || v == verdictLikelyFlaky {
			return true
		}
	}
	return false
}

type count struct {
	Name      string
	Count     int
	Scenarios map[string]bool
	Example   string
}

type week struct {
	Start                            time.Time
	Jobs, Runs, Fails, InfraFailures int
}

// incident is one job's block where many scenarios failed together: one cause (a control
// plane that stopped answering, a login that broke) rather than many flaky scenarios.
type incident struct {
	job
	Block     string
	Scenarios int
	Signature string
}

// incidentSize is how many scenarios of one block must fail in one job to call it an incident.
const incidentSize = 5

type analysis struct {
	Incidents  []*incident
	All        []*scenarioStats // every scenario variant seen
	Scenarios  []*scenarioStats // the variants that failed at least once
	Groups     []*scenarioStats // Scenarios merged across blocks and legs, most failures first
	Infra      []execution
	Categories []*count
	Signatures []*count
	Weeks      []*week
	Executions int
}

// analyze aggregates executions per scenario and decides each failing scenario's verdicts.
// Executions arrive in job start order.
func analyze(execs []execution) *analysis {
	a := &analysis{Executions: len(execs)}
	byKey := map[string]*scenarioStats{}
	var keys []string
	history := map[string][]point{}

	blockFails := map[string]int{}
	for _, e := range execs {
		if e.Kind == "scenario" && e.Failed {
			blockFails[fmt.Sprintf("%d|%s", e.ID, e.Block)]++
		}
	}
	incidents := map[string]*incident{}
	pendingIncident := map[string]int{} // incident failures seen before the scenario's first counted run
	cats, sigs := map[string]*count{}, map[string]*count{}
	weeks := map[time.Time]*week{}
	weekJobs := map[time.Time]map[int64]bool{}

	tally := func(m map[string]*count, name, scenario, url string) {
		c := m[name]
		if c == nil {
			c = &count{Name: name, Scenarios: map[string]bool{}, Example: url}
			m[name] = c
		}
		c.Count++
		c.Scenarios[scenario] = true
	}

	for i, e := range execs {
		w := weekOf(e.Started)
		if weeks[w] == nil {
			weeks[w], weekJobs[w] = &week{Start: w}, map[int64]bool{}
		}
		weekJobs[w][e.ID] = true

		if e.Kind == "infra" {
			a.Infra = append(a.Infra, e)
			weeks[w].InfraFailures++
			continue
		}
		if e.Failed {
			if ik := fmt.Sprintf("%d|%s", e.ID, e.Block); blockFails[ik] >= incidentSize {
				in := incidents[ik]
				if in == nil {
					in = &incident{job: e.job, Block: e.Block, Scenarios: blockFails[ik], Signature: e.Signature}
					incidents[ik] = in
					a.Incidents = append(a.Incidents, in)
				}
				execs[i].Incident = true
				if s := byKey[e.key()]; s != nil {
					s.IncidentFails++
				} else {
					pendingIncident[e.key()]++
				}
				continue
			}
		}
		weeks[w].Runs++
		k := e.key()
		s := byKey[k]
		if s == nil {
			s = &scenarioStats{Key: k, Suite: e.Suite, Leg: e.Leg, Block: e.Block, Runner: e.Runner,
				Scenario: e.Scenario, FailBranches: map[string]bool{}, Signatures: map[string]int{}}
			byKey[k] = s
			keys = append(keys, k)
			s.IncidentFails, pendingIncident[k] = pendingIncident[k], 0
		}
		s.Runs++
		s.Durations = append(s.Durations, e.Elapsed)
		history[k] = append(history[k], point{e.HeadSHA, e.Branch, e.Started, e.Failed})
		if !e.Failed {
			continue
		}
		weeks[w].Fails++
		s.Fails++
		s.FailBranches[e.Branch] = true
		if s.FirstFail.IsZero() {
			s.FirstFail = e.Started
		}
		s.LastFail, s.LastFailURL = e.Started, e.URL
		s.Signatures[e.Signature]++
		tally(cats, e.Category, k, e.URL)
		tally(sigs, e.Signature, k, e.URL)
	}

	for _, k := range keys {
		s := byKey[k]
		a.All = append(a.All, s)
		if s.Fails == 0 {
			continue
		}
		s.Verdicts, s.Detail = verdicts(history[k])
		a.Scenarios = append(a.Scenarios, s)
	}
	a.Groups = group(a.All)
	sort.SliceStable(a.Incidents, func(i, j int) bool { return a.Incidents[i].Scenarios > a.Incidents[j].Scenarios })
	a.Categories, a.Signatures = sortedCounts(cats), sortedCounts(sigs)
	for w, wk := range weeks {
		wk.Jobs = len(weekJobs[w])
		a.Weeks = append(a.Weeks, wk)
	}
	sort.Slice(a.Weeks, func(i, j int) bool { return a.Weeks[i].Start.Before(a.Weeks[j].Start) })
	return a
}

// group merges a scenario's variants (an integration scenario runs once per database and
// gateway combination) into one row. A group's verdict is its most certain variant's: a
// "same commit passed and failed" only counts within one variant, never across two.
func group(all []*scenarioStats) []*scenarioStats {
	byName := map[string]*scenarioStats{}
	var out []*scenarioStats
	rank := func(s *scenarioStats) int {
		for i, v := range verdictOrder {
			if slices.Contains(s.Verdicts, v) {
				return i
			}
		}
		return len(verdictOrder)
	}
	for _, s := range all {
		// A Scenario Outline's examples are separate subtests (#01, #02…) of one scenario.
		name := outlineExample.ReplaceAllString(s.Scenario, "")
		k := s.Suite + "|" + s.Runner + "|" + name
		g := byName[k]
		if g == nil {
			g = &scenarioStats{Key: k, Suite: s.Suite, Runner: s.Runner, Scenario: name,
				FailBranches: map[string]bool{}, Signatures: map[string]int{}}
			byName[k] = g
			out = append(out, g)
		}
		g.Variants++
		g.Runs += s.Runs
		g.IncidentFails += s.IncidentFails
		g.Durations = append(g.Durations, s.Durations...)
		if s.Fails == 0 {
			continue
		}
		g.FailingVariants++
		g.Fails += s.Fails
		if g.Block == "" {
			g.Block = s.Block
		}
		for b := range s.FailBranches {
			g.FailBranches[b] = true
		}
		for sig, n := range s.Signatures {
			g.Signatures[sig] += n
		}
		if g.FirstFail.IsZero() || s.FirstFail.Before(g.FirstFail) {
			g.FirstFail = s.FirstFail
		}
		if s.LastFail.After(g.LastFail) {
			g.LastFail, g.LastFailURL = s.LastFail, s.LastFailURL
		}
		if g.Verdicts == nil || rank(s) < rank(g) {
			g.Verdicts, g.Detail = s.Verdicts, s.Detail
		}
	}
	failing := out[:0]
	for _, g := range out {
		if g.Fails == 0 {
			continue
		}
		// Each variant failing once is a one-off per variant; several failing on one branch
		// point at that branch.
		if slices.Equal(g.Verdicts, []string{verdictOneOff}) && g.Fails > 1 && len(g.FailBranches) == 1 {
			g.Verdicts = []string{verdictBranchOnly}
			for b := range g.FailBranches {
				g.Detail = "only on " + b
			}
		}
		failing = append(failing, g)
	}
	sort.SliceStable(failing, func(i, j int) bool {
		x, y := failing[i], failing[j]
		if x.Fails != y.Fails {
			return x.Fails > y.Fails
		}
		return x.rate() > y.rate()
	})
	return failing
}

var verdictOrder = []string{verdictProvenFlaky, verdictBrokenWindow, verdictBranchOnly, verdictLikelyFlaky, verdictOneOff}

var outlineExample = regexp.MustCompile(`#\d+$`)

// point is one counted run of a scenario, all that its verdict needs.
type point struct {
	HeadSHA, Branch string
	Started         time.Time
	Failed          bool
}

// verdicts reads one scenario's chronological results.
func verdicts(h []point) ([]string, string) {
	var out []string
	var detail string

	bySHA := map[string][2]bool{} // passed, failed
	for _, e := range h {
		v := bySHA[e.HeadSHA]
		if e.Failed {
			v[1] = true
		} else {
			v[0] = true
		}
		bySHA[e.HeadSHA] = v
	}
	for sha, v := range bySHA {
		if v[0] && v[1] {
			out = append(out, verdictProvenFlaky)
			detail = fmt.Sprintf("commit %.9s both passed and failed", sha)
			break
		}
	}

	// The longest stretch of consecutive failures; across several branches it means the
	// scenario was broken for everyone for a while.
	bestLen, bestFrom, curLen, curFrom := 0, 0, 0, 0
	for i, e := range h {
		if !e.Failed {
			curLen = 0
			continue
		}
		if curLen == 0 {
			curFrom = i
		}
		curLen++
		if curLen > bestLen {
			bestLen, bestFrom = curLen, curFrom
		}
	}
	streakBranches := map[string]bool{}
	for _, e := range h[bestFrom : bestFrom+bestLen] {
		streakBranches[e.Branch] = true
	}
	if bestLen >= 3 && len(streakBranches) >= 2 {
		out = append(out, verdictBrokenWindow)
		if detail == "" {
			detail = fmt.Sprintf("failed every run %s to %s on %d branches",
				h[bestFrom].Started.Format("Jan 2"), h[bestFrom+bestLen-1].Started.Format("Jan 2"), len(streakBranches))
		}
	}

	failBranches := map[string]bool{}
	fails := 0
	for _, e := range h {
		if e.Failed {
			failBranches[e.Branch] = true
			fails++
		}
	}
	switch {
	case fails == 1 && len(out) == 0:
		out = append(out, verdictOneOff)
	case len(failBranches) == 1 && len(out) == 0:
		out = append(out, verdictBranchOnly)
		for b := range failBranches {
			detail = "only on " + b
		}
	case len(failBranches) >= 2 && !slices.Contains(out, verdictBrokenWindow) && !slices.Contains(out, verdictProvenFlaky):
		out = append(out, verdictLikelyFlaky)
		detail = fmt.Sprintf("failed on %d branches, passing in between", len(failBranches))
	}
	return out, detail
}

func sortedCounts(m map[string]*count) []*count {
	out := make([]*count, 0, len(m))
	for _, c := range m {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// weekOf is the Monday 00:00 UTC that starts t's week.
func weekOf(t time.Time) time.Time {
	t = t.UTC()
	d := (int(t.Weekday()) + 6) % 7
	return time.Date(t.Year(), t.Month(), t.Day()-d, 0, 0, 0, 0, time.UTC)
}
