package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/apenella/go-ansible/v2/pkg/execute"
	"github.com/apenella/go-ansible/v2/pkg/playbook"
	"github.com/cvhariharan/flowctl-executors/pkg/executorutil"
	"github.com/cvhariharan/flowctl/sdk/executor"
	"github.com/invopop/jsonschema"
	"gopkg.in/yaml.v3"
)

type AnsibleWithConfig struct {
	Repo      string            `yaml:"repo" json:"repo" jsonschema:"title=Repository,description=Git repository URL containing the Ansible playbook (HTTPS or SSH),required"`
	Username  string            `yaml:"username,omitempty" json:"username,omitempty" jsonschema:"title=Username,description=Username for HTTPS basic auth. Examples: 'x-access-token' for GitHub fine-grained PATs, 'oauth2' for GitLab, 'gitea' for Gitea. Ignored for SSH URLs"`
	Token     string            `yaml:"token,omitempty" json:"token,omitempty" jsonschema:"title=Token,description=Deploy token or SSH key. Supports ${ENV_VAR} substitution. Optional for public repos"`
	Path      string            `yaml:"path,omitempty" json:"path,omitempty" jsonschema:"title=Path,description=Subdirectory within the repo containing the playbook"`
	Playbook  string            `yaml:"playbook" json:"playbook" jsonschema:"title=Playbook,description=Playbook filename relative to Path (e.g. site.yml),required" jsonschema_extras:"placeholder=site.yml"`
	ExtraVars map[string]string `yaml:"extra_vars,omitempty" json:"extra_vars,omitempty" jsonschema:"title=Extra Variables,description=Variables passed to the playbook via --extra-vars. Values support ${VAR} substitution" jsonschema_extras:"widget=keyvalue"`
	Tags      string            `yaml:"tags,omitempty" json:"tags,omitempty" jsonschema:"title=Tags,description=Comma-separated tags to run"`
	SkipTags  string            `yaml:"skip_tags,omitempty" json:"skip_tags,omitempty" jsonschema:"title=Skip Tags,description=Comma-separated tags to skip"`
	Verbose   bool              `yaml:"verbose,omitempty" json:"verbose,omitempty" jsonschema:"title=Verbose,description=Run ansible-playbook with -vvvv" jsonschema_extras:"type=checkbox"`
}

type AnsibleExecutor struct {
	name string
}

func GetSchema() interface{} {
	return jsonschema.Reflect(&AnsibleWithConfig{})
}

func GetCapabilities() executor.Capability {
	return executor.RemoteExecution | executor.NodeDispatch | executor.StreamingOutput
}

func NewAnsibleExecutor(name string, node executor.Node, execID string) (executor.Executor, error) {
	return &AnsibleExecutor{name: name}, nil
}

func (e *AnsibleExecutor) GetArtifactsDir() string {
	return ""
}

func (e *AnsibleExecutor) Close() error {
	return nil
}

func (e *AnsibleExecutor) Execute(ctx context.Context, execCtx executor.ExecutionContext) (map[string]string, error) {
	var config AnsibleWithConfig
	if err := yaml.Unmarshal(execCtx.WithConfig, &config); err != nil {
		return nil, fmt.Errorf("could not read config for ansible executor %s: %w", e.name, err)
	}

	if config.Repo == "" {
		return nil, fmt.Errorf("repo is required for ansible executor %s", e.name)
	}
	if config.Playbook == "" {
		return nil, fmt.Errorf("playbook is required for ansible executor %s", e.name)
	}
	if len(execCtx.Nodes) == 0 {
		return nil, fmt.Errorf("ansible executor %s requires at least one target node", e.name)
	}

	cloneDir, err := os.MkdirTemp("", "ansible-clone-*")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp dir for git clone: %w", err)
	}
	defer os.RemoveAll(cloneDir)

	if err := executorutil.Clone(ctx, nil, cloneDir, executorutil.CloneOptions{
		URL:      config.Repo,
		Username: executorutil.SubstituteEnvVars(config.Username, execCtx.Inputs),
		Token:    executorutil.SubstituteEnvVars(config.Token, execCtx.Inputs),
		Stdout:   execCtx.Stdout,
	}); err != nil {
		return nil, err
	}

	playbookDir := cloneDir
	if config.Path != "" {
		playbookDir = filepath.Join(cloneDir, config.Path)
		if _, err := os.Stat(playbookDir); err != nil {
			return nil, fmt.Errorf("path %q not found in repository: %w", config.Path, err)
		}
	}

	playbookPath := filepath.Join(playbookDir, config.Playbook)
	if _, err := os.Stat(playbookPath); err != nil {
		return nil, fmt.Errorf("playbook %q not found: %w", config.Playbook, err)
	}

	invDir, err := os.MkdirTemp("", "ansible-inv-*")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp dir for inventory: %w", err)
	}
	defer os.RemoveAll(invDir)

	invPath, err := writeInventory(invDir, execCtx.Nodes)
	if err != nil {
		return nil, err
	}

	extraVars := make(map[string]any, len(config.ExtraVars))
	for k, v := range config.ExtraVars {
		extraVars[k] = executorutil.SubstituteEnvVars(v, execCtx.Inputs)
	}

	playbookOpts := &playbook.AnsiblePlaybookOptions{
		Inventory: invPath,
		ExtraVars: extraVars,
		Tags:      config.Tags,
		SkipTags:  config.SkipTags,
		Verbose:   config.Verbose,
	}

	playbookCmd := playbook.NewAnsiblePlaybookCmd(
		playbook.WithPlaybooks(playbookPath),
		playbook.WithPlaybookOptions(playbookOpts),
	)

	exec := execute.NewDefaultExecute(
		execute.WithCmd(playbookCmd),
		execute.WithCmdRunDir(playbookDir),
		execute.WithWrite(execCtx.Stdout),
		execute.WithWriteError(execCtx.Stderr),
		execute.WithErrorEnrich(playbook.NewAnsiblePlaybookErrorEnrich()),
	)

	fmt.Fprintf(execCtx.Stdout, "Running ansible-playbook %s against %d node(s)...\n", config.Playbook, len(execCtx.Nodes))
	if err := exec.Execute(ctx); err != nil {
		return nil, fmt.Errorf("ansible-playbook failed: %w", err)
	}

	return map[string]string{"status": "success"}, nil
}

func writeInventory(dir string, nodes []executor.Node) (string, error) {
	var b strings.Builder
	b.WriteString("[all]\n")

	for i, n := range nodes {
		if n.Hostname == "" {
			return "", fmt.Errorf("node %d has empty hostname", i)
		}

		var fields []string
		fields = append(fields, fmt.Sprintf("ansible_host=%s", n.Hostname))
		if n.Port != 0 {
			fields = append(fields, fmt.Sprintf("ansible_port=%d", n.Port))
		}
		if n.Username != "" {
			fields = append(fields, fmt.Sprintf("ansible_user=%s", n.Username))
		}
		conn := n.ConnectionType
		if conn == "" {
			conn = "ssh"
		}
		fields = append(fields, fmt.Sprintf("ansible_connection=%s", conn))

		switch n.Auth.Method {
		case "key":
			if n.Auth.Key == "" {
				return "", fmt.Errorf("node %s has key auth but empty key", n.Hostname)
			}
			keyPath := filepath.Join(dir, fmt.Sprintf("key_%d", i))
			if err := os.WriteFile(keyPath, []byte(n.Auth.Key), 0600); err != nil {
				return "", fmt.Errorf("failed to write SSH key for %s: %w", n.Hostname, err)
			}
			fields = append(fields, fmt.Sprintf("ansible_ssh_private_key_file=%s", keyPath))
		case "password":
			fields = append(fields, fmt.Sprintf("ansible_password=%s", n.Auth.Key))
		case "":
			// no auth specified; rely on ansible/ssh defaults
		default:
			return "", fmt.Errorf("node %s has unsupported auth method %q", n.Hostname, n.Auth.Method)
		}

		fields = append(fields, "ansible_ssh_common_args='-o StrictHostKeyChecking=no -o IdentitiesOnly=yes -o PreferredAuthentications=publickey,password'")

		fmt.Fprintf(&b, "%s %s\n", n.Hostname, strings.Join(fields, " "))
	}

	invPath := filepath.Join(dir, "inventory.ini")
	if err := os.WriteFile(invPath, []byte(b.String()), 0600); err != nil {
		return "", fmt.Errorf("failed to write inventory file: %w", err)
	}
	return invPath, nil
}

type AnsibleExecutorPlugin struct{}

func (p *AnsibleExecutorPlugin) GetName() string {
	return "ansible"
}

func (p *AnsibleExecutorPlugin) GetSchema() interface{} {
	return GetSchema()
}

func (p *AnsibleExecutorPlugin) GetCapabilities() executor.Capability {
	return GetCapabilities()
}

func (p *AnsibleExecutorPlugin) New(name string, node executor.Node, execID string) (executor.Executor, error) {
	return NewAnsibleExecutor(name, node, execID)
}
