package executorutil

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/cvhariharan/flowctl/sdk/executor"
	"github.com/go-git/go-git/v5"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
)

// IsSSHURL reports whether url uses SSH transport.
func IsSSHURL(url string) bool {
	return strings.HasPrefix(url, "git@") || strings.HasPrefix(url, "ssh://")
}

// CloneOptions configures a git clone via Clone.
type CloneOptions struct {
	URL      string
	Username string    // HTTPS basic-auth username; ignored for SSH
	Token    string    // password (HTTPS) or PEM-encoded private key (SSH); empty for no auth
	Stdout   io.Writer // optional progress writer
}

// Clone shallow-clones opts.URL into dst. If driver is non-nil and remote, the
// clone runs on the remote node (requires `git` on the remote). Otherwise the
// clone happens locally via go-git.
func Clone(ctx context.Context, driver executor.NodeDriver, dst string, opts CloneOptions) error {
	if driver != nil && driver.IsRemote() {
		return cloneRemote(ctx, driver, dst, opts)
	}
	return cloneLocal(dst, opts)
}

func cloneLocal(dst string, opts CloneOptions) error {
	cloneOpts := &git.CloneOptions{
		URL:   opts.URL,
		Depth: 1,
	}

	if opts.Token != "" {
		if IsSSHURL(opts.URL) {
			auth, err := gitssh.NewPublicKeys("git", []byte(opts.Token), "")
			if err != nil {
				return fmt.Errorf("failed to create SSH auth: %w", err)
			}
			cloneOpts.Auth = auth
		} else {
			if opts.Username == "" {
				return fmt.Errorf("username is required for HTTPS auth (e.g. 'x-access-token' for GitHub, 'oauth2' for GitLab)")
			}
			cloneOpts.Auth = &githttp.BasicAuth{
				Username: opts.Username,
				Password: opts.Token,
			}
		}
	}

	if opts.Stdout != nil {
		fmt.Fprintf(opts.Stdout, "Cloning repository %s...\n", opts.URL)
	}
	if _, err := git.PlainClone(dst, false, cloneOpts); err != nil {
		return fmt.Errorf("failed to clone repository: %w", err)
	}
	return nil
}

func cloneRemote(ctx context.Context, driver executor.NodeDriver, dst string, opts CloneOptions) error {
	stdout := opts.Stdout
	if stdout == nil {
		stdout = io.Discard
	}
	stderr := stdout

	cloneURL := opts.URL
	var env []string
	var cleanup func()

	if opts.Token != "" {
		if IsSSHURL(opts.URL) {
			keyPath, c, err := uploadSSHKey(ctx, driver, opts.Token)
			if err != nil {
				return err
			}
			cleanup = c
			env = append(env, fmt.Sprintf(
				`GIT_SSH_COMMAND=ssh -i %s -o StrictHostKeyChecking=no -o IdentitiesOnly=yes -o UserKnownHostsFile=/dev/null`,
				keyPath,
			))
		} else {
			if opts.Username == "" {
				return fmt.Errorf("username is required for HTTPS auth (e.g. 'x-access-token' for GitHub, 'oauth2' for GitLab)")
			}
			cloneURL = injectBasicAuth(opts.URL, opts.Username, opts.Token)
		}
	}
	if cleanup != nil {
		defer cleanup()
	}

	fmt.Fprintf(stdout, "Cloning repository %s on remote node...\n", opts.URL)

	// `git clone` requires the destination to not exist (or be empty). Caller is
	// expected to provide a fresh path; we use --depth=1 for a shallow clone.
	cmd := fmt.Sprintf("git clone --depth=1 %s %s", shellQuote(cloneURL), shellQuote(dst))
	if err := driver.Exec(ctx, cmd, driver.TempDir(), env, stdout, stderr); err != nil {
		return fmt.Errorf("failed to clone repository on remote: %w", err)
	}
	return nil
}

func uploadSSHKey(ctx context.Context, driver executor.NodeDriver, key string) (remotePath string, cleanup func(), err error) {
	f, err := os.CreateTemp("", "git-ssh-key-*")
	if err != nil {
		return "", nil, fmt.Errorf("failed to create temp ssh key file: %w", err)
	}
	localPath := f.Name()
	if _, err := f.WriteString(key); err != nil {
		f.Close()
		os.Remove(localPath)
		return "", nil, fmt.Errorf("failed to write temp ssh key: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(localPath)
		return "", nil, fmt.Errorf("failed to close temp ssh key: %w", err)
	}
	if err := os.Chmod(localPath, 0600); err != nil {
		os.Remove(localPath)
		return "", nil, fmt.Errorf("failed to chmod temp ssh key: %w", err)
	}

	remotePath = driver.Join(driver.TempDir(), "git-ssh-key")
	if err := driver.Upload(ctx, localPath, remotePath); err != nil {
		os.Remove(localPath)
		return "", nil, fmt.Errorf("failed to upload ssh key to remote: %w", err)
	}
	if err := driver.SetPermissions(ctx, remotePath, 0600); err != nil {
		os.Remove(localPath)
		_ = driver.Remove(ctx, remotePath)
		return "", nil, fmt.Errorf("failed to set ssh key permissions on remote: %w", err)
	}

	cleanup = func() {
		os.Remove(localPath)
		_ = driver.Remove(ctx, remotePath)
	}
	return remotePath, cleanup, nil
}

// injectBasicAuth rewrites an HTTPS URL to include user:password credentials.
// Note: the URL becomes visible in the remote process list; callers should
// treat the secret as compromised after use and prefer ephemeral tokens.
func injectBasicAuth(rawURL, username, token string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	u.User = url.UserPassword(username, token)
	return u.String()
}

// shellQuote single-quotes s for safe POSIX-shell use, escaping embedded quotes.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
