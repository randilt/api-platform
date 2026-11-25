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

// Package codemode declares the Code Mode executor component: the MCP server, from
// gateway/codemode-executor, that gives an agent search and execute tools over the gateway's MCP
// proxies. Its single endpoint takes its sources from the X-Codemode-Sources header that the
// Code Mode proxy's set-headers policy sends, so every scenario deploys and names its own
// proxies and nothing in the executor's config is shared between scenarios.
package codemode
