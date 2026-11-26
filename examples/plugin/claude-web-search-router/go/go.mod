module github.com/router-for-me/CLIProxyAPI/v8/examples/plugin/claude-web-search-router/go

go 1.27.4

require (
	github.com/MoeForge/moego v0.1.0-alpha.5
	github.com/router-for-me/CLIProxyAPI/v8 v8.0.0
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/sirupsen/logrus v1.10.2 // indirect
	golang.org/x/net v0.63.0 // indirect
	golang.org/x/sys v0.50.0 // indirect
)

replace github.com/router-for-me/CLIProxyAPI/v8 => ../../../..

replace github.com/MoeForge/moego => github.com/router-for-me/moego v0.1.3
