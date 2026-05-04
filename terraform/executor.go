package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/cvhariharan/flowctl/sdk/executor"
	"github.com/go-git/go-git/v5"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"github.com/invopop/jsonschema"
	"gopkg.in/yaml.v3"
)

type TerraformWithConfig struct {
	Repo     string `yaml:"repo" json:"repo" jsonschema:"title=Repository,description=Git repository URL (HTTPS or SSH),required"`
	Username string `yaml:"username,omitempty" json:"username,omitempty" jsonschema:"title=Username,description=Username for HTTPS basic auth. Examples: 'x-access-token' for GitHub fine-grained PATs, 'oauth2' for GitLab, 'gitea' for Gitea. Ignored for SSH URLs"`
	Token    string `yaml:"token,omitempty" json:"token,omitempty" jsonschema:"title=Token,description=Deploy token or access key. Supports ${ENV_VAR} substitution. Optional for public repos"`
	Path     string `yaml:"path,omitempty" json:"path,omitempty" jsonschema:"title=Path,description=Subdirectory within the repo containing the Terraform module"`
	Command  string `yaml:"command,omitempty" json:"command,omitempty" jsonschema:"title=Command,description=Terraform command to run (default: apply -auto-approve)"`
}

type TerraformExecutor struct {
	name   string
	driver executor.NodeDriver
}

func GetSchema() interface{} {
	return jsonschema.Reflect(&TerraformWithConfig{})
}

func GetCapabilities() executor.Capability {
	return executor.RemoteExecution | executor.StreamingOutput
}

func NewTerraformExecutor(name string, node executor.Node, execID string) (executor.Executor, error) {
	driver, err := executor.NewNodeDriver(context.Background(), node)
	if err != nil {
		return nil, fmt.Errorf("failed to create node driver: %w", err)
	}

	return &TerraformExecutor{
		name:   name,
		driver: driver,
	}, nil
}

func (e *TerraformExecutor) GetArtifactsDir() string {
	return ""
}

func (e *TerraformExecutor) Close() error {
	return e.driver.Close()
}

var envVarRe = regexp.MustCompile(`\$\{([^}]+)\}`)

func substituteEnvVars(s string, inputs map[string]any) string {
	return envVarRe.ReplaceAllStringFunc(s, func(m string) string {
		key := strings.TrimSpace(envVarRe.FindStringSubmatch(m)[1])
		if v, ok := inputs[key]; ok {
			return fmt.Sprintf("%v", v)
		}
		return m
	})
}

func (e *TerraformExecutor) Execute(ctx context.Context, execCtx executor.ExecutionContext) (map[string]string, error) {
	var config TerraformWithConfig
	if err := yaml.Unmarshal(execCtx.WithConfig, &config); err != nil {
		return nil, fmt.Errorf("could not read config for terraform executor %s: %w", e.name, err)
	}

	if config.Repo == "" {
		return nil, fmt.Errorf("repo is required for terraform executor %s", e.name)
	}
	if config.Command == "" {
		config.Command = "apply -auto-approve"
	}

	token := substituteEnvVars(config.Token, execCtx.Inputs)

	// Clone the repo locally
	cloneDir, err := os.MkdirTemp("", "terraform-clone-*")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp dir for git clone: %w", err)
	}
	defer os.RemoveAll(cloneDir)

	cloneOpts := &git.CloneOptions{
		URL:   config.Repo,
		Depth: 1,
	}

	if token != "" {
		if isSSHURL(config.Repo) {
			auth, err := gitssh.NewPublicKeys("git", []byte(token), "")
			if err != nil {
				return nil, fmt.Errorf("failed to create SSH auth: %w", err)
			}
			cloneOpts.Auth = auth
		} else {
			username := substituteEnvVars(config.Username, execCtx.Inputs)
			if username == "" {
				return nil, fmt.Errorf("username is required for HTTPS auth (e.g. 'x-access-token' for GitHub, 'oauth2' for GitLab)")
			}
			cloneOpts.Auth = &githttp.BasicAuth{
				Username: username,
				Password: token,
			}
		}
	}

	fmt.Fprintf(execCtx.Stdout, "Cloning repository %s...\n", config.Repo)
	if _, err := git.PlainClone(cloneDir, false, cloneOpts); err != nil {
		return nil, fmt.Errorf("failed to clone repository: %w", err)
	}

	moduleDir := cloneDir
	if config.Path != "" {
		moduleDir = filepath.Join(cloneDir, config.Path)
		if _, err := os.Stat(moduleDir); err != nil {
			return nil, fmt.Errorf("path %q not found in repository: %w", config.Path, err)
		}
	}

	env := os.Environ()
	for k, v := range execCtx.Inputs {
		env = append(env, fmt.Sprintf("%s=%s", k, fmt.Sprint(v)))
	}

	if e.driver.IsRemote() {
		return e.executeRemote(ctx, execCtx, moduleDir, config.Command, env)
	}
	return e.executeLocal(ctx, execCtx, moduleDir, config.Command, env)
}

const stateFileName = "terraform.tfstate"

type stateKV struct {
	client *executor.APIClient
	bucket string
	key    string
}

func (e *TerraformExecutor) stateKV(execCtx executor.ExecutionContext) *stateKV {
	if execCtx.APIBaseURL == "" || execCtx.APIKey == "" {
		return nil
	}
	return &stateKV{
		client: executor.NewAPIClient(execCtx.APIBaseURL, execCtx.APIKey, execCtx.UserUUID),
		bucket: e.name,
		key:    fmt.Sprintf("%s-%s", execCtx.NamespaceName, execCtx.FlowName),
	}
}

func (e *TerraformExecutor) executeLocal(ctx context.Context, execCtx executor.ExecutionContext, moduleDir, command string, env []string) (map[string]string, error) {
	statePath := filepath.Join(moduleDir, stateFileName)
	kv := e.stateKV(execCtx)
	if kv != nil {
		if state, err := kv.client.KVGet(ctx, kv.bucket, kv.key); err == nil && state != "" {
			fmt.Fprintf(execCtx.Stdout, "Restoring terraform state from %s/%s...\n", kv.bucket, kv.key)
			if err := os.WriteFile(statePath, []byte(state), 0600); err != nil {
				return nil, fmt.Errorf("failed to write restored state file: %w", err)
			}
		} else if err != nil {
			fmt.Fprintf(execCtx.Stdout, "No prior state found (%v), starting fresh\n", err)
		}
	}

	fmt.Fprintf(execCtx.Stdout, "Running terraform init...\n")
	initCmd := fmt.Sprintf("terraform -chdir=%s init -no-color", moduleDir)
	if err := e.driver.Exec(ctx, initCmd, moduleDir, env, execCtx.Stdout, execCtx.Stderr); err != nil {
		return nil, fmt.Errorf("terraform init failed: %w", err)
	}

	fmt.Fprintf(execCtx.Stdout, "Running terraform %s...\n", command)
	tfCmd := fmt.Sprintf("terraform -chdir=%s %s -no-color", moduleDir, command)
	tfErr := e.driver.Exec(ctx, tfCmd, moduleDir, env, execCtx.Stdout, execCtx.Stderr)

	if kv != nil {
		if data, err := os.ReadFile(statePath); err == nil {
			persistState(ctx, kv, data, execCtx.Stdout, execCtx.Stderr)
		} else if !os.IsNotExist(err) {
			fmt.Fprintf(execCtx.Stderr, "warning: could not read state file for persistence: %v\n", err)
		}
	}

	if tfErr != nil {
		return nil, fmt.Errorf("terraform %s failed: %w", command, tfErr)
	}
	return map[string]string{"status": "success"}, nil
}

func persistState(ctx context.Context, kv *stateKV, data []byte, stdout, stderr io.Writer) {
	fmt.Fprintf(stdout, "Persisting terraform state to %s/%s...\n", kv.bucket, kv.key)
	if err := kv.client.KVSet(ctx, kv.bucket, kv.key, string(data)); err != nil {
		fmt.Fprintf(stderr, "warning: failed to persist terraform state: %v\n", err)
	}
}

func (e *TerraformExecutor) executeRemote(ctx context.Context, execCtx executor.ExecutionContext, moduleDir, command string, env []string) (map[string]string, error) {
	remoteDir := e.driver.Join(e.driver.TempDir(), "terraform-module")
	if err := e.driver.CreateDir(ctx, remoteDir); err != nil {
		return nil, fmt.Errorf("failed to create remote directory: %w", err)
	}
	defer e.driver.Remove(ctx, remoteDir)

	tarballPath := filepath.Join(os.TempDir(), "terraform-module.tar.gz")
	if err := compressDirectory(moduleDir, tarballPath); err != nil {
		return nil, fmt.Errorf("failed to compress module directory: %w", err)
	}
	defer os.Remove(tarballPath)

	if err := uploadAndExtract(ctx, e.driver, tarballPath, remoteDir, execCtx.Stdout); err != nil {
		return nil, err
	}

	remoteStatePath := e.driver.Join(remoteDir, stateFileName)
	kv := e.stateKV(execCtx)
	if kv != nil {
		if state, err := kv.client.KVGet(ctx, kv.bucket, kv.key); err == nil && state != "" {
			fmt.Fprintf(execCtx.Stdout, "Restoring terraform state from %s/%s...\n", kv.bucket, kv.key)
			tmpPath, err := writeTempFile("tfstate-*.json", []byte(state))
			if err != nil {
				return nil, err
			}
			defer os.Remove(tmpPath)
			if err := e.driver.Upload(ctx, tmpPath, remoteStatePath); err != nil {
				return nil, fmt.Errorf("failed to upload state file: %w", err)
			}
		} else if err != nil {
			fmt.Fprintf(execCtx.Stdout, "No prior state found (%v), starting fresh\n", err)
		}
	}

	fmt.Fprintf(execCtx.Stdout, "Running terraform init...\n")
	initCmd := fmt.Sprintf("terraform -chdir=%s init -no-color", remoteDir)
	if err := e.driver.Exec(ctx, initCmd, remoteDir, env, execCtx.Stdout, execCtx.Stderr); err != nil {
		return nil, fmt.Errorf("terraform init failed: %w", err)
	}

	fmt.Fprintf(execCtx.Stdout, "Running terraform %s...\n", command)
	tfCmd := fmt.Sprintf("terraform -chdir=%s %s -no-color", remoteDir, command)
	tfErr := e.driver.Exec(ctx, tfCmd, remoteDir, env, execCtx.Stdout, execCtx.Stderr)

	if kv != nil {
		if data, err := downloadRemoteFile(ctx, e.driver, remoteStatePath); err == nil {
			persistState(ctx, kv, data, execCtx.Stdout, execCtx.Stderr)
		} else {
			fmt.Fprintf(execCtx.Stderr, "warning: could not retrieve remote state file: %v\n", err)
		}
	}

	if tfErr != nil {
		return nil, fmt.Errorf("terraform %s failed: %w", command, tfErr)
	}
	return map[string]string{"status": "success"}, nil
}

func writeTempFile(pattern string, data []byte) (string, error) {
	f, err := os.CreateTemp("", pattern)
	if err != nil {
		return "", fmt.Errorf("failed to create temp file: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", fmt.Errorf("failed to write temp file: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", fmt.Errorf("failed to close temp file: %w", err)
	}
	return f.Name(), nil
}

func downloadRemoteFile(ctx context.Context, driver executor.NodeDriver, remotePath string) ([]byte, error) {
	f, err := os.CreateTemp("", "remote-dl-*")
	if err != nil {
		return nil, fmt.Errorf("create temp file: %w", err)
	}
	localPath := f.Name()
	f.Close()
	defer os.Remove(localPath)

	if err := driver.Download(ctx, remotePath, localPath); err != nil {
		return nil, fmt.Errorf("download: %w", err)
	}
	return os.ReadFile(localPath)
}

func isSSHURL(url string) bool {
	return strings.HasPrefix(url, "git@") || strings.HasPrefix(url, "ssh://")
}

func compressDirectory(srcDir, tarballPath string) error {
	outFile, err := os.Create(tarballPath)
	if err != nil {
		return fmt.Errorf("failed to create tarball file: %w", err)
	}
	defer outFile.Close()

	gzWriter := gzip.NewWriter(outFile)
	defer gzWriter.Close()

	tarWriter := tar.NewWriter(gzWriter)
	defer tarWriter.Close()

	return filepath.Walk(srcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if info.IsDir() && info.Name() == ".git" {
			return filepath.SkipDir
		}

		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return fmt.Errorf("failed to create tar header for %s: %w", path, err)
		}

		// Use relative path
		relPath, err := filepath.Rel(srcDir, path)
		if err != nil {
			return fmt.Errorf("failed to get relative path: %w", err)
		}
		header.Name = relPath

		if err := tarWriter.WriteHeader(header); err != nil {
			return fmt.Errorf("failed to write tar header: %w", err)
		}

		if info.IsDir() {
			return nil
		}

		file, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("failed to open file %s: %w", path, err)
		}
		defer file.Close()

		if _, err := io.Copy(tarWriter, file); err != nil {
			return fmt.Errorf("failed to write file to tar: %w", err)
		}

		return nil
	})
}

func uploadAndExtract(ctx context.Context, driver executor.NodeDriver, tarballPath, remoteDir string, stdout io.Writer) error {
	remoteTarball := driver.Join(driver.TempDir(), "terraform-module.tar.gz")

	fmt.Fprintf(stdout, "Uploading module to remote node...\n")
	if err := driver.Upload(ctx, tarballPath, remoteTarball); err != nil {
		return fmt.Errorf("failed to upload tarball: %w", err)
	}

	extractCmd := fmt.Sprintf("tar -xzf %s -C %s", remoteTarball, remoteDir)
	if err := driver.Exec(ctx, extractCmd, remoteDir, nil, stdout, stdout); err != nil {
		return fmt.Errorf("failed to extract tarball on remote: %w", err)
	}

	if err := driver.Remove(ctx, remoteTarball); err != nil {
		return fmt.Errorf("failed to remove remote tarball: %w", err)
	}

	return nil
}

// TerraformExecutorPlugin implements executor.ExecutorPlugin for the Terraform executor.
type TerraformExecutorPlugin struct{}

func (p *TerraformExecutorPlugin) GetName() string {
	return "terraform"
}

func (p *TerraformExecutorPlugin) GetSchema() interface{} {
	return GetSchema()
}

func (p *TerraformExecutorPlugin) GetCapabilities() executor.Capability {
	return GetCapabilities()
}

func (p *TerraformExecutorPlugin) New(name string, node executor.Node, execID string) (executor.Executor, error) {
	return NewTerraformExecutor(name, node, execID)
}
