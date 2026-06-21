import type { Reporter, TestCase, TestResult } from '@playwright/test/reporter'
import { execSync } from 'node:child_process'
import { mkdirSync, copyFileSync, writeFileSync, existsSync } from 'node:fs'
import { join, basename, dirname, relative } from 'node:path'
import { fileURLToPath } from 'node:url'

// Failure-evidence archiver (directive 2026-06-21 file-add-dnd rootcause §2.5).
//
// Playwright clears `test-results/` at the start of each run, so a failure's artefacts are
// gone by the next run — which is exactly why the ab9286b one-shot flake could not be
// root-caused afterward. This reporter copies the COMPLETE failing moment OUT to a
// non-wiped, per-run archive (keyed by timestamp + git commit) the instant a test fails,
// before any later run can wipe it. It does NOT depend on provoking the flake: whenever a
// test ever fails — run 7 or run 700 — its evidence and replay inputs are on disk.
//
// Captured per failing attempt: the Playwright TRACE (which itself carries DOM snapshots,
// console logs, network logs, and per-step timing), VIDEO, SCREENSHOT, the error(s), and
// the replay inputs (test file/line/title, worker + parallel index, retry attempt,
// repeat-each index, project, duration, start time, git commit). A retried-then-passed
// test is still archived for its failing attempt(s) and surfaced as flaky by Playwright —
// never silently green.

const here = dirname(fileURLToPath(import.meta.url))
const webRoot = join(here, '..', '..') // web/
const ARCHIVE_ROOT = join(webRoot, 'playwright-failures')

function gitCommit(): string {
  try {
    return execSync('git rev-parse --short HEAD', { cwd: webRoot }).toString().trim()
  } catch {
    return 'nogit'
  }
}

function sanitize(s: string): string {
  return s.replace(/[^\w.-]+/g, '_').slice(0, 120)
}

export default class FailureArchiver implements Reporter {
  private runId = ''

  onBegin(): void {
    const ts = new Date().toISOString().replace(/[:.]/g, '-')
    this.runId = `${ts}__${gitCommit()}`
  }

  onTestEnd(test: TestCase, result: TestResult): void {
    // Archive every FAILING attempt (incl. attempt 0 of a flaky test that later passes),
    // before the next run clears test-results. A passing attempt is not archived.
    if (result.status !== 'failed' && result.status !== 'timedOut') return

    const dir = join(
      ARCHIVE_ROOT,
      this.runId,
      `${sanitize(test.titlePath().join('__'))}__attempt${result.retry}`,
    )
    mkdirSync(dir, { recursive: true })

    // Copy the artefacts (trace/video/screenshot) out of the soon-to-be-wiped test-results.
    const archived: string[] = []
    for (const a of result.attachments) {
      if (!a.path || !existsSync(a.path)) continue
      const dest = join(dir, `${sanitize(a.name)}-${basename(a.path)}`)
      try {
        copyFileSync(a.path, dest)
        archived.push(basename(dest))
      } catch {
        /* best-effort; metadata below still records the failure */
      }
    }

    const meta = {
      title: test.title,
      titlePath: test.titlePath(),
      file: relative(webRoot, test.location.file),
      line: test.location.line,
      status: result.status,
      // Replay inputs — enough to reconstruct the exact firing afterward.
      attemptRetry: result.retry,
      workerIndex: result.workerIndex,
      parallelIndex: result.parallelIndex,
      repeatEachIndex: test.repeatEachIndex,
      projectName: test.parent.project()?.name ?? 'unknown',
      durationMs: result.duration,
      startTime: result.startTime,
      gitCommit: gitCommit(),
      errors: result.errors.map((e) => e.message ?? String(e)),
      archived,
      replayCommand: `npx playwright test ${relative(webRoot, test.location.file)} -g ${JSON.stringify(test.title)}`,
      hints: [
        'Open the trace: npx playwright show-trace <…-trace.zip> (DOM snapshots + console + network + step timings).',
        'To make a captured firing deterministic, replay with the test-scoped injectable hook at the race window (e.g. DND_INJECT_SHIFT_PX for the file-add DnD resolve-then-dispatch window).',
      ],
    }
    writeFileSync(join(dir, 'failure.json'), JSON.stringify(meta, null, 2))
    // Surface the archive location so it is obvious in the run log (never silently green).
    // eslint-disable-next-line no-console
    console.log(`\n[failure-archiver] archived failing moment → ${relative(webRoot, dir)}\n`)
  }
}
