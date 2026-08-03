---
name: resolve-pr-comments-stack
description: Resolve unresolved PR review comments across an entire Graphite (gt) stack of many PRs, bottom-up, in one working directory. Use when asked to "go through this stack and resolve comments", "clean up review comments across the whole stack", or given a list/range of PR numbers that form (or partially form) a gt stack. Distinct from the single-PR resolve-pr-comments skill, which this one uses as its per-branch inner loop.
allowed-tools: Read, Grep, Glob, Bash, Edit, Write, WebFetch, AskUserQuestion
---

# Resolve PR Comments — Stack

Systematically resolve unresolved review comments (CodeRabbit, Greptile, human reviewers)
across every branch in a Graphite stack, working bottom-up in a single checkout. Each
branch's fixes are committed into that branch via `gt modify` before moving to the next,
so the stack stays restacked and consistent throughout — no branch is left dirty, and no
branch is worked on out of dependency order.

This is the stack-wide orchestration layer. The per-PR triage logic (fetch threads,
classify FIX/REPLY/SKIP, apply) is the same as the single-PR `resolve-pr-comments` skill;
this skill adds the ordering, persistence, and cross-branch judgment on top.

## When to reach for this vs. the single-PR skill

Use this skill when the target is a *stack* — a list of PR numbers with dependency order,
or "go through this whole thing". Use the plain `resolve-pr-comments` skill for a single,
standalone PR. A stack pass invokes the single-PR triage logic once per branch internally;
it does not require calling that skill separately.

## Step 0: Establish stack order and confirm working mode

Get the real dependency order, don't trust an externally supplied list blindly:

```bash
gt log --stack
```

Cross-reference against whatever PR list was given. Confirm with the user (`AskUserQuestion`)
only if genuinely ambiguous — e.g., the given list doesn't obviously map onto one contiguous
run of the stack, or ordering conflicts with what `gt log` shows. Otherwise proceed bottom-up
(base branches first): a later branch's design routinely supersedes an earlier one's, and
working bottom-up means each branch is fixed before its descendants are even touched.

Work **serially, in place, in one working directory** — checkout each branch in turn, never
use worktrees for this. Multi-branch work with a shared identity (this same PR-comment pass)
belongs in one checkout, not parallel worktrees.

## Step 1: Per-branch loop

For each branch, bottom-up:

```bash
git status --short                                       # must be empty - if not, stop and resolve first
gt checkout <branch>
gh pr view <number> --json number,title,url,headRefName   # confirm branch <-> PR mapping
```

Compare the checked-out branch name against the returned `headRefName` before doing
anything else — `--json number,title,url` alone doesn't expose branch metadata, so a
silent mismatch (wrong branch checked out for this PR number) would otherwise go
unnoticed until comments are posted against the wrong PR.

Fetch unresolved review threads via GraphQL — the REST API does not expose resolved
status. `reviewThreads` returns at most 100 threads per request; paginate with
`pageInfo.hasNextPage`/`endCursor` (same cursor loop as the single-PR
`resolve-pr-comments` skill's Step 2) so a PR with more review activity than that
doesn't silently drop threads on later pages:

```bash
gh api graphql -f query='
{
  repository(owner: "OWNER", name: "REPO") {
    pullRequest(number: N) {
      reviewThreads(first: 100, after: AFTER_CURSOR_OR_NULL) {
        pageInfo { hasNextPage endCursor }
        nodes {
          isResolved
          comments(first: 1) { nodes { databaseId path body } }
        }
      }
    }
  }
}' | jq -r '.data.repository.pullRequest.reviewThreads.nodes[] | select(.isResolved == false) | "\(.comments.nodes[0].databaseId)|\(.comments.nodes[0].path)|\(.comments.nodes[0].body[0:200])"'
```

Repeat with the previous page's `endCursor` while `hasNextPage` is `true`.

For each unresolved comment, fetch the comment **and every existing reply in its
thread** — not just the single comment — before triaging:

```bash
gh api repos/OWNER/REPO/pulls/NUMBER/comments --paginate | jq '.[] | select(.id == ID or .in_reply_to_id == ID)'
```

A bare single-comment fetch misses a reply already posted (this session or a previous
one), which risks re-litigating an already-settled thread. Then triage:

- **FIX** — the finding is valid against current code. Make the change, keep it minimal,
  build, run targeted tests. No reply needed — silence plus the eventual push is the
  verification signal reviewers expect.
- **REPLY** — the comment is stale, a duplicate, already resolved by a **later** branch in
  this same stack (cite the PR number and what changed there), or **factually wrong**.
  CodeRabbit findings are not automatically correct: verify claims against actual code
  (grep the codebase, read the referenced schema/config) before accepting or declining a
  finding — declining with a cited, checked reason is a valid and expected outcome, not a
  failure to fix something. Post the reply immediately (`gh api .../replies -X POST`);
  it doesn't need to wait for a push, since it's not claiming a code fix.
- **SKIP** — out of scope, or a pre-existing/unrelated issue (e.g. a schema-sync gap not
  touched by this PR). Don't scope-creep into fixing it; note it for the final summary
  instead.

**Before executing any FIX or REPLY, present the finding, your proposed action, and your
reasoning to the user and wait for their decision** (`AskUserQuestion` with FIX/REPLY/SKIP
options) — mirroring the single-PR `resolve-pr-comments` skill's Step 4 gate. Never apply
a code change or post a reply unprompted, even mid-pass across many PRs; the gate is
per-comment, not once per branch or once per stack.

Once every unresolved comment on this branch is triaged and every approved FIX is applied:

```bash
go build ./<affected packages>/...     # or the relevant language's equivalent
go test ./<affected packages>/... -race  # -race for concurrency-sensitive changes
git status --short                     # must show only the files this branch's fixes touched
gt modify -a -m "<commit message describing the fixes>"
```

`gt modify` amends the branch's tip with the staged changes and auto-restacks every
descendant branch — this is the correct persistence mechanism for a stack pass, not a plain
`git commit` (which wouldn't restack) and not leaving changes uncommitted (which would carry
into the next `gt checkout` and corrupt it).

After the push (or immediately for a REPLY-only comment), re-fetch this branch's unresolved
threads. A bot-authored thread (CodeRabbit, Greptile) auto-resolves once it re-scans the
pushed diff, so a leftover bot thread right after a fix is expected, not a failure signal.
A **human-reviewer** thread does not auto-resolve — it stays open until the reviewer closes
it. Treat a fix addressing a human reviewer's comment as complete once you've replied
explaining what changed (never "Fixed" — see Hard Rule 1) and report it as pending human
review in the final summary, not as an error to retry. Then move to the next branch.

## Hard rules

1. **Never post a "Fixed" reply after a genuine code FIX.** Only reply for
   stale/duplicate/resolved-elsewhere/incorrect-claim cases (see Step 1). A code fix speaks
   for itself once pushed.
2. **Never manually resolve GitHub review threads** (no `resolveReviewThread` GraphQL
   mutation). CodeRabbit and Greptile auto-resolve their own threads once the branch is
   pushed; manually resolving is redundant and was explicitly called out as unwanted.
3. **Never `git stash`** in a repo where `go.mod`/toolchain files churn across branches —
   stash pops conflict. Use `gt modify` to persist per-branch instead of stashing to move
   between branches.
4. **No worktrees for this pass.** One feature/pass at a time in the main checkout.
5. **`gt modify`, not ad hoc `git commit`, between branches** — even though the general rule
   elsewhere is "don't commit without asking," a stack-wide comment pass is the documented
   exception: leaving many branches simultaneously dirty isn't viable, and `gt modify` is the
   stack-native persistence step, not a scope-creeping commit.
6. **Don't fix unrelated pre-existing failures** discovered along the way (e.g. a schema-sync
   test failing for reasons unconnected to any review comment). Flag them in the final
   summary; fixing them is a separate, explicitly-requested task.

## If `gt modify` hits a restack conflict

When amending a branch's fixes causes a conflict while restacking a descendant branch,
stop the per-branch loop where you are — don't check out elsewhere or start a fresh
`gt restack`. This is the same recovery procedure documented in the `stack-absorb`
skill's Step 4; reuse it directly:

1. Stay on the conflicted branch with the operation paused — the conflict markers are
   already in the working tree. `gt abort` only cancels the operation entirely.
2. `grep -n "^<<<<<<<\|^=======\|^>>>>>>>" "<file>"` to locate every conflict region.
3. Resolve each region by hand against the current descendant's content — the common
   case is "keep both sides" (two independent changes near the same spot).
4. Build/test the affected code, then confirm no markers remain:
   `grep -cE "^(<<<<<<<|=======|>>>>>>>)" "<file>"` should be 0.
5. `gt add "<file>"` for every resolved file, then `gt continue`.
6. Repeat if `gt continue` hits another conflict further up the stack.
7. Once the restack completes, re-check `git status --short` (clean) and `gt log short`
   (no "(needs restack)") before the next `gt checkout`.

## Judgment calls worth calling out explicitly

- **Merge conflicts during `gt modify`'s restack**: when a later branch's independent
  changes conflict with an earlier branch's fix, don't default to either side blindly.
  Read both, determine whether the later branch's design supersedes the earlier fix's
  *mechanism* while the *intent* still needs to be preserved (re-implement it against the
  newer structure), or whether the earlier fix is simply still correct and the later
  branch's change should adapt instead. Explain the reasoning if the user asks.
- **CodeRabbit/Greptile findings are hypotheses, not verdicts.** Before fixing, especially
  for "this violates the documented contract" or "this restriction exists in code" style
  claims, verify against the actual schema/config/code (a grep or a short subagent
  investigation is cheap compared to shipping a fix — or worse, documentation — for a
  restriction that doesn't exist).
- **A comment resolved by a later PR in the stack** still needs a reply on the *earlier*
  PR citing the later PR number and what changed there — the thread on the earlier PR
  won't auto-resolve just because a later branch fixed the same thing. Since the pass is
  bottom-up, you won't always know this yet while sitting on the earlier branch: if an
  earlier branch's comment looks like something a *later* branch in this same stack is
  likely to address (a design that visibly needs to change further upstack, a TODO
  pointing at later work), don't guess — track it (comment ID, PR number, why you suspect
  it) instead of resolving it on the spot. After the full bottom-up pass completes, revisit
  each tracked comment: if a later branch did in fact fix it, post the required reply on
  the earlier PR citing that later PR number; if not, go back and triage it properly
  (FIX/REPLY/SKIP) like any other comment.

## Step 2: Final summary

After the last branch (including the deferred revisit pass above), report: what was fixed
per PR, what was replied-to and why, any human-reviewer threads left pending their review,
and any out-of-scope issues flagged along the way (per Hard Rule 6) for the user to pick up
separately.
