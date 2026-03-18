# flowctl-executors

Executor plugins for [flowctl](https://github.com/cvhariharan/flowctl). Refer to [Writing Executor Plugins](https://flowctl.net/docs/advanced/executor-plugins/) to build custom executors.

## Plugins

- **http** — Make HTTP requests
- **terraform** — Import and run terraform modules from git repos. Supports remote execution.


## Build
Run `make`, this will compile all the plugins and place the binaries in a `plugins` directory.
