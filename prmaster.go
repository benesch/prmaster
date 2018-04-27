package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/vbauerster/mpb/decor"

	"golang.org/x/oauth2"

	"github.com/google/go-github/github"
	color "github.com/logrusorgru/aurora"
	"github.com/pkg/errors"
	"github.com/vbauerster/mpb"
)

const usage = `usage: prmaster <list|sync>`

func main() {
	if err := run(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %s\n", err)

		cause := errors.Cause(err)
		if _, ok := cause.(*github.RateLimitError); ok {
			fmt.Fprintln(os.Stderr, `hint: unauthenticated GitHub requests are subject to a very strict rate
limit. Please configure backport with a personal access token:
			$ git config cockroach.githubToken TOKEN
For help creating a personal access token, see https://goo.gl/Ep2E6x.`)
		} else if err, ok := cause.(hintedErr); ok {
			fmt.Fprintf(os.Stderr, "hint: %s\n", err.hint)
		}

		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	c, err := loadConfig(ctx)
	if err != nil {
		return err
	}

	if len(os.Args) != 2 {
		return errors.New(usage)
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
	opts := &github.SearchOptions{
		Sort: "created",
	}
	query := fmt.Sprintf("type:pr is:open repo:cockroachdb/cockroach author:%s", c.username)
	res, _, err := c.ghClient.Search.Issues(ctx, query, opts)
	if err != nil {
		return err
	}
	for _, issue := range res.Issues {
		pr, _, err := c.ghClient.PullRequests.Get(ctx, "cockroachdb", "cockroach", *issue.Number)
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
			"%s\n    Branch %s. Opened %s.\n    https://github.com/cockroachdb/cockroach/pull/%d\n",
			color.Bold(*pr.Title), color.Cyan(*pr.Head.Ref),
			dateColor(pr.CreatedAt.Format("2006-01-02")), *pr.Number)
	}
	return nil
}

func runSync(ctx context.Context, c config) error {
	branches, err := loadBranches(ctx, c)
	if err != nil {
		return err
	}
	for i := range branches {
		b := &branches[i]
		colorName := color.Bold(b.name)
		if b.isRelease() {
			fmt.Printf("Skipping %s. Looks like a release branch.\n", colorName)
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
				if err := spawn("git", "push", "-qd", c.remote, b.name); err != nil {
					return err
				}
				fmt.Printf("%s remote %s. PR #%d is closed.\n", color.Brown("Deleted"),
					colorName, b.pr.GetNumber())
				b.remote = nil
			} else {
				fmt.Printf("Skipping remote %s. Branch commit is newer than #%d.\n",
					colorName, b.pr.GetNumber())
			}
		}
		if b.local != nil {
			if b.local.sha == b.pr.sha || b.local.commitDate.Before(b.pr.commitDate) {
				if err := spawn("git", "branch", "-qD", b.name); err != nil {
					return err
				}
				fmt.Printf("%s local %s. PR #%d is closed.\n", color.Brown("Deleted"),
					colorName, b.pr.GetNumber())
				b.local = nil
			} else {
				fmt.Printf("Skipping local %s. Branch commit is newer than #%d.\n",
					colorName, b.pr.GetNumber())
			}
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
		fmt.Printf("    Manage: https://github.com/%s/cockroach/branches/yours\n", c.username)
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
	fmt.Printf("Running `git remote prune %s`.\n", c.remote)
	return spawn("git", "remote", "prune", c.remote)
}

type commit struct {
	sha        string
	commitDate time.Time
}

func newCommit(repoCommit *github.RepositoryCommit) *commit {
	ghCommit := repoCommit.GetCommit()
	return &commit{
		sha:        ghCommit.GetSHA(),
		commitDate: ghCommit.GetCommitter().GetDate(),
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

	// Collect remote branches.
	ghBranches, res, err := c.ghClient.Repositories.ListBranches(
		ctx, c.username, "cockroach", &github.ListOptions{PerPage: 100})
	if err != nil {
		return nil, err
	} else if res.NextPage != 0 {
		fmt.Fprintln(os.Stderr, "warning: more than 100 remote branches; some will be omitted")
	}
	for _, b := range ghBranches {
		branches = append(branches, branch{
			name:   b.GetName(),
			remote: newCommit(b.GetCommit()),
		})
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
		for _, b := range branches {
			if b.name == name {
				b.local = commit
				continue outer
			}
		}
		branches = append(branches, branch{name: name, local: commit})
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
	for i := range branches {
		prOpts := &github.PullRequestListOptions{
			State: "all",
			Head:  fmt.Sprintf("%s:%s", c.username, branches[i].name),
		}
		prs, _, err := c.ghClient.PullRequests.List(ctx, "cockroachdb", "cockroach", prOpts)
		if err != nil {
			return nil, err
		}
		if len(prs) != 0 {
			// PRs are sorted so that the most recent PR is first.
			pr := prs[0]
			commits, _, err := c.ghClient.PullRequests.ListCommits(ctx, "cockroachdb", "cockroach",
				pr.GetNumber(), nil /* listOptions */)
			if err != nil {
				return nil, err
			}
			if len(commits) == 0 {
				// TODO: Is this an error?
				continue
			}
			branches[i].pr.PullRequest = pr
			branches[i].pr.commit = newCommit(commits[len(commits)-1])
		}
		bar.Increment()
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
	ghClient *github.Client
	remote   string
	username string
	gitDir   string
}

func loadConfig(ctx context.Context) (config, error) {
	var c config

	// Determine remote.
	c.remote, _ = capture("git", "config", "--get", "cockroach.remote")
	if c.remote == "" {
		return c, hintedErr{
			error: errors.New("missing cockroach.remote configuration"),
			hint: `set cockroach.remote to the name of the Git remote to check
for personal branches. For example:

    $ git config cockroach.remote origin
`,
		}
	}

	// Determine username.
	remoteURL, err := capture("git", "remote", "get-url", "--push", c.remote)
	if err != nil {
		return c, errors.Wrapf(err, "determining URL for remote %q", c.remote)
	}
	m := regexp.MustCompile(`github.com(:|/)([[:alnum:]\-]+)`).FindStringSubmatch(remoteURL)
	if len(m) != 3 {
		return c, errors.Errorf("unable to guess GitHub username from remote %q (%s)",
			c.remote, remoteURL)
	} else if m[2] == "cockroachdb" {
		return c, errors.Errorf("refusing to use unforked remote %q (%s)",
			c.remote, remoteURL)
	}
	c.username = m[2]

	// Build GitHub client.
	var ghAuthClient *http.Client
	ghToken, _ := capture("git", "config", "--get", "cockroach.githubToken")
	if ghToken != "" {
		ghAuthClient = oauth2.NewClient(ctx, oauth2.StaticTokenSource(
			&oauth2.Token{AccessToken: ghToken}))
	}
	c.ghClient = github.NewClient(ghAuthClient)

	// Determine Git directory.
	c.gitDir, err = capture("git", "rev-parse", "--git-dir")
	return c, errors.Wrap(err, "looking up git directory")
}

type hintedErr struct {
	hint string
	error
}
