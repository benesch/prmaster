package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/google/go-github/github"
	color "github.com/logrusorgru/aurora"
	"github.com/pkg/errors"
	"github.com/vbauerster/mpb"
	"github.com/vbauerster/mpb/decor"
	"golang.org/x/oauth2"
	"golang.org/x/sync/errgroup"
)

const usage = `usage: prmaster sync [-n]
          or: prmaster list`

func main() {
	if err := run(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %s\n", err)

		cause := errors.Cause(err)
		if _, ok := cause.(*github.RateLimitError); ok {
			fmt.Fprintln(os.Stderr, `hint: unauthenticated GitHub requests are subject to a very strict rate
limit. Please configure prmaster with a personal access token:
    $ git config --global prmaster.githubToken TOKEN
For help creating a personal access token, see https://goo.gl/Ep2E6x.`)
		} else if err, ok := cause.(hintedErr); ok {
			fmt.Fprintf(os.Stderr, "hint: %s\n", err.hint)
		}

		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	if len(os.Args) < 2 {
		return errors.New(usage)
	}

	c, err := loadConfig(ctx)
	if err != nil {
		return err
	}

	switch cmd := os.Args[1]; cmd {
	case "list":
		return runList(ctx, c)
	case "sync":
		return runSync(ctx, c)
	default:
		return fmt.Errorf("unknown command %s", cmd)
	}
}

func runList(ctx context.Context, c config) error {
	if len(os.Args) != 2 {
		return errors.New(usage)
	}
	opts := &github.SearchOptions{
		Sort: "created",
	}
	query := fmt.Sprintf("type:pr is:open repo:%s/%s author:%s",
		c.upstreamUsername, c.repo, c.username)
	res, _, err := c.ghClient.Search.Issues(ctx, query, opts)
	if err != nil {
		return err
	}
	for _, issue := range res.Issues {
		pr, _, err := c.ghClient.PullRequests.Get(ctx, c.upstreamUsername, c.repo, *issue.Number)
		if err != nil {
			return err
		}
		dateColor := color.Green
		if time.Since(*pr.CreatedAt) > 30*24*time.Hour {
			dateColor = color.Red
		} else if time.Since(*pr.CreatedAt) > 7*24*time.Hour {
			dateColor = color.Brown
		}
		fmt.Printf(
			"%s\n    Branch %s. Opened %s.\n    https://github.com/%s/%s/pull/%d\n",
			color.Bold(*pr.Title), color.Cyan(*pr.Head.Ref),
			dateColor(pr.CreatedAt.Format("2006-01-02")),
			c.upstreamUsername, c.repo, *pr.Number)
	}
	return nil
}

func runSync(ctx context.Context, c config) error {
	var dryRun bool
	flagSet := flag.NewFlagSet("sync", flag.ContinueOnError)
	flagSet.BoolVar(&dryRun, "n", false, "don't actually delete any branches")
	if err := flagSet.Parse(os.Args[2:]); err != nil {
		return err
	} else if flagSet.NArg() != 0 {
		return errors.New(usage)
	}

	colorDelete := color.Brown("Deleting")
	if dryRun {
		colorDelete = color.Brown("Would delete")
	}

	branches, err := loadBranches(ctx, c)
	if err != nil {
		return err
	}

	currentBranch, err := capture("git", "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return err
	}

	var localDeletes, remoteDeletes []*branch
	for i := range branches {
		b := &branches[i]
		colorName := color.Bold(b.name)
		if b.isRelease() {
			fmt.Printf("Skipping %s. Looks like a release branch.\n", colorName)
			continue
		}
		if b.name == currentBranch {
			fmt.Printf("Skipping %s. It's checked out in your current worktree.\n", colorName)
			continue
		}
		if b.pr.commit == nil {
			fmt.Printf("Skipping %s. Not associated with any PRs.\n", colorName)
			continue
		}
		if b.pr.GetState() == "open" {
			fmt.Printf("Skipping %s. PR #%d is still open.\n", colorName, b.pr.GetNumber())
			continue
		}
		if b.remote != nil {
			if b.remote.sha == b.pr.sha || b.remote.commitDate.Before(b.pr.commitDate) {
				remoteDeletes = append(remoteDeletes, b)
				fmt.Printf("%s remote %s. PR #%d is closed.\n", colorDelete,
					colorName, b.pr.GetNumber())
			} else {
				fmt.Printf("Skipping remote %s. Branch commit is newer than #%d.\n",
					colorName, b.pr.GetNumber())
			}
		}
		if b.local != nil {
			if b.local.sha == b.pr.sha || b.local.commitDate.Before(b.pr.commitDate) {
				localDeletes = append(localDeletes, b)
			} else {
				fmt.Printf("Skipping local %s. Branch commit is newer than #%d.\n",
					colorName, b.pr.GetNumber())
			}
		}
	}

	if !dryRun && len(localDeletes) > 0 {
		args := []string{"git", "branch", "-qD"}
		for _, b := range localDeletes {
			args = append(args, b.name)
			b.local = nil
		}
		fmt.Printf("Deleting %d local branches...\n", len(localDeletes))
		if err := spawn(args...); err != nil {
			return fmt.Errorf("deleting local branches: %s", err)
		}
	}
	if len(remoteDeletes) > 0 {
		if !dryRun {
			args := []string{"git", "push", "-qd", c.remote}
			for _, b := range remoteDeletes {
				args = append(args, b.name)
				b.remote = nil
			}
			fmt.Printf("Deleting %d remote branches...\n", len(remoteDeletes))
			if err := spawn(args...); err != nil {
				return fmt.Errorf("deleting remote branches: %s", err)
			}
		} else {
			fmt.Printf("Would delete %d remote branches.\n", len(remoteDeletes))
		}
	}

	if noPRBranches := branches.filter(func(b branch) bool {
		return !b.isRelease() && b.remote != nil && b.pr.commit == nil
	}); len(noPRBranches) > 0 {
		fmt.Println()
		fmt.Println("These remote branches do not have open PRs:")
		for _, b := range noPRBranches {
			fmt.Printf("    %s\n", b.name)
		}
		fmt.Println()
		fmt.Printf("    Manage: https://github.com/%s/%s/branches/yours\n", c.username, c.repo)
	}

	if localOnlyBranches := branches.filter(func(b branch) bool {
		return !b.isRelease() && b.local != nil && b.remote == nil
	}); len(localOnlyBranches) > 0 {
		fmt.Println()
		fmt.Println("These local branches do not exist on your remote:")
		for _, b := range localOnlyBranches {
			fmt.Printf("    %s\n", b.name)
		}
	}

	fmt.Println()
	if !dryRun {
		fmt.Printf("Running `git remote prune %s`...\n", c.remote)
		return spawn("git", "remote", "prune", c.remote)
	}
	fmt.Printf("Would run `git remote prune %s`.\n", c.remote)
	return nil
}

type commit struct {
	sha        string
	commitDate time.Time
}

func newCommit(repoCommit *github.RepositoryCommit) *commit {
	return &commit{
		sha:        repoCommit.GetSHA(),
		commitDate: repoCommit.GetCommit().GetCommitter().GetDate(),
	}
}

type branch struct {
	name   string
	local  *commit
	remote *commit
	pr     struct {
		*commit
		*github.PullRequest
	}
}

func loadBranches(ctx context.Context, c config) (branches, error) {
	var branches branches

	username := c.upstreamUsername
	if c.personal {
		username = c.username
	}

	// Collect remote branches.
	for page := 1; page != 0; {
		ghBranches, res, err := c.ghClient.Repositories.ListBranches(
			ctx, username, c.repo, &github.ListOptions{PerPage: 100, Page: page})
		if err != nil {
			return nil, err
		}
		for _, b := range ghBranches {
			if strings.HasPrefix(b.GetName(), c.branchPrefix) {
				branches = append(branches, branch{
					name:   b.GetName(),
					remote: newCommit(b.GetCommit()),
				})
			}
		}
		page = res.NextPage
	}

	// Collect local branches.
	out, err := capture("git", "for-each-ref", "--format",
		"%(refname:short)\t%(objectname)\t%(committerdate:iso8601-strict)", "refs/heads")
	if err != nil {
		return nil, err
	}
outer:
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			return nil, errors.New("`git for-each-ref` produced unexpected output")
		}
		name, sha := fields[0], fields[1]
		date, err := time.Parse(time.RFC3339, fields[2])
		if err != nil {
			return nil, err
		}
		commit := &commit{sha: sha, commitDate: date}
		for i := range branches {
			if branches[i].name == name {
				branches[i].local = commit
				continue outer
			}
		}
		if strings.HasPrefix(name, c.branchPrefix) {
			branches = append(branches, branch{name: name, local: commit})
		}
	}

	// Attach PR, if any.
	progress := mpb.New(mpb.WithWidth(42))
	defer progress.Wait()
	bar := progress.AddBar(int64(len(branches)), mpb.BarRemoveOnComplete(),
		mpb.PrependDecorators(
			decor.StaticName("Fetching PR ", 0, 0),
			decor.CountersNoUnit("%d / %d", 7, 0)),
		mpb.AppendDecorators(
			decor.ETA(0, 0),
			decor.StaticName(" remaining", 0, 0)))
	var g errgroup.Group
	// Limit concurrency. The GitHub API doesn't like too many concurrent
	// requests. It may throw a "secondary rate limit" error if it observed too
	// much concurrency. This is true even for authenticated users and even if
	// the total rate of requests stays below the "primary" rate limit.
	//
	// From https://docs.github.com/en/rest/guides/best-practices-for-integrators#dealing-with-secondary-rate-limits:
	// > Make requests for a single user or client ID serially. Do not make
	// > requests for a single user or client ID concurrently.
	sem := make(chan struct{}, 32)
	for i := range branches {
		i := i
		g.Go(func() error {
			sem <- struct{}{}
			defer func() { <-sem }()
			prOpts := &github.PullRequestListOptions{
				State: "all",
				Head:  fmt.Sprintf("%s:%s", username, branches[i].name),
			}
			prs, _, err := c.ghClient.PullRequests.List(ctx, c.upstreamUsername, c.repo, prOpts)
			if err != nil {
				return err
			}
			if len(prs) != 0 {
				// PRs are sorted so that the most recent PR is first.
				pr := prs[0]
				commits, _, err := c.ghClient.PullRequests.ListCommits(ctx, c.upstreamUsername,
					c.repo, pr.GetNumber(), nil /* listOptions */)
				if err != nil {
					return err
				}
				if len(commits) == 0 {
					// TODO: Is this an error?
					return nil
				}
				branches[i].pr.PullRequest = pr
				branches[i].pr.commit = newCommit(commits[len(commits)-1])
			}
			bar.Increment()
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		progress.Abort(bar)
		return nil, err
	}

	return branches, nil
}

var releaseMatcher = regexp.MustCompile(`master|release-\d`)

func (b *branch) isRelease() bool {
	return releaseMatcher.MatchString(b.name)
}

type branches []branch

func (bs branches) filter(fn func(branch) bool) branches {
	var out branches
	for _, b := range bs {
		if fn(b) {
			out = append(out, b)
		}
	}
	return out
}

type config struct {
	ghClient         *github.Client
	upstreamUsername string
	repo             string
	remote           string
	username         string
	personal         bool
	branchPrefix     string
	gitDir           string
}

var errNoRemote = errors.New("remote does not exist")

func tryUpstream(remote string) (upstreamUsername, repo string, err error) {
	upstreamURL, _ := capture("git", "config", "--get", fmt.Sprintf("remote.%s.url", remote))
	if upstreamURL == "" {
		return "", "", errNoRemote
	}
	m := regexp.MustCompile(`github.com(:|/)([[:alnum:]\-]+)/([[:alnum:]\-]+)`).FindStringSubmatch(upstreamURL)
	if len(m) != 4 {
		return "", "", errors.Errorf("unable to guess upstream GitHub information from remote %q (%s)",
			remote, upstreamURL)
	}
	return m[2], m[3], nil
}

func loadConfig(ctx context.Context) (config, error) {
	var c config

	// Determine upstream username and repo.
	var err error
	upstreamRemote := "upstream"
	c.upstreamUsername, c.repo, err = tryUpstream("upstream")
	if err != nil {
		if err != errNoRemote {
			return c, err
		}
		upstreamRemote = "origin"
		c.upstreamUsername, c.repo, err = tryUpstream("origin")
		if err == errNoRemote {
			return c, hintedErr{
				error: errors.New("unable to guess upstream GitHub information"),
				hint: `ensure you have a remote named either "upstream" or "origin" that is
configured with a GitHub URL`,
			}
		} else if err != nil {
			return c, err
		}
	}

	// Determine remote.
	c.remote, _ = capture("git", "config", "--get", "prmaster.personalRemote")
	if c.remote == "" {
		hint := `set prmaster.personalRemote to the name of the Git remote to check
for personal branches. For example:

    $ git config prmaster.personalRemote benesch

If you don't use personal remotes, you can set prmaster.personalRemote to
the special value "none", then use the prmaster.branchPrefix configuration to
limit prmaster to only branches that begin with that string. For example:

    $ git config prmaster.personalRemote none
    $ git config prmaster.branchPrefix benesch/
`

		if r, _ := capture("git", "config", "--get", "cockroach.remote"); r != "" {
			hint += `
The old configuration setting, cockroach.remote, is no longer checked.
`
		}

		return c, hintedErr{
			error: errors.New("missing prmaster.personalRemote configuration"),
			hint:  hint,
		}
	}

	if c.remote == "none" {
		c.remote = upstreamRemote
	} else {
		c.personal = true
		remoteURL, err := capture("git", "remote", "get-url", "--push", c.remote)
		if err != nil {
			return c, errors.Wrapf(err, "determining URL for remote %q", c.remote)
		}
		m := regexp.MustCompile(`github.com(:|/)([[:alnum:]\-]+)`).FindStringSubmatch(remoteURL)
		if len(m) != 3 {
			return c, errors.Errorf("unable to guess GitHub username from remote %q (%s)",
				c.remote, remoteURL)
		} else if m[2] == c.upstreamUsername {
			return c, errors.Errorf("refusing to use unforked remote %q (%s)",
				c.remote, remoteURL)
		}
	}

	// Determine branch prefix, if any.
	c.branchPrefix, _ = capture("git", "config", "--get", "prmaster.branchPrefix")

	// Build GitHub client.
	var ghAuthClient *http.Client
	ghToken, _ := capture("git", "config", "--get", "prmaster.githubToken")
	if ghToken == "" {
		ghToken, _ = capture("git", "config", "--get", "cockroach.githubToken")
	}
	if ghToken != "" {
		ghAuthClient = oauth2.NewClient(ctx, oauth2.StaticTokenSource(
			&oauth2.Token{AccessToken: ghToken}))
	}
	c.ghClient = github.NewClient(ghAuthClient)

	user, _, err := c.ghClient.Users.Get(ctx, "")
	if err != nil {
		return c, errors.Wrap(err, "looking up GitHub username")
	}
	c.username = *user.Login

	// Determine Git directory.
	c.gitDir, err = capture("git", "rev-parse", "--git-dir")
	return c, errors.Wrap(err, "looking up git directory")
}

type hintedErr struct {
	hint string
	error
}
