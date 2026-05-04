package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/cvhariharan/flowctl-executors/pkg/executorutil"
	"github.com/cvhariharan/flowctl/sdk/executor"
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

	cloneOpts := executorutil.CloneOptions{
		URL:      config.Repo,
		Username: executorutil.SubstituteEnvVars(config.Username, execCtx.Inputs),
		Token:    executorutil.SubstituteEnvVars(config.Token, execCtx.Inputs),
		Stdout:   execCtx.Stdout,
	}

	env := os.Environ()
	for k, v := range execCtx.Inputs {
		env = append(env, fmt.Sprintf("%s=%s", k, fmt.Sprint(v)))
	}

	if e.driver.IsRemote() {
		return e.executeRemote(ctx, execCtx, cloneOpts, config, env)
	}
	return e.executeLocal(ctx, execCtx, cloneOpts, config, env)
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

func (e *TerraformExecutor) executeLocal(ctx context.Context, execCtx executor.ExecutionContext, cloneOpts executorutil.CloneOptions, config TerraformWithConfig, env []string) (map[string]string, error) {
	cloneDir, err := os.MkdirTemp("", "terraform-clone-*")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp dir for git clone: %w", err)
	}
	defer os.RemoveAll(cloneDir)

	if err := executorutil.Clone(ctx, nil, cloneDir, cloneOpts); err != nil {
		return nil, err
	}

	moduleDir := cloneDir
	if config.Path != "" {
		moduleDir = filepath.Join(cloneDir, config.Path)
		if _, err := os.Stat(moduleDir); err != nil {
			return nil, fmt.Errorf("path %q not found in repository: %w", config.Path, err)
		}
	}

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

	fmt.Fprintf(execCtx.Stdout, "Running terraform %s...\n", config.Command)
	tfCmd := fmt.Sprintf("terraform -chdir=%s %s -no-color", moduleDir, config.Command)
	tfErr := e.driver.Exec(ctx, tfCmd, moduleDir, env, execCtx.Stdout, execCtx.Stderr)

	if kv != nil {
		if data, err := os.ReadFile(statePath); err == nil {
			persistState(ctx, kv, data, execCtx.Stdout, execCtx.Stderr)
		} else if !os.IsNotExist(err) {
			fmt.Fprintf(execCtx.Stderr, "warning: could not read state file for persistence: %v\n", err)
		}
	}

	if tfErr != nil {
		return nil, fmt.Errorf("terraform %s failed: %w", config.Command, tfErr)
	}
	return map[string]string{"status": "success"}, nil
}

func persistState(ctx context.Context, kv *stateKV, data []byte, stdout, stderr io.Writer) {
	fmt.Fprintf(stdout, "Persisting terraform state to %s/%s...\n", kv.bucket, kv.key)
	if err := kv.client.KVSet(ctx, kv.bucket, kv.key, string(data)); err != nil {
		fmt.Fprintf(stderr, "warning: failed to persist terraform state: %v\n", err)
	}
}

func (e *TerraformExecutor) executeRemote(ctx context.Context, execCtx executor.ExecutionContext, cloneOpts executorutil.CloneOptions, config TerraformWithConfig, env []string) (map[string]string, error) {
	cloneDir := e.driver.Join(e.driver.TempDir(), fmt.Sprintf("terraform-clone-%s", execCtx.ExecID))
	// `git clone` requires the destination not to exist; clean up any prior leftover.
	_ = e.driver.Remove(ctx, cloneDir)
	defer e.driver.Remove(ctx, cloneDir)

	if err := executorutil.Clone(ctx, e.driver, cloneDir, cloneOpts); err != nil {
		return nil, err
	}

	moduleDir := cloneDir
	if config.Path != "" {
		moduleDir = e.driver.Join(cloneDir, config.Path)
	}

	remoteStatePath := e.driver.Join(moduleDir, stateFileName)
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
	initCmd := fmt.Sprintf("terraform -chdir=%s init -no-color", moduleDir)
	if err := e.driver.Exec(ctx, initCmd, moduleDir, env, execCtx.Stdout, execCtx.Stderr); err != nil {
		return nil, fmt.Errorf("terraform init failed: %w", err)
	}

	fmt.Fprintf(execCtx.Stdout, "Running terraform %s...\n", config.Command)
	tfCmd := fmt.Sprintf("terraform -chdir=%s %s -no-color", moduleDir, config.Command)
	tfErr := e.driver.Exec(ctx, tfCmd, moduleDir, env, execCtx.Stdout, execCtx.Stderr)

	if kv != nil {
		if data, err := downloadRemoteFile(ctx, e.driver, remoteStatePath); err == nil {
			persistState(ctx, kv, data, execCtx.Stdout, execCtx.Stderr)
		} else {
			fmt.Fprintf(execCtx.Stderr, "warning: could not retrieve remote state file: %v\n", err)
		}
	}

	if tfErr != nil {
		return nil, fmt.Errorf("terraform %s failed: %w", config.Command, tfErr)
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
