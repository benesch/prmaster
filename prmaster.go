package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"golang.org/x/oauth2"

	"github.com/google/go-github/github"
	color "github.com/logrusorgru/aurora"
	"github.com/pkg/errors"
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
	opt := &github.ListOptions{PerPage: 100}
	branches, _, err := c.ghClient.Repositories.ListBranches(ctx, c.username, "cockroach", opt)
	if err != nil {
		return err
	}
	remoteBranches := map[string]struct{}{}
	var remoteExtras []string
	for _, branch := range branches {
		colorName := color.Cyan(branch.GetName())
		if isReleaseBranch(branch.GetName()) {
			fmt.Printf("Skipping %s. Looks like a release branch.\n", colorName)
			continue
		}
		remoteBranches[branch.GetName()] = struct{}{}
		prOpts := &github.PullRequestListOptions{
			State: "all",
			Head:  fmt.Sprintf("%s:%s", c.username, branch.GetName()),
		}
		prs, _, err := c.ghClient.PullRequests.List(ctx, "cockroachdb", "cockroach", prOpts)
		if err != nil {
			return err
		}
		if len(prs) == 0 {
			fmt.Printf("Skipping %s. Not associated with any PRs.\n", colorName)
			remoteExtras = append(remoteExtras, branch.GetName())
			continue
		}
		// PRs are sorted so that the most recent PR is first.
		pr := prs[0]
		if pr.Head.GetSHA() != branch.Commit.GetSHA() {
			fmt.Printf("Skipping %s. SHA does not match candidate PR #%d.\n",
				colorName, pr.GetNumber())
			remoteExtras = append(remoteExtras, branch.GetName())
			continue
		}
		if pr.GetState() == "closed" {
			if err := deleteBranch(c, branch.GetName()); err != nil {
				fmt.Printf("Unable to delete %s. (PR #%d is closed.)\nError: %s\n",
					colorName, pr.GetNumber(), err)
			} else {
				fmt.Printf("Deleted %s. PR #%d is closed.\n", colorName, pr.GetNumber())
			}
			continue
		}
		fmt.Printf("Skipping %s. PR #%d is still open.\n", colorName, pr.GetNumber())
	}
	if len(remoteExtras) > 0 {
		fmt.Println()
		fmt.Println("These remote branches do not have open PRs:")
		for _, b := range remoteExtras {
			fmt.Printf("    %s\n", b)
		}
		fmt.Println()
		fmt.Printf("    Manage: https://github.com/%s/cockroach/branches/yours\n", c.username)
	}
	var localBranches, extras []string
	{
		out, err := capture("git", "for-each-ref", "--format", "%(refname:short)", "refs/heads")
		if err != nil {
			return err
		}
		localBranches = strings.Fields(out)
	}
	for _, b := range localBranches {
		if isReleaseBranch(b) {
			continue
		}
		if _, ok := remoteBranches[b]; !ok {
			extras = append(extras, b)
		}
	}
	if len(extras) > 0 {
		fmt.Println()
		fmt.Println("These local branches do not exist on your remote:")
		for _, b := range extras {
			fmt.Printf("    %s\n", b)
		}
	}
	fmt.Println()
	fmt.Printf("Running `git remote prune %s`.\n", c.remote)
	return spawn("git", "remote", "prune", c.remote)
}

func isReleaseBranch(s string) bool {
	return s == "master" || strings.HasPrefix(s, "release-")
}

func deleteBranch(c config, s string) error {
	if err := spawn("git", "push", "-d", c.remote, s); err != nil {
		return err
	}
	if err := spawn("git", "show-ref", "--verify", "--quiet", "refs/heads/"+s); err == nil {
		return spawn("git", "branch", "-D", s)
	}
	return nil
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
