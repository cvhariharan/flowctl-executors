package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/cvhariharan/flowctl/sdk/executor"
	"github.com/invopop/jsonschema"
	"gopkg.in/yaml.v3"
)

type HTTPWithConfig struct {
	Method  string `yaml:"method" json:"method" jsonschema:"title=Method,enum=GET,enum=POST,enum=PUT,enum=PATCH,enum=DELETE,required"`
	URL     string `yaml:"url" json:"url" jsonschema:"title=URL,description=Request URL,required" jsonschema_extras:"placeholder=https://api.example.com/endpoint"`
	Headers string `yaml:"headers,omitempty" json:"headers,omitempty" jsonschema:"title=Headers,description=JSON headers object. Use ${VAR} for variable substitution" jsonschema_extras:"widget=keyvalue"`
	Body    string `yaml:"body,omitempty" json:"body,omitempty" jsonschema:"title=Body,description=Request body" jsonschema_extras:"widget=codeeditor"`
}

type HTTPExecutor struct {
	name string
}

func GetSchema() interface{} {
	return jsonschema.Reflect(&HTTPWithConfig{})
}

func NewHTTPExecutor(name string, node executor.Node, execID string) (executor.Executor, error) {
	return &HTTPExecutor{name: name}, nil
}

func (e *HTTPExecutor) GetArtifactsDir() string {
	return ""
}

func (e *HTTPExecutor) Close() error {
	return nil
}

func GetCapabilities() executor.Capability {
	return executor.StreamingOutput
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

func (e *HTTPExecutor) Execute(ctx context.Context, execCtx executor.ExecutionContext) (map[string]string, error) {
	var config HTTPWithConfig
	if err := yaml.Unmarshal(execCtx.WithConfig, &config); err != nil {
		return nil, fmt.Errorf("could not read config for HTTP executor %s: %w", e.name, err)
	}

	if config.URL == "" {
		return nil, fmt.Errorf("url is required for HTTP executor %s", e.name)
	}
	if config.Method == "" {
		config.Method = "GET"
	}

	// Substitute env vars in headers JSON string
	headers := make(map[string]string)
	if config.Headers != "" {
		resolved := substituteEnvVars(config.Headers, execCtx.Inputs)
		if err := json.Unmarshal([]byte(resolved), &headers); err != nil {
			return nil, fmt.Errorf("failed to parse headers JSON for HTTP executor %s: %w", e.name, err)
		}
	}

	var bodyReader io.Reader
	if config.Body != "" {
		bodyReader = strings.NewReader(config.Body)
	}

	req, err := http.NewRequestWithContext(ctx, config.Method, config.URL, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("failed to create HTTP request: %w", err)
	}

	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read HTTP response body: %w", err)
	}

	fmt.Fprintf(execCtx.Stdout, "status %d\n%s\n", resp.StatusCode, string(respBody))

	return map[string]string{
		"status_code": fmt.Sprintf("%d", resp.StatusCode),
		"body":        string(respBody),
	}, nil
}

// HTTPExecutorPlugin implements executor.ExecutorPlugin for the HTTP executor.
type HTTPExecutorPlugin struct{}

func (p *HTTPExecutorPlugin) GetName() string {
	return "http"
}

func (p *HTTPExecutorPlugin) GetSchema() interface{} {
	return GetSchema()
}

func (p *HTTPExecutorPlugin) GetCapabilities() executor.Capability {
	return GetCapabilities()
}

func (p *HTTPExecutorPlugin) New(name string, node executor.Node, execID string) (executor.Executor, error) {
	return NewHTTPExecutor(name, node, execID)
}
