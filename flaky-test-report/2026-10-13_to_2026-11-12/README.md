# Flaky test report: 2026-10-13 to 2026-11-12

Every UI and integration suite job of the Platform Tests workflow in this window, read from
the GitHub Actions logs: 268 runs on 80 branches, 310 UI jobs and 716 integration jobs, about
2.57 million scenario results.

| File | What it is |
|---|---|
| [`report.md`](report.md) | The findings: scenarios ranked by failures with a verdict for each, incidents, failure categories and signatures, a weekly trend, the slowest scenarios, and how to read the verdicts. Start here. |
| `allure-report.html` | The same failures in [Allure 3](https://allurereport.org). One self-contained page: download it and open it in a browser. GitHub doesn't render it. |
| `executions.jsonl.gz` | The raw data: one JSON line for every run of every scenario that failed at least once, and every failure above a scenario. |

## Opening the Allure report

Open `allure-report.html` in a browser. There's nothing to install, and it works offline.

- **Categories:** failures grouped by cause (`service-unreachable`, `http-status`, `ui-timeout`, …).
- **A scenario's page:** its latest failure, with its other failures under **Retries**, each with a link to the GitHub job.
- **Flaky:** set from this report's verdicts (*proven flaky*, *likely flaky*).
- **`(incidents)` suite:** one entry per incident, where 5 or more scenarios of one block failed together in one job. Their individual failures are counted in `report.md` only.

## Regenerating

The tool is [`tests/framework/cmd/flakereport`](../../tests/framework/cmd/flakereport). It needs the
GitHub CLI signed in (`gh auth login`), or `GITHUB_TOKEN` set, plus Go and Node. From `tests/framework`:

```sh
go run ./cmd/flakereport -since 30d -out /tmp/flake -cache ~/.cache/flakereport
# or a fixed window:
go run ./cmd/flakereport -since 2026-10-13 -until 2026-11-12 -out /tmp/flake -cache ~/.cache/flakereport
```

- **First run:** about 1.5 hours, because GitHub serves job logs at about 10 a minute.
- **Later runs:** read the cache and download only new jobs, then take about 7 minutes and 6 GB of memory.

## Limits

- **Only Platform Tests jobs are read.** They run the suites with `-v`, which prints every scenario's result. The nightly Codecov run doesn't, so it's not included.
- **Cancelled jobs aren't counted:** 223 in this window, superseded by a newer push.
- **Excluded scenarios never appear:** scenarios tagged `@known-issue` aren't run. From #4173 (merged on 2026-11-12, the window's last day) a pull request runs only the integration blocks it can affect, so later windows see fewer integration runs per pull request.
- **Categories come from rules on the failure text.** `other` means no rule matched yet.
