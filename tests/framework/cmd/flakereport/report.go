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
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"time"
)

type reportMeta struct {
	Repo, Workflow string
	From, To       time.Time
	Jobs, Missing  []job
}

func writeExecutions(path string, execs []execution) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	for _, e := range execs {
		if err := enc.Encode(e); err != nil {
			_ = f.Close()
			return err
		}
	}
	if err := w.Flush(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func writeMarkdown(path string, a *analysis, m reportMeta) error {
	var b strings.Builder
	p := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }

	runs, branches := map[int64]bool{}, map[string]bool{}
	suites := map[string]int{}
	for _, j := range m.Jobs {
		runs[j.RunID] = true
		branches[j.Branch] = true
		suites[j.Suite]++
	}
	scenarioFails, flaky := 0, 0
	for _, g := range a.Groups {
		scenarioFails += g.Fails
		if g.flaky() {
			flaky++
		}
	}

	p("# Flaky test report: %s, %s\n\n", m.Repo, m.Workflow)
	p("- **Window:** %s to %s (UTC)\n", m.From.Format("2006-01-02"), m.To.Format("2006-01-02"))
	p("- **Read:** %d runs on %d branches; %d UI jobs, %d integration jobs", len(runs), len(branches), suites["ui"], suites["it"])
	if len(m.Missing) > 0 {
		p(" (%d jobs had no usable log: cancelled or expired)", len(m.Missing))
	}
	p("\n- **Scenario results:** %d, of which %d failed, in %d distinct scenarios (%d look flaky)\n", a.Executions-len(a.Infra), scenarioFails, len(a.Groups), flaky)
	inIncidents := 0
	for _, in := range a.Incidents {
		inIncidents += in.Scenarios
	}
	p("- **Incidents:** %d times %d or more scenarios of one block failed together in one job, %d failures in all. They are listed on their own and left out of the scenario counts below.\n", len(a.Incidents), incidentSize, inIncidents)
	p("- **Failures above any scenario** (boot, build, empty selection): %d\n\n", len(a.Infra))

	const top = 50
	p("## Scenarios that failed, most failures first (top %d of %d)\n\n", min(top, len(a.Groups)), len(a.Groups))
	p("An integration scenario runs once per database and gateway combination (its variants); counts add them up.\n\n")
	p("| # | Suite | Scenario | Runner | Failed / runs | Also in incidents | Variants failing | Branches | Verdict | Main reason | Last failure |\n|---|---|---|---|---|---|---|---|---|---|---|\n")
	for i, g := range a.Groups {
		if i == top {
			break
		}
		p("| %d | %s | %s | %s | %d / %d (%.1f%%) | %d | %d of %d | %d | %s | %s | [%s](%s) |\n", i+1, g.Suite, cell(display(g.Scenario)),
			cell(g.Runner), g.Fails, g.Runs, 100*g.rate(), g.IncidentFails, g.FailingVariants, g.Variants, len(g.FailBranches), cell(verdictText(g)),
			cell(topSignature(g.Signatures)), g.LastFail.Format("Jan 2"), g.LastFailURL)
	}
	p("\n### By verdict\n\n| Verdict | Scenarios | Failures |\n|---|---|---|\n")
	for _, v := range verdictOrder {
		n, f := 0, 0
		for _, g := range a.Groups {
			if slices.Contains(g.Verdicts, v) {
				n++
				f += g.Fails
			}
		}
		p("| %s | %d | %d |\n", v, n, f)
	}

	p("\n## Incidents: many scenarios of one block failing together\n\n")
	if len(a.Incidents) == 0 {
		p("None.\n")
	} else {
		p("| When | Suite | Block | Scenarios failed | Branch | First failure's reason | Job |\n|---|---|---|---|---|---|---|\n")
		for i, in := range a.Incidents {
			if i == top {
				p("\n%d more in executions.jsonl (`incident` is set on their failures).\n", len(a.Incidents)-top)
				break
			}
			block := in.Block
			if in.Leg != "" {
				block = in.Leg + " · " + block
			}
			p("| %s | %s | %s | %d | %s | %s | [job](%s) |\n", in.Started.Format("Jan 2 15:04"), in.Suite, cell(block), in.Scenarios,
				cell(in.Branch), cell(in.Signature), in.URL)
		}
	}

	p("\n## Why scenarios failed\n\n| Category | Failures | Scenarios | Example |\n|---|---|---|---|\n")
	for _, c := range a.Categories {
		p("| %s | %d | %d | [job](%s) |\n", c.Name, c.Count, len(c.Scenarios), c.Example)
	}
	p("\n### Most common failure signatures\n\n| Signature | Failures | Scenarios | Example |\n|---|---|---|---|\n")
	for i, c := range a.Signatures {
		if i == 25 {
			break
		}
		p("| %s | %d | %d | [job](%s) |\n", cell(c.Name), c.Count, len(c.Scenarios), c.Example)
	}

	p("\n## Failures above any scenario\n\n")
	if len(a.Infra) == 0 {
		p("None.\n")
	} else {
		type infraRow struct {
			where, category, message, lastURL string
			count                             int
			last                              time.Time
		}
		rows := map[string]*infraRow{}
		var order []*infraRow
		for _, e := range a.Infra {
			where := e.Suite + " · " + e.Block
			if e.Block == "" {
				where = e.Suite + " · (whole suite)"
			}
			if e.Leg != "" {
				where = e.Suite + " · " + e.Leg + " · " + e.Block
			}
			k := where + "|" + e.Category
			r := rows[k]
			if r == nil {
				r = &infraRow{where: where, category: e.Category, message: normalize(e.Message)}
				rows[k] = r
				order = append(order, r)
			}
			r.count++
			r.last, r.lastURL = e.Started, e.URL
		}
		sort.SliceStable(order, func(i, j int) bool { return order[i].count > order[j].count })
		p("| Where | Category | Times | Last | Message |\n|---|---|---|---|---|\n")
		for _, r := range order {
			p("| %s | %s | %d | [%s](%s) | %s |\n", cell(r.where), r.category, r.count, r.last.Format("Jan 2"), r.lastURL, cell(r.message))
		}
	}

	p("\n## By week\n\n| Week of | Jobs | Scenario results | Failed | Failure rate | Failures above scenarios |\n|---|---|---|---|---|---|\n")
	for _, w := range a.Weeks {
		rate := 0.0
		if w.Runs > 0 {
			rate = 100 * float64(w.Fails) / float64(w.Runs)
		}
		p("| %s | %d | %d | %d | %.2f%% | %d |\n", w.Start.Format("2006-01-02"), w.Jobs, w.Runs, w.Fails, rate, w.InfraFailures)
	}

	p("\n## Slowest scenarios (95th percentile)\n\n| Suite | Scenario | Block / runner | Runs | Median | p95 |\n|---|---|---|---|---|---|\n")
	for _, s := range slowest(a, 15) {
		p("| %s | %s | %s | %d | %.0fs | %.0fs |\n", s.Suite, cell(display(s.Scenario)), cell(s.Block+" / "+s.Runner), len(s.Durations),
			percentile(s.Durations, 50), percentile(s.Durations, 95))
	}

	p("\n## How to read this\n\n")
	p("- **%s:** the same commit both passed and failed (a rerun, or another leg of one run), so the failure can't be the code.\n", verdictProvenFlaky)
	p("- **%s:** three or more runs in a row failed, on two or more branches: something on main was broken for everyone.\n", verdictBrokenWindow)
	p("- **%s:** every failure was on one branch, so that branch's change is the first suspect.\n", verdictBranchOnly)
	p("- **%s:** failed on several branches with passes in between.\n", verdictLikelyFlaky)
	p("- **%s:** failed once in the window.\n", verdictOneOff)
	p("- **Incidents** are failures where %d or more scenarios of one block failed in the same job. They point at one cause, so they don't count towards any scenario's verdict or failure rate.\n", incidentSize)
	p("- Categories come from rules on the failure text; `other` means no rule matched yet. The full per-run data is in `executions.jsonl`, and `allure-report` shows every run of a scenario on its page.\n")
	p("- Only the Platform Tests jobs are read: they run the suites with `-v`, which prints every scenario's result. Scenarios tagged `@known-issue` are excluded from runs, so they never appear.\n")

	return os.WriteFile(path, []byte(b.String()), 0o644)
}

func verdictText(s *scenarioStats) string {
	v := strings.Join(s.Verdicts, ", ")
	if s.Detail != "" {
		v += " (" + s.Detail + ")"
	}
	return v
}

func topSignature(sigs map[string]int) string {
	best, n := "", 0
	for s, c := range sigs {
		if c > n || (c == n && s < best) {
			best, n = s, c
		}
	}
	if len(sigs) > 1 {
		best += fmt.Sprintf(" (+%d other)", len(sigs)-1)
	}
	return best
}

func slowest(a *analysis, n int) []*scenarioStats {
	// Every scenario, not only failing ones: slow passing scenarios are the next timeouts.
	var out []*scenarioStats
	for _, s := range a.All {
		if len(s.Durations) >= 3 {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return percentile(out[i].Durations, 95) > percentile(out[j].Durations, 95) })
	if len(out) > n {
		out = out[:n]
	}
	return out
}

func percentile(xs []float64, p float64) float64 {
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	i := int(float64(len(s)-1) * p / 100)
	return s[i]
}

// display turns a go test subtest name back into the scenario's title.
func display(name string) string { return strings.ReplaceAll(name, "_", " ") }

func cell(s string) string {
	return strings.NewReplacer("|", `\|`, "\n", " ").Replace(s)
}
