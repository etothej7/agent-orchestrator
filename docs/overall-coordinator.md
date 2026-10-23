# Overall coordinator

This fork supports one local coordinator above explicitly assigned repository
managers. The coordinator is the main conversation for priorities, dependencies,
progress, and decisions. Each repository manager uses AO's existing workers,
task worktrees, and independent reviewers.

## Setup

1. Add each repository as its own project. Do not add the parent development
   folder as a workspace project: workspace registration can initialize Git.
2. In each repository's project settings, choose its manager, worker, and
   reviewer agents.
3. Open **Settings → General → Overall coordinator**. Choose an agent and click
   **Create coordinator**. This creates a separate plain-directory Scratch
   project inside AO's data directory, without initializing Git.
4. Select the repositories and **Save assignments**. Saving does not start,
   stop, or replace agents. If an affected manager already has an open session,
   finish its work and end that session before assigning its new role.
5. Click **Open coordinator**. Give it the desired outcome in plain language.
   It reuses existing assigned managers or starts missing managers as needed.

GitHub Desktop is optional. Agents work locally in task worktrees; they do not
SSH into repositories. Existing GitHub credentials remain separate from local
task ownership. Per-repository SSH keys are not needed for this hierarchy.

## Reporting and review

Workers report to their own repo manager. Assigned repo managers use
`ao report "summary or suggestion"` to report to the overall coordinator.
The daemon chooses the destination and preserves the originating repository
and session. Reports survive restart and are retried when the recipient returns.
An offline coordinator does not block worker reports in other destinations.

Manager instructions require independent review of the exact revision, checks,
and verification of affected user flows before recommending release. The
coordinator summarizes evidence and remaining gaps for the human. These are
agent workflow instructions, not an automatic merge gate or OS permission
boundary. Existing repository rules still govern pushes, merges, deployments,
secrets, and external messages. This change adds no worker concurrency cap.

Assignment changes reject active lifecycle operations, affected open
orchestrator sessions, and undelivered manager reports. Assigned projects cannot
be removed until their assignments are cleared. These guards preserve role
instructions and report destinations without restarting agents automatically.

## Local validation

Use a separate `AO_DATA_DIR` and `AO_RUN_FILE` for a test daemon. Do not point
a development build at another running installation's state. UI source builds
and browser previews follow the repository's `AGENTS.md` guidance.

The new settings text currently uses English entries in the locale catalogs.
