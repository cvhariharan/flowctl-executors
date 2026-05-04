package main

import (
	"fmt"
	"os"

	sdkplugin "github.com/cvhariharan/flowctl/sdk/plugin"
	goplugin "github.com/hashicorp/go-plugin"
)

// version is set at build time via -ldflags "-X main.version=..."
var version = "dev"

func main() {
	fmt.Fprintf(os.Stderr, "http executor plugin version %s\n", version)
	goplugin.Serve(&goplugin.ServeConfig{
		HandshakeConfig: sdkplugin.HandshakeConfig,
		GRPCServer:      goplugin.DefaultGRPCServer,
		Plugins: map[string]goplugin.Plugin{
			"executor": &sdkplugin.GRPCExecutorPlugin{Impl: &HTTPExecutorPlugin{}},
		},
	})
}
