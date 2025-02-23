package git

import (
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/kris-nova/logger"
	"github.com/pkg/errors"
	giturls "github.com/whilp/git-urls"

	"github.com/weaveworks/eksctl/pkg/git/executor"
)

// TmpCloner can clone git repositories in temporary directories
type TmpCloner interface {
	CloneRepoInTmpDir(cloneDirPrefix string, options CloneOptions) (string, error)
}

// Client can perform git operations on the given directory
type Client struct {
	executor executor.Executor
	dir      string
}

// ClientParams groups the arguments to provide to create a new Git client.
type ClientParams struct {
	PrivateSSHKeyPath string
}

// Options holds options for cloning a git repository
type Options struct {
	URL    string
	Branch string
	User   string
	Email  string
}

// ValidateURL validates the URL field of this Options object, returning an
// error should the current value not be valid.
func (o Options) ValidateURL() error {
	if o.URL == "" {
		return errors.New("empty Git URL")
	}
	if !IsGitURL(o.URL) {
		return errors.New("invalid Git URL")
	}
	if !o.isSSHURL() {
		return errors.New("got a HTTP(S) Git URL, but eksctl currently only supports SSH Git URLs")
	}
	return nil
}

func (o Options) isSSHURL() bool {
	url, err := giturls.Parse(o.URL)
	return err == nil && (url.Scheme == "git" || url.Scheme == "ssh")
}

// NewGitClient returns a client that can perform git operations
func NewGitClient(params ClientParams) *Client {
	return &Client{
		executor: executor.NewShellExecutor(envVars(params)),
	}
}

func envVars(params ClientParams) []string {
	envVars := []string{}
	if params.PrivateSSHKeyPath != "" {
		envVars = append(envVars, fmt.Sprintf("GIT_SSH_COMMAND=ssh -i %s", params.PrivateSSHKeyPath))
	}
	return envVars
}

// NewGitClientFromExecutor returns a client that can have an executor injected. Useful for testing
func NewGitClientFromExecutor(executor executor.Executor) *Client {
	return &Client{
		executor: executor,
	}
}

// CloneOptions are the options for cloning a Git repository
type CloneOptions struct {
	URL       string
	Branch    string
	Bootstrap bool // create the branch if the repository is empty
}

// CloneRepoInTmpDir clones a repo specified in the gitURL in a temporary directory and checks out the specified branch
func (git *Client) CloneRepoInTmpDir(tmpDirPrefix string, options CloneOptions) (string, error) {
	cloneDir, err := ioutil.TempDir(os.TempDir(), tmpDirPrefix)
	if err != nil {
		return "", fmt.Errorf("cannot create temporary directory: %s", err)
	}
	return cloneDir, git.cloneRepoInPath(cloneDir, options)
}

// CloneRepoInPath behaves like CloneRepoInTmpDir but clones the repository in a specific directory
// which creates if needed
func (git *Client) CloneRepoInPath(clonePath string, options CloneOptions) error {
	if err := os.MkdirAll(clonePath, 0700); err != nil {
		return errors.Wrapf(err, "unable to create directory for cloning")
	}
	return git.cloneRepoInPath(clonePath, options)
}

func (git *Client) cloneRepoInPath(clonePath string, options CloneOptions) error {
	args := []string{"clone", options.URL, clonePath}
	if err := git.runGitCmd(args...); err != nil {
		return err
	}
	// Set the working directory to the cloned directory, but
	// only do it after the clone so that it doesn't create an
	// undesirable nested directory
	git.dir = clonePath

	if options.Branch != "" {
		// Switch to target branch
		args := []string{"checkout", options.Branch}
		if options.Bootstrap {
			empty, err := git.isRepoEmpty()
			if err != nil {
				return err
			}
			if empty {
				args = []string{"checkout", "-b", options.Branch}
			}
		}
		if err := git.runGitCmd(args...); err != nil {
			return err
		}
	}

	return nil
}

func (git *Client) isRepoEmpty() (bool, error) {
	// A repository is empty if it doesn't have branches
	files, err := ioutil.ReadDir(filepath.Join(git.dir, ".git", "refs", "heads"))
	if err != nil {
		return false, err
	}
	return len(files) == 0, nil
}

// Add performs can perform a `git add` operation on the given file paths
func (git Client) Add(files ...string) error {
	args := append([]string{"add", "--"}, files...)
	if err := git.runGitCmd(args...); err != nil {
		return err
	}
	return nil
}

// Commit makes a commit if there are staged changes
func (git Client) Commit(message, user, email string) error {
	// Note, this used to do runGitCmd(diffCtx, git.dir, "diff", "--cached", "--quiet", "--", fi.opts.gitFluxPath); err == nil {
	if err := git.runGitCmd("diff", "--cached", "--quiet"); err == nil {
		logger.Info("Nothing to commit (the repository contained identical files), moving on")
		return nil
	} else if _, ok := err.(*exec.ExitError); !ok {
		return err
	}

	// If the username and email have been provided, configure and use these as
	// otherwise, git will rely on the global configuration, which may lead to
	// confusion at best, as a different username/email will be used, or if
	// missing (e.g.: in CI, in a blank environment), will fail with:
	//   *** Please tell me who you are.
	//   [...]
	//   fatal: unable to auto-detect email address (got '[...]')
	// N.B.: we do it before committing, instead of after cloning, as other
	// operations will not fail because of missing configuration, and as we may
	// commit on a repository we haven't cloned ourselves.
	if email != "" {
		if err := git.runGitCmd("config", "user.email", email); err != nil {
			return err
		}
	}
	if user != "" {
		if err := git.runGitCmd("config", "user.name", user); err != nil {
			return err
		}
	}

	// Commit
	args := []string{"commit",
		"-m", message,
		fmt.Sprintf("--author=%s <%s>", user, email),
	}
	if err := git.runGitCmd(args...); err != nil {
		return err
	}
	return nil
}

// Push pushes the changes to the origin remote
func (git Client) Push() error {
	err := git.runGitCmd("push")
	return err
}

// DeleteLocalRepo deletes the local copy of a repository, including the directory
func (git Client) DeleteLocalRepo() error {
	if git.dir != "" {
		return os.RemoveAll(git.dir)
	}
	return fmt.Errorf("no cloned directory to delete")
}

func (git Client) runGitCmd(args ...string) error {
	logger.Debug(fmt.Sprintf("running git %v in %s", args, git.dir))
	return git.executor.Exec("git", git.dir, args...)
}

// RepoName returns the name of the repository given its URL
func RepoName(repoURL string) (string, error) {
	u, err := giturls.Parse(repoURL)
	if err != nil {
		return "", errors.Wrapf(err, "unable to parse git URL '%s'", repoURL)
	}
	parts := strings.Split(u.Path, "/")
	if len(parts) == 0 {
		return "", fmt.Errorf("could not find name of repository %s", repoURL)
	}

	lastPathSegment := parts[len(parts)-1]
	return strings.TrimRight(lastPathSegment, ".git"), nil
}

// IsGitURL returns true if the argument matches the git url format
func IsGitURL(rawURL string) bool {
	parsedURL, err := giturls.Parse(rawURL)
	if err == nil && parsedURL.IsAbs() && parsedURL.Hostname() != "" {
		return true
	}
	return false
}
