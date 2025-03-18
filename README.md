# prmaster

CLI PR management for OCD developers.

## Usage

```shell
$ go install github.com/benesch/prmaster@latest
$ prmaster <list|sync>
```

To use a PR flow based on personal forks:

```
$ git config prmaster.personalRemote benesch
```

To use a PR flow based on pushing directly to the origin:

```
$ git config prmaster.personalRemote none
$ git config prmaster.branchPrefix benesch/
```

To access a private repository, you'll need to provide prmaster with a personal
access token that has access to the desired repository:

```
$ git config prmaster.githubToken ghp_Kre5dO1...
```

Consider adding a personal access token even if you're only using prmaster with
public repositories. Authenticated requests to the GitHub API enjoy a much
higher rate limit than unauthenticated requests.

## Example

Running in [cockroachdb/cockroach]:

<img src="screenshot.png" width="485" />

```shell
$ prmaster sync
Skipping better-gce-sync. Not associated with any PRs.
Skipping cgosymbolizer-darwin. PR #24245 is still open.
Skipping gceworker-test. PR #20468 is still open.
Deleted local keys-doc. PR #25032 is closed.
Deleted remote keys-doc. PR #25032 is closed.
Skipping logictest-sort. Not associated with any PRs.
Skipping mandatory-addressing. PR #25061 is still open.
Skipping master. Looks like a release branch.
Skipping range-deletions. PR #24057 is still open.
Skipping rfc-range-merges. PR #24394 is still open.
Skipping role-patch. Not associated with any PRs.
Skipping srf. Not associated with any PRs.
Skipping xmake. Not associated with any PRs.

These remote branches do not have open PRs:
    better-gce-sync
    logictest-sort
    range-merges
    role-patch

    Manage: https://github.com/benesch/cockroach/branches/yours

These local branches do not exist on your remote:
    srf
    xmake

Running `git remote prune origin`.
```

[cockroachdb/cockroach]: https://github.com/cockroachdb/cockroach
