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
	Repo    string `yaml:"repo" json:"repo" jsonschema:"title=Repository,description=Git repository URL (HTTPS or SSH),required"`
	Token   string `yaml:"token,omitempty" json:"token,omitempty" jsonschema:"title=Token,description=Deploy token or access key. Supports ${ENV_VAR} substitution. Optional for public repos"`
	Path    string `yaml:"path,omitempty" json:"path,omitempty" jsonschema:"title=Path,description=Subdirectory within the repo containing the Terraform module"`
	Command string `yaml:"command,omitempty" json:"command,omitempty" jsonschema:"title=Command,description=Terraform command to run (default: apply -auto-approve)"`
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
			// HTTPS with token — supports "username:password" or bare token
			username, password := "", token
			if i := strings.Index(token, ":"); i >= 0 {
				username = token[:i]
				password = token[i+1:]
			}
			cloneOpts.Auth = &githttp.BasicAuth{
				Username: username,
				Password: password,
			}
		}
	}

	fmt.Fprintf(execCtx.Stdout, "Cloning repository %s...\n", config.Repo)
	if _, err := git.PlainClone(cloneDir, false, cloneOpts); err != nil {
		return nil, fmt.Errorf("failed to clone repository: %w", err)
	}

	// Determine the module directory
	moduleDir := cloneDir
	if config.Path != "" {
		moduleDir = filepath.Join(cloneDir, config.Path)
		if _, err := os.Stat(moduleDir); err != nil {
			return nil, fmt.Errorf("path %q not found in repository: %w", config.Path, err)
		}
	}

	// Prepare env vars from inputs
	var env []string
	for k, v := range execCtx.Inputs {
		env = append(env, fmt.Sprintf("%s=%s", k, fmt.Sprint(v)))
	}

	if e.driver.IsRemote() {
		return e.executeRemote(ctx, execCtx, moduleDir, config.Command, env)
	}
	return e.executeLocal(ctx, execCtx, moduleDir, config.Command, env)
}

func (e *TerraformExecutor) executeLocal(ctx context.Context, execCtx executor.ExecutionContext, moduleDir, command string, env []string) (map[string]string, error) {
	fmt.Fprintf(execCtx.Stdout, "Running terraform init...\n")
	initCmd := fmt.Sprintf("terraform -chdir=%s init -no-color", moduleDir)
	if err := e.driver.Exec(ctx, initCmd, moduleDir, env, execCtx.Stdout, execCtx.Stderr); err != nil {
		return nil, fmt.Errorf("terraform init failed: %w", err)
	}

	fmt.Fprintf(execCtx.Stdout, "Running terraform %s...\n", command)
	tfCmd := fmt.Sprintf("terraform -chdir=%s %s -no-color", moduleDir, command)
	if err := e.driver.Exec(ctx, tfCmd, moduleDir, env, execCtx.Stdout, execCtx.Stderr); err != nil {
		return nil, fmt.Errorf("terraform %s failed: %w", command, err)
	}

	return map[string]string{"status": "success"}, nil
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

	fmt.Fprintf(execCtx.Stdout, "Running terraform init...\n")
	initCmd := fmt.Sprintf("terraform -chdir=%s init -no-color", remoteDir)
	if err := e.driver.Exec(ctx, initCmd, remoteDir, env, execCtx.Stdout, execCtx.Stderr); err != nil {
		return nil, fmt.Errorf("terraform init failed: %w", err)
	}

	fmt.Fprintf(execCtx.Stdout, "Running terraform %s...\n", command)
	tfCmd := fmt.Sprintf("terraform -chdir=%s %s -no-color", remoteDir, command)
	if err := e.driver.Exec(ctx, tfCmd, remoteDir, env, execCtx.Stdout, execCtx.Stderr); err != nil {
		return nil, fmt.Errorf("terraform %s failed: %w", command, err)
	}

	return map[string]string{"status": "success"}, nil
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
