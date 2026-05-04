package main

import (
	sdkplugin "github.com/cvhariharan/flowctl/sdk/plugin"
	goplugin "github.com/hashicorp/go-plugin"
)

func main() {
	goplugin.Serve(&goplugin.ServeConfig{
		HandshakeConfig: sdkplugin.HandshakeConfig,
		GRPCServer:      goplugin.DefaultGRPCServer,
		Plugins: map[string]goplugin.Plugin{
			"executor": &sdkplugin.GRPCExecutorPlugin{Impl: &AnsibleExecutorPlugin{}},
		},
	})
}
