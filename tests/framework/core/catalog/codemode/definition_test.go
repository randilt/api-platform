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
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestExecutorDefinition(t *testing.T) {
	definition := Executor()
	require.NoError(t, definition.Validate())
	require.Equal(t, Name, definition.Name)
	require.Equal(t, Name, definition.Alias)
	require.False(t, definition.Shared, "each block's executor calls that block's gateway")
	require.Equal(t, imgExecutor, definition.Image.Ref)

	mcp, ok := definition.Endpoint("mcp")
	require.True(t, ok)
	require.Equal(t, Port, mcp.Port)
	require.True(t, mcp.AwaitListening)
	ops, ok := definition.Endpoint("ops")
	require.True(t, ok)
	require.Equal(t, OpsPort, ops.Port)
	require.Equal(t, "/healthz", definition.Health.Path)

	require.Len(t, definition.Files, 1)
	require.Equal(t, "/etc/codemode/config.toml", definition.Files[0].ContainerPath)
	cfg := string(definition.Files[0].Content)
	require.True(t, strings.Contains(cfg, `name = "`+Endpoint+`"`), "the config serves the endpoint the suites call")
	require.Contains(t, cfg, `base_url = "http://gateway-runtime:8080"`)
	require.NotContains(t, cfg, "[[endpoints.sources]]", "sources come from the gateway, never from a fixed list")
	require.Equal(t, []string{"/executor", "-config", "/etc/codemode/config.toml"}, definition.Cmd)
}

func TestExecutorImageOverride(t *testing.T) {
	t.Setenv(EnvImageExecutor, "registry.example/codemode-executor:pinned")
	require.Equal(t, "registry.example/codemode-executor:pinned", Executor().Image.Ref)

	t.Setenv(EnvImageExecutor, "   ")
	require.Equal(t, imgExecutor, Executor().Image.Ref, "a blank override keeps the default")
}
