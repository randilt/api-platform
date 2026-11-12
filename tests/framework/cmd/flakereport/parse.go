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
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// execution is one scenario's result in one job, or a failure above any scenario (a block
// that never booted, a suite whose images never built).
type execution struct {
	job
	Kind      string  `json:"kind"` // scenario | infra
	Block     string  `json:"block"`
	Runner    string  `json:"runner,omitempty"`
	Scenario  string  `json:"scenario,omitempty"`
	Failed    bool    `json:"failed"`
	Elapsed   float64 `json:"elapsed"`
	Step      string  `json:"step,omitempty"`
	Message   string  `json:"message,omitempty"`
	Locator   string  `json:"locator,omitempty"`
	Category  string  `json:"category,omitempty"`
	Signature string  `json:"signature,omitempty"`
	Incident  bool    `json:"incident,omitempty"`
	output    string
}

// key identifies a scenario across jobs. An integration leg is a different gateway release,
// so it is part of the identity.
func (e execution) key() string {
	return strings.Join([]string{e.Suite, e.Leg, e.Block, e.Runner, e.Scenario}, "|")
}

type testEvent struct {
	Action string
	Test   string
	Output string
}

var (
	timestampPrefix = regexp.MustCompile(`(?m)^\d{4}-\d\d-\d\dT[\d:.]+Z ?`)
	runnerHeader    = regexp.MustCompile(`^=== (\S+) ===$`)
	failedScenario  = regexp.MustCompile(`^\s+Scenario(?: Outline)?: (.+?)\s+# `)
	failedStep      = regexp.MustCompile(`^\s+((?:Given|When|Then|And|But|\*) .+?)\s+# `)
	failedError     = regexp.MustCompile(`^\s+Error: (.*)`)
	// test2json drops the duration of an indented subtest result line, so it is read here.
	resultLine = regexp.MustCompile(`(?m)^\s*--- (?:PASS|FAIL|SKIP): (\S+) \(([\d.]+)s\)`)
)

// parseLog turns one job's log into executions. go tool test2json does the go test -v
// bookkeeping (parallel subtests interleave); godog's "--- Failed steps:" summary is read
// from the raw text, because it is printed after the runner and test2json files it under
// whichever test happens to be current.
func parseLog(j job, raw []byte) ([]execution, error) {
	text := timestampPrefix.ReplaceAll(raw, nil)

	bin, err := test2json()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(bin, "-p", j.Suite)
	cmd.Stdin = bytes.NewReader(text)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("test2json: %w", err)
	}

	type result struct {
		action string
		output strings.Builder
	}
	tests := map[string]*result{}
	var order []string
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		var ev testEvent
		if json.Unmarshal(sc.Bytes(), &ev) != nil || ev.Test == "" {
			continue
		}
		r := tests[ev.Test]
		if r == nil {
			r = &result{}
			tests[ev.Test] = r
			order = append(order, ev.Test)
		}
		switch ev.Action {
		case "output":
			r.output.WriteString(ev.Output)
		case "pass", "fail", "skip":
			r.action = ev.Action
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}

	// A test failed "at its origin" when none of its subtests failed: that is where the
	// failure is reported, once.
	// A subtest name can itself contain "/" (an integration block such as gateway-core/sqlite),
	// so every prefix is marked, not only the one before the last slash.
	hasChild, childFailed := map[string]bool{}, map[string]bool{}
	for _, name := range order {
		for i := strings.IndexByte(name, '/'); i > 0; i = nextSlash(name, i) {
			hasChild[name[:i]] = true
			if tests[name].action == "fail" {
				childFailed[name[:i]] = true
			}
		}
	}

	elapsed := map[string]float64{}
	for _, m := range resultLine.FindAllSubmatch(text, -1) {
		elapsed[string(m[1])], _ = strconv.ParseFloat(string(m[2]), 64)
	}
	steps := failedSteps(string(text))
	var execs []execution
	for _, name := range order {
		r := tests[name]
		if r.action != "pass" && r.action != "fail" {
			continue
		}
		segs := strings.Split(name, "/")
		leaf := !hasChild[name]
		switch {
		case leaf && len(segs) >= 4:
			// TestXSuite/<block…>/<runner>/<scenario>; integration blocks may be two
			// segments (gateway-core/postgres).
			e := execution{job: j, Kind: "scenario", Block: strings.Join(segs[1:len(segs)-2], "/"),
				Runner: segs[len(segs)-2], Scenario: segs[len(segs)-1], Failed: r.action == "fail", Elapsed: elapsed[name]}
			if e.Failed {
				e.output = r.output.String()
				if s, ok := steps[strings.Join(segs[1:], "/")]; ok {
					e.Step, e.Message = s.step, s.err
				}
			}
			execs = append(execs, e)
		case r.action == "fail" && !childFailed[name]:
			execs = append(execs, execution{job: j, Kind: "infra", Block: strings.Join(segs[1:], "/"),
				Failed: true, Elapsed: elapsed[name], output: r.output.String()})
		}
	}
	for i := range execs {
		classify(&execs[i])
	}
	return execs, nil
}

// test2json is the toolchain's test2json binary, run directly: going through `go tool` for
// each of hundreds of logs costs more than the conversion.
var test2json = sync.OnceValues(func() (string, error) {
	out, err := exec.Command("go", "tool", "-n", "test2json").Output()
	if err != nil {
		return "", fmt.Errorf("locating test2json: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
})

func nextSlash(s string, after int) int {
	if j := strings.IndexByte(s[after+1:], '/'); j >= 0 {
		return after + 1 + j
	}
	return -1
}

type stepFailure struct{ step, err string }

// failedSteps maps "<block>/<runner>/<Scenario_name>" to the step godog reports as failed.
func failedSteps(text string) map[string]stepFailure {
	found := map[string]stepFailure{}
	var runner, scenario string
	inFailed := false
	for _, line := range strings.Split(text, "\n") {
		if m := runnerHeader.FindStringSubmatch(line); m != nil {
			runner, inFailed = m[1], false
			continue
		}
		if strings.HasPrefix(line, "--- Failed steps:") {
			inFailed = true
			continue
		}
		if !inFailed || runner == "" {
			continue
		}
		if m := failedScenario.FindStringSubmatch(line); m != nil {
			scenario = runner + "/" + strings.ReplaceAll(m[1], " ", "_")
			continue
		}
		if scenario == "" {
			continue
		}
		if m := failedStep.FindStringSubmatch(line); m != nil {
			s := found[scenario]
			s.step = m[1]
			found[scenario] = s
			continue
		}
		if m := failedError.FindStringSubmatch(line); m != nil {
			s := found[scenario]
			if s.err == "" {
				s.err = strings.TrimSpace(m[1])
			}
			found[scenario] = s
			scenario = ""
		}
	}
	return found
}
