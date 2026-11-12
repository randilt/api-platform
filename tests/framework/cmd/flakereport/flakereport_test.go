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
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// uiLog is the shape of a UI suite job log: timestamped go test -v output, a runner's godog
// summary printed after its scenarios, and a block that never booted.
const uiLog = `2026-11-12T06:00:00.0000001Z === RUN   TestUISuite
2026-11-12T06:00:00.0000002Z === RUN   TestUISuite/ai-core
2026-11-12T06:00:00.0000003Z === RUN   TestUISuite/ai-core/mcp-refetch
2026-11-12T06:00:00.0000004Z === RUN   TestUISuite/ai-core/mcp-refetch/Refetching_without_any_edits
2026-11-12T06:00:00.0000005Z === RUN   TestUISuite/ai-core/mcp-refetch/Saving_a_URL-only_edit
2026-11-12T06:01:00.0000001Z === NAME  TestUISuite/ai-core/mcp-refetch/Refetching_without_any_edits
2026-11-12T06:01:00.0000002Z     suite.go:640: after scenario hook failed: Locator expected to be visible
2026-11-12T06:01:00.0000003Z         Call log:
2026-11-12T06:01:00.0000004Z           - waiting for getByText('Connection verified').first(), step error: Locator expected to be visible
2026-11-12T06:01:01.0000001Z === ai-core/mcp-refetch ===
2026-11-12T06:01:01.0000002Z --- Failed steps:
2026-11-12T06:01:01.0000003Z
2026-11-12T06:01:01.0000004Z   Scenario: Refetching without any edits # /w/features/mcp.feature:25
2026-11-12T06:01:01.0000005Z     Then the user sees "Connection verified" on the page # /w/features/mcp.feature:34
2026-11-12T06:01:01.0000006Z       Error: after scenario hook failed: Locator expected to be visible
2026-11-12T06:01:01.0000007Z
2026-11-12T06:01:01.0000008Z 2 scenarios (1 passed, 1 failed)
2026-11-12T06:02:00.0000001Z === RUN   TestUISuite/broken-block
2026-11-12T06:08:00.0000001Z     runtime.go:143: block "broken-block" failed to boot after 6m0.394s: runtime: starting component "browser": create container: context deadline exceeded
2026-11-12T06:09:00.0000001Z --- FAIL: TestUISuite (540.00s)
2026-11-12T06:09:00.0000002Z     --- FAIL: TestUISuite/ai-core (90.00s)
2026-11-12T06:09:00.0000003Z         --- FAIL: TestUISuite/ai-core/mcp-refetch (80.00s)
2026-11-12T06:09:00.0000004Z             --- FAIL: TestUISuite/ai-core/mcp-refetch/Refetching_without_any_edits (76.72s)
2026-11-12T06:09:00.0000005Z             --- PASS: TestUISuite/ai-core/mcp-refetch/Saving_a_URL-only_edit (29.08s)
2026-11-12T06:09:00.0000006Z     --- FAIL: TestUISuite/broken-block (360.39s)
2026-11-12T06:09:00.0000007Z FAIL
`

func TestParseLogFindsScenariosAndFailuresAboveThem(t *testing.T) {
	execs, err := parseLog(job{ID: 7, Suite: "ui"}, []byte(uiLog))
	require.NoError(t, err)
	for i := range execs {
		classify(&execs[i])
	}
	require.Len(t, execs, 3)

	failed, passed, boot := execs[0], execs[1], execs[2]
	require.Equal(t, "scenario", failed.Kind)
	require.Equal(t, "ai-core", failed.Block)
	require.Equal(t, "mcp-refetch", failed.Runner)
	require.True(t, failed.Failed)
	require.Equal(t, 76.72, failed.Elapsed)
	require.Equal(t, `Then the user sees "Connection verified" on the page`, failed.Step)
	require.Equal(t, "getByText('Connection verified').first()", failed.Locator)
	require.Equal(t, "ui-not-visible: getByText('Connection verified').first()", failed.Signature)

	require.Equal(t, "Saving_a_URL-only_edit", passed.Scenario)
	require.False(t, passed.Failed)

	require.Equal(t, "infra", boot.Kind)
	require.Equal(t, "broken-block", boot.Block)
	require.Equal(t, "infra-boot/timeout", boot.Category)
}

// An integration block's subtest name has a slash in it (gateway-core/sqlite). The suite's
// failure must still be traced to the scenario, not reported again above it.
func TestParseLogFollowsBlockNamesContainingSlashes(t *testing.T) {
	log := `=== RUN   TestIntegrationSuite
=== RUN   TestIntegrationSuite/gateway-core/sqlite
=== RUN   TestIntegrationSuite/gateway-core/sqlite/search
=== RUN   TestIntegrationSuite/gateway-core/sqlite/search/Search_returns_empty
    suite.go:640: expected status 200, got GET http://x/apis -> 500
--- FAIL: TestIntegrationSuite (10.00s)
    --- FAIL: TestIntegrationSuite/gateway-core/sqlite (9.00s)
        --- FAIL: TestIntegrationSuite/gateway-core/sqlite/search (5.67s)
            --- FAIL: TestIntegrationSuite/gateway-core/sqlite/search/Search_returns_empty (4.52s)
FAIL
`
	execs, err := parseLog(job{Suite: "it"}, []byte(log))
	require.NoError(t, err)
	require.Len(t, execs, 1)
	require.Equal(t, "gateway-core/sqlite", execs[0].Block)
	require.Equal(t, "search", execs[0].Runner)
	require.Equal(t, 4.52, execs[0].Elapsed)
}

// Excerpts of failures diagnosed by hand on #4112, #4114 and #4200.
func TestClassify(t *testing.T) {
	for _, tc := range []struct {
		name, output, category string
	}{
		{"toast read after it hid", `    suite.go:640: after scenario hook failed: playwright: timeout: Timeout 30000ms exceeded.
          - waiting for getByRole('alert'), step error: playwright: timeout: Timeout 30000ms exceeded.`, "ui-timeout/toast"},
		{"text never shown", `          - waiting for getByText('An LLM provider with this ID already exists.').first(), step error: Locator expected to be visible`, "ui-not-visible"},
		{"gateway route never ready", `      Error: after scenario hook failed: waiting for GET http://127.0.0.1:33012/cel-method-hp3e4tpt-4/v1.0.256998/get to return 200: the condition never held; the last response was GET http://127.0.0.1:33012/cel-method-hp3e4tpt-4/v1.0.256998/get -> 404, body="{\"error\":\"Not Found\"}"`, "poll-never-held/last-404"},
		{"image deleted before boot", `    runtime.go:143: block "management-portal-deploy" failed to boot after 6m0.394s: runtime: create container: error from registry: unauthorized`, "infra-boot/image-missing"},
		{"portal build out of memory", `    suite_test.go:176: building source images: build api-control-plane: command 1: exit status 1`, "infra-build"},
		{"empty selection", `    runtime.go:122: the suite executed NO scenarios — every runner matched nothing`, "infra-no-scenarios"},
		{"unknown", `    something odd happened`, "other"},
	} {
		e := execution{Failed: true, output: tc.output}
		classify(&e)
		require.Equal(t, tc.category, e.Category, tc.name)
		require.True(t, strings.HasPrefix(e.Signature, tc.category+": "), tc.name)
	}
}

func TestNormalizeMakesRepeatsOfOneFailureEqual(t *testing.T) {
	a := normalize(`GET http://127.0.0.1:33012/cel-method-hp3e4tpt-4/get after 6m0.394s, id 9f86d081884c7d65`)
	b := normalize(`GET http://127.0.0.1:41877/cel-method-k2l9xq1a-2/get after 5m59.1s, id 2c26b46b68ffc68f`)
	require.Equal(t, a, b)
}

func TestVerdicts(t *testing.T) {
	day := func(d int) time.Time { return time.Date(2026, 11, d, 0, 0, 0, 0, time.UTC) }
	run := func(d int, branch, sha string, failed bool) point {
		return point{HeadSHA: sha, Branch: branch, Started: day(d), Failed: failed}
	}
	for _, tc := range []struct {
		name    string
		history []point
		want    []string
	}{
		{"failed then passed on a rerun of one commit", []point{
			run(1, "a", "s1", true), run(1, "a", "s1", false), run(2, "b", "s2", false),
		}, []string{verdictProvenFlaky}},
		{"failed for everyone for a while", []point{
			run(1, "a", "s1", false), run(2, "b", "s2", true), run(3, "c", "s3", true), run(4, "d", "s4", true), run(5, "e", "s5", false),
		}, []string{verdictBrokenWindow}},
		{"only one branch fails it", []point{
			run(1, "a", "s1", true), run(2, "b", "s2", false), run(3, "a", "s3", true),
		}, []string{verdictBranchOnly}},
		{"several branches, passes between", []point{
			run(1, "a", "s1", true), run(2, "b", "s2", false), run(3, "c", "s3", true), run(4, "d", "s4", false),
		}, []string{verdictLikelyFlaky}},
		{"once", []point{run(1, "a", "s1", false), run(2, "b", "s2", true)}, []string{verdictOneOff}},
	} {
		got, _ := verdicts(tc.history)
		require.Equal(t, tc.want, got, tc.name)
	}
}

// Many scenarios of one block failing in one job are one incident: they don't make each
// scenario look flaky.
func TestAnalyzeSeparatesIncidents(t *testing.T) {
	j1 := job{ID: 1, Suite: "it", Branch: "a", HeadSHA: "s1", Started: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}
	j2 := job{ID: 2, Suite: "it", Branch: "b", HeadSHA: "s2", Started: time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)}
	var execs []execution
	for i := range incidentSize {
		name := fmt.Sprintf("Scenario_%d", i)
		execs = append(execs, execution{job: j1, Kind: "scenario", Block: "core", Runner: "r", Scenario: name, Failed: true, Signature: "service-unreachable: x"})
		execs = append(execs, execution{job: j2, Kind: "scenario", Block: "core", Runner: "r", Scenario: name})
	}
	// One isolated failure, of an outline example: it groups with its scenario.
	execs = append(execs, execution{job: j2, Kind: "scenario", Block: "core", Runner: "r", Scenario: "Scenario_0#01", Failed: true, Signature: "other: y"})

	a := analyze(execs)
	require.Len(t, a.Incidents, 1)
	require.Equal(t, incidentSize, a.Incidents[0].Scenarios)
	require.True(t, execs[0].Incident)
	require.Len(t, a.Groups, 1, "only the isolated failure ranks a scenario")
	require.Equal(t, "Scenario_0", a.Groups[0].Scenario)
	require.Equal(t, 1, a.Groups[0].Fails)
	require.Equal(t, 1, a.Groups[0].IncidentFails)
}

func TestWindow(t *testing.T) {
	now := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	from, to, err := window("30d", "", now)
	require.NoError(t, err)
	require.Equal(t, now, to)
	require.Equal(t, now.AddDate(0, 0, -30), from)

	from, to, err = window("2026-10-01", "2026-10-31", now)
	require.NoError(t, err)
	require.Equal(t, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), from)
	require.Equal(t, time.Date(2026, 10, 31, 23, 59, 59, 0, time.UTC), to)

	_, _, err = window("0d", "", now)
	require.Error(t, err)
}
