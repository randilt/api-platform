/*
 * Copyright (c) 2026, WSO2 LLC. (https://www.wso2.com).
 *
 * WSO2 LLC. licenses this file to you under the Apache License,
 * Version 2.0 (the "License"); you may not use this file except
 * in compliance with the License.
 * You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied.  See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

package codemode

import (
	"time"

	"github.com/wso2/api-platform/tests/framework/core/catalog/shared"
	"github.com/wso2/api-platform/tests/framework/core/components"
)

// EnvImageExecutor overrides the executor image.
const EnvImageExecutor = "APIP_IT_IMAGE_CODEMODE_EXECUTOR"

// Name is the component name and its DNS alias on the block network.
const Name = "codemode-executor"

// Port is the executor's MCP port; OpsPort serves /healthz and /metrics.
const (
	Port    = 7100
	OpsPort = 9100
)

// Endpoint is the executor endpoint every Code Mode proxy in the suites points at:
// http://codemode-executor:7100/endpoints/it/mcp.
const Endpoint = "it"

const imgExecutor = "ghcr.io/wso2/api-platform/codemode-executor:test"

// config is the executor's configuration. The sources come from the header the gateway sets,
// joined onto the gateway runtime's router on the block network. The 3 s run timeout keeps the
// runaway-script scenario short.
const config = `
[server]
address = ":7100"
ops_address = ":9100"
max_concurrent_runs = 16

[sandbox]
root_dir = "/sandbox-root"
max_memory_bytes = 67108864

[[endpoints]]
name = "it"

[endpoints.gateway_sources]
base_url = "http://gateway-runtime:8080"
header = "X-Codemode-Sources"

[endpoints.limits]
timeout = "3s"
memory_bytes = 33554432
`

// Executor returns the Code Mode executor definition.
func Executor() *components.Definition {
	return &components.Definition{
		Name:  Name,
		Image: shared.Image(EnvImageExecutor, imgExecutor),
		Alias: Name,
		Files: []components.FileMount{{Content: []byte(config), ContainerPath: "/etc/codemode/config.toml", Mode: 0o644}},
		Cmd:   []string{"/executor", "-config", "/etc/codemode/config.toml"},
		Endpoints: []components.Endpoint{
			{Name: "mcp", Port: Port, Scheme: "http", AwaitListening: true},
			{Name: "ops", Port: OpsPort, Scheme: "http"},
		},
		Health: &components.HealthCheck{
			Endpoint: "ops", Path: "/healthz", ExpectStatus: 200,
			Timeout: 60 * time.Second, Interval: time.Second,
		},
		Limits: components.ResourceLimits{CPUs: 1, MemoryMB: 512},
	}
}
