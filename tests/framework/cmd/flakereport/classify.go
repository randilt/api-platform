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
	"regexp"
	"strings"
)

// rule names one kind of failure. The first rule whose pattern matches the failure's text
// wins; sub, when set, refines the category from the same text.
type rule struct {
	category string
	pattern  *regexp.Regexp
	sub      func(text string) string
}

var (
	imageMissing = regexp.MustCompile(`unauthorized|pull access denied|No such image|manifest unknown`)
	lastResponse = regexp.MustCompile(`last response was .*?-> (\d{3})`)
	apiErrorCode = regexp.MustCompile(`\\?"code\\?":\s*\\?"([A-Z][A-Z_]+)\\?"`)
)

var rules = []rule{
	{"infra-build", regexp.MustCompile(`building source images|heap out of memory`), nil},
	{"infra-boot", regexp.MustCompile(`failed to boot`), func(t string) string {
		switch {
		case strings.Contains(t, "context deadline exceeded"):
			return "timeout"
		case imageMissing.MatchString(t):
			return "image-missing"
		}
		return ""
	}},
	{"infra-no-scenarios", regexp.MustCompile(`executed NO scenarios`), nil},
	{"ui-not-visible", regexp.MustCompile(`Locator expected to be visible`), toast},
	{"ui-text-mismatch", regexp.MustCompile(`Locator expected to (have|contain) text`), toast},
	{"ui-timeout", regexp.MustCompile(`Timeout \d+ms exceeded`), toast},
	{"service-unreachable", regexp.MustCompile(`connect: connection refused|: EOF\b|no such host|connection reset by peer`), nil},
	{"poll-never-held", regexp.MustCompile(`condition never held|observed=|never succeeded within|did not settle`), func(t string) string {
		if m := lastResponse.FindStringSubmatch(t); m != nil {
			return "last-" + m[1]
		}
		return ""
	}},
	{"http-status", regexp.MustCompile(`(?i)(HTTP|status( code)?) \d{3},? (want|expected)|expected status|want \d{3}`), func(t string) string {
		if m := apiErrorCode.FindStringSubmatch(t); m != nil {
			return m[1]
		}
		return ""
	}},
	{"assertion", regexp.MustCompile(`(?i)expected|mismatch|not equal|should`), nil},
}

// toast marks a failure waiting on a notification toast, which hides itself after a few
// seconds: a slow runner can look for it after it is gone.
func toast(t string) string {
	if strings.Contains(t, "getByRole('alert')") {
		return "toast"
	}
	return ""
}

var (
	stepErrorLine = regexp.MustCompile(`waiting for (.+?)(?:, step error: (.*))?$`)
	hookFailed    = regexp.MustCompile(`hook failed: (.*)`)
	errorLine     = regexp.MustCompile(`Error: (.*)`)
	goTestLine    = regexp.MustCompile(`^\s*(=== |--- |\S+\.go:\d+: runner )`)
)

// classify fills the failure's message, locator, category and signature.
func classify(e *execution) {
	if !e.Failed {
		return
	}
	lines := strings.Split(e.output, "\n")
	for _, l := range lines {
		if m := stepErrorLine.FindStringSubmatch(l); m != nil && e.Locator == "" {
			e.Locator = strings.TrimSpace(m[1])
			if m[2] != "" && e.Message == "" {
				e.Message = strings.TrimSpace(m[2])
			}
		}
	}
	for _, re := range []*regexp.Regexp{hookFailed, errorLine} {
		for _, l := range lines {
			if e.Message != "" {
				break
			}
			if m := re.FindStringSubmatch(l); m != nil {
				e.Message = strings.TrimSpace(m[1])
			}
		}
	}
	if e.Message == "" {
		for _, l := range lines {
			if t := strings.TrimSpace(l); t != "" && !goTestLine.MatchString(l) {
				e.Message = t
				break
			}
		}
	}
	text := e.Message + "\n" + e.Locator + "\n" + e.output
	e.Category = "other"
	for _, r := range rules {
		if r.pattern.MatchString(text) {
			e.Category = r.category
			if r.sub != nil {
				if s := r.sub(text); s != "" {
					e.Category += "/" + s
				}
			}
			break
		}
	}
	detail := e.Message
	if e.Locator != "" {
		detail = e.Locator
	}
	e.Signature = e.Category + ": " + normalize(detail)
}

var (
	hookPrefix = regexp.MustCompile(`^(?:after scenario hook failed: )+`)
	stepTail   = regexp.MustCompile(`, step error: .*$`)
)

var normalizers = []struct {
	re   *regexp.Regexp
	with string
}{
	{regexp.MustCompile(`https?://[^\s"',)]+`), "<url>"},
	{regexp.MustCompile(`-[a-z0-9]{8}-\d+\b`), "-<uniq>"},
	{regexp.MustCompile(`\b[0-9a-f]{8,}\b`), "<id>"},
	{regexp.MustCompile(`\b\d+(?:\.\d+)?(?:h\d+(?:\.\d+)?)?(?:m\d+(?:\.\d+)?)?(?:ms|s|m|h)\b`), "<dur>"},
	{regexp.MustCompile(`\b\d+(\.\d+)?(ms|s|m)?\b`), "N"},
}

// normalize removes what differs between two occurrences of the same failure: generated
// handles, ids, ports, durations.
func normalize(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	s = stepTail.ReplaceAllString(hookPrefix.ReplaceAllString(s, ""), "")
	for _, n := range normalizers {
		s = n.re.ReplaceAllString(s, n.with)
	}
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return strings.TrimSpace(s)
}
