# --------------------------------------------------------------------
# Copyright (c) 2026, WSO2 LLC. (https://www.wso2.com).
#
# WSO2 LLC. licenses this file to you under the Apache License,
# Version 2.0 (the "License"); you may not use this file except
# in compliance with the License.
# You may obtain a copy of the License at
#
# http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing,
# software distributed under the License is distributed on an
# "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
# KIND, either express or implied.  See the License for the
# specific language governing permissions and limitations
# under the License.
# --------------------------------------------------------------------

@CODEMODE
Feature: Code Mode over the gateway's MCP proxies
  As an agent developer
  I want one Code Mode MCP proxy that lets an agent search and script the tools of several MCP proxies
  So that the agent loads two tools instead of many, and every tool call it scripts is still governed by the gateway

  # Each scenario deploys its own three MCP proxies: "open" and "guarded" over the testbench MCP
  # service (add, echo, headers), guarded with mcp-acl-list denying echo, and the Code Mode proxy,
  # whose upstream is the block's executor and whose set-headers policy names the other two in
  # X-Codemode-Sources. The executor's run timeout in this block is 3 s.

  Background:
    Given the gateway services are running
    And I authenticate using basic auth as "admin"
    And I generate a unique resource name from "cm-open" and store it as "openName"
    And I generate a unique API version from "cm-open" and store it as "openVersion"
    And I generate a unique API context from "/cm-open" and store it as "openContext"
    And I generate a unique resource name from "cm-guarded" and store it as "guardedName"
    And I generate a unique API version from "cm-guarded" and store it as "guardedVersion"
    And I generate a unique API context from "/cm-guarded" and store it as "guardedContext"
    And I generate a unique resource name from "cm-proxy" and store it as "cmName"
    And I generate a unique API version from "cm-proxy" and store it as "cmVersion"
    And I generate a unique API context from "/cm-proxy" and store it as "cmContext"
    When I create MCP proxy from "resources/templates/mcp.yaml" with values:
      | apiVersion        | ${CTX:gatewaySpecVersion}                           |
      | name              | ${CTX:openName}                                     |
      | displayName       | ${CTX:openName}                                     |
      | version           | ${CTX:openVersion}                                  |
      | context           | ${CTX:openContext}                                  |
      | specVersion       | 2025-06-18                                          |
      | spec.upstream.url | http://testbench:3009${CTX:gatewayMCPUpstreamPath}  |
    Then the response should be successful
    When I create MCP proxy from "resources/templates/mcp.yaml" with values:
      | apiVersion        | ${CTX:gatewaySpecVersion}                           |
      | name              | ${CTX:guardedName}                                  |
      | displayName       | ${CTX:guardedName}                                  |
      | version           | ${CTX:guardedVersion}                               |
      | context           | ${CTX:guardedContext}                               |
      | specVersion       | 2025-06-18                                          |
      | spec.upstream.url | http://testbench:3009${CTX:gatewayMCPUpstreamPath}  |
      | spec.policies     | [{"name":"mcp-acl-list","version":"v1","params":{"tools":{"mode":"allow","exceptions":["echo"]}}}] |
    Then the response should be successful
    When I create MCP proxy from "resources/templates/mcp.yaml" with values:
      | apiVersion        | ${CTX:gatewaySpecVersion}                                          |
      | name              | ${CTX:cmName}                                                      |
      | displayName       | ${CTX:cmName}                                                      |
      | version           | ${CTX:cmVersion}                                                   |
      | context           | ${CTX:cmContext}                                                   |
      | specVersion       | 2025-06-18                                                         |
      | spec.upstream.url | http://codemode-executor:7100/endpoints/it${CTX:gatewayMCPUpstreamPath} |
      | spec.policies     | [{"name":"set-headers","version":"v1","params":{"request":{"headers":[{"name":"X-Codemode-Sources","value":"open=${CTX:openContext}/mcp,guarded=${CTX:guardedContext}/mcp"}]}}}] |
    Then the response should be successful
    And I set header "Content-Type" to "application/json"
    And I set header "Accept" to "application/json, text/event-stream"
    # The member routes first, so the executor finds both when the Code Mode route answers.
    And I send a "POST" request to "${CTX:openContext}/mcp" until status 200 with body:
      """
      {"jsonrpc":"2.0","id":0,"method":"tools/list"}
      """
    And I send a "POST" request to "${CTX:guardedContext}/mcp" until status 200 with body:
      """
      {"jsonrpc":"2.0","id":0,"method":"tools/list"}
      """
    And I send a "POST" request to "${CTX:cmContext}/mcp" until status 200 and the response body contains "guarded: {" with body:
      """
      {"jsonrpc":"2.0","id":0,"method":"tools/call","params":{"name":"search","arguments":{"query":""}}}
      """

  @CODEMODE-001
  Scenario: search declares every tool the caller may call, grouped by source
    When I send a "POST" request to "${CTX:cmContext}/mcp" with body:
      """
      {"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search","arguments":{"query":""}}}
      """
    Then the response status code should be 200
    And the JSON response field "result.structuredContent.matches" should be 5
    And the JSON response field "result.structuredContent.declarations" should be:
      """
      declare const codemode: {
        guarded: {
          /** Adds two numbers */
          add(args: { /** First number */ a: number; /** Second number */ b: number; }): Promise<unknown>;
          /** Reports the value of a header on the inbound HTTP request */
          headers(args: { /** Header name to read from the request */ name: string; }): Promise<unknown>;
        };
        open: {
          /** Adds two numbers */
          add(args: { /** First number */ a: number; /** Second number */ b: number; }): Promise<unknown>;
          /** Echoes back the input */
          echo(args: { /** Message to echo */ message: string; }): Promise<unknown>;
          /** Reports the value of a header on the inbound HTTP request */
          headers(args: { /** Header name to read from the request */ name: string; }): Promise<unknown>;
        };
      };
      """
    When I send a "POST" request to "${CTX:cmContext}/mcp" with body:
      """
      {"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search","arguments":{"query":"echo"}}}
      """
    Then the response status code should be 200
    And the JSON response field "result.structuredContent.matches" should be 1
    And the JSON response field "result.structuredContent.declarations" should contain "echo(args: { /** Message to echo */ message: string; })"

  @CODEMODE-002
  Scenario: A script combines tool results from two sources in one execute call
    When I send a "POST" request to "${CTX:cmContext}/mcp" with body:
      """
      {"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"execute","arguments":{"code":"const sum = await codemode.open.add({ a: 40, b: 2 }); const n = Number(sum.split(' is ')[1].replace('.', '')); const more = await codemode.guarded.add({ a: n, b: 1 }); const echoed = await codemode.open.echo({ message: 'total ' + n }); console.log('sum', n); return { n, more, echoed };"}}}
      """
    Then the response status code should be 200
    And the JSON response field "result.structuredContent.result.n" should be 42
    And the JSON response field "result.structuredContent.result.more" should be "The sum of 42 and 1 is 43."
    And the JSON response field "result.structuredContent.result.echoed" should be "Echo: total 42"
    And the JSON response field "result.structuredContent.logs[0]" should be "sum 42"
    And the JSON response array field "result.structuredContent.toolCalls" should have 3 items
    And the JSON response field "result.structuredContent.toolCalls[0].tool" should be "open.add"
    And the JSON response field "result.structuredContent.toolCalls[1].tool" should be "guarded.add"
    And the JSON response field "result.structuredContent.toolCalls[2].tool" should be "open.echo"
    And the JSON response field "result.structuredContent.toolCalls[2].outcome" should be "ok"
    And the JSON response field "result.structuredContent.error" should not exist

  @CODEMODE-003 @type:negative
  Scenario: A tool the source's policy denies fails inside the script
    When I send a "POST" request to "${CTX:cmContext}/mcp" with body:
      """
      {"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"execute","arguments":{"code":"const allowed = await codemode.guarded.add({ a: 1, b: 2 }); const name = ['ec', 'ho'].join(''); try { await codemode.guarded[name]({ message: 'x' }); return { allowed, denied: 'went through' }; } catch (e) { return { allowed, denied: e.message }; }"}}}
      """
    Then the response status code should be 200
    And the JSON response field "result.structuredContent.result.allowed" should be "The sum of 1 and 2 is 3."
    And the JSON response field "result.structuredContent.result.denied" should contain "MCP capability not allowed"
    And the JSON response field "result.structuredContent.toolCalls[0].outcome" should be "ok"
    And the JSON response field "result.structuredContent.toolCalls[1].tool" should be "guarded.echo"
    And the JSON response field "result.structuredContent.toolCalls[1].outcome" should be "failed"

  @CODEMODE-004 @type:negative
  Scenario: A tool error becomes an exception the script can catch
    When I send a "POST" request to "${CTX:cmContext}/mcp" with body:
      """
      {"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"execute","arguments":{"code":"const ok = await codemode.open.add({ a: 1, b: 1 }); try { await codemode.open.add({ a: 'one', b: 1 }); return { ok, caught: 'no error' }; } catch (e) { return { ok, caught: e.message }; }"}}}
      """
    Then the response status code should be 200
    And the JSON response field "result.structuredContent.result.ok" should be "The sum of 1 and 1 is 2."
    And the JSON response field "result.structuredContent.result.caught" should contain "validating"
    And the JSON response field "result.structuredContent.toolCalls[1].outcome" should be "tool_error"

  @CODEMODE-005
  Scenario: The upstream receives the run id on every inner call
    When I send a "POST" request to "${CTX:cmContext}/mcp" with body:
      """
      {"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"execute","arguments":{"code":"return [await codemode.open.headers({ name: 'X-Codemode-Run-Id' }), await codemode.guarded.headers({ name: 'X-Codemode-Run-Id' })];"}}}
      """
    Then the response status code should be 200
    And the JSON response field "result.structuredContent.runId" should match pattern "^[0-9a-f]{16}$"
    And I store the JSON response field "result.structuredContent.runId" as "runId"
    And the JSON response field "result.structuredContent.result[0]" should be "X-Codemode-Run-Id: ${CTX:runId}"
    And the JSON response field "result.structuredContent.result[1]" should be "X-Codemode-Run-Id: ${CTX:runId}"

  @CODEMODE-006 @type:negative
  Scenario: A script has no network and no file system
    When I send a "POST" request to "${CTX:cmContext}/mcp" with body:
      """
      {"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"execute","arguments":{"code":"const out = { fetch: typeof fetch }; const os = await import('qjs:os'); out.root = os.readdir('/')[0].filter(n => n !== '.' && n !== '..').length; const std = await import('qjs:std'); out.passwd = std.loadFile('/etc/passwd'); return out;"}}}
      """
    Then the response status code should be 200
    And the JSON response field "result.structuredContent.result.fetch" should be "undefined"
    And the JSON response field "result.structuredContent.result.root" should be 0
    And the JSON response field "result.structuredContent.result.passwd" should be null
    And the JSON response field "result.structuredContent.error" should not exist

  @CODEMODE-007 @type:negative
  Scenario: A runaway script is stopped at the time limit
    When I send a "POST" request to "${CTX:cmContext}/mcp" with body:
      """
      {"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"execute","arguments":{"code":"while (true) {}"}}}
      """
    Then the response status code should be 200
    And the JSON response field "result.isError" should be true
    And the JSON response field "result.structuredContent.error" should be "run stopped: exceeded 3s wall-clock limit"
    When I send a "POST" request to "${CTX:cmContext}/mcp" with body:
      """
      {"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"execute","arguments":{"code":"return 1 + 1;"}}}
      """
    Then the response status code should be 200
    And the JSON response field "result.structuredContent.result" should be 2

  @CODEMODE-008 @type:negative
  Scenario: A script that exhausts memory is stopped
    When I send a "POST" request to "${CTX:cmContext}/mcp" with body:
      """
      {"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"execute","arguments":{"code":"const a = []; while (true) a.push('x'.repeat(1 << 20));"}}}
      """
    Then the response status code should be 200
    And the JSON response field "result.isError" should be true
    And the JSON response field "result.structuredContent.error" should contain "out of memory"
    When I send a "POST" request to "${CTX:cmContext}/mcp" with body:
      """
      {"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"execute","arguments":{"code":"return 'still up';"}}}
      """
    Then the response status code should be 200
    And the JSON response field "result.structuredContent.result" should be "still up"

  @CODEMODE-009 @type:negative
  Scenario: A client cannot replace the sources the proxy names
    Given I set header "X-Codemode-Sources" to "open=${CTX:guardedContext}/mcp"
    When I send a "POST" request to "${CTX:cmContext}/mcp" with body:
      """
      {"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search","arguments":{"query":""}}}
      """
    Then the response status code should be 200
    And the JSON response field "result.structuredContent.matches" should be 5
    And the JSON response field "result.structuredContent.declarations" should contain "guarded: {"
    And the JSON response field "result.structuredContent.declarations" should contain "echo(args"

  @CODEMODE-010 @type:negative
  Scenario: A Code Mode proxy that names no sources is refused
    Given I generate a unique resource name from "cm-nosrc" and store it as "noSrcName"
    And I generate a unique API version from "cm-nosrc" and store it as "noSrcVersion"
    And I generate a unique API context from "/cm-nosrc" and store it as "noSrcContext"
    When I create MCP proxy from "resources/templates/mcp.yaml" with values:
      | apiVersion        | ${CTX:gatewaySpecVersion}                                          |
      | name              | ${CTX:noSrcName}                                                   |
      | displayName       | ${CTX:noSrcName}                                                   |
      | version           | ${CTX:noSrcVersion}                                                |
      | context           | ${CTX:noSrcContext}                                                |
      | specVersion       | 2025-06-18                                                         |
      | spec.upstream.url | http://codemode-executor:7100/endpoints/it${CTX:gatewayMCPUpstreamPath} |
    Then the response should be successful
    And I set header "Content-Type" to "application/json"
    And I set header "Accept" to "application/json, text/event-stream"
    When I send a "POST" request to "${CTX:noSrcContext}/mcp" until status 200 with body:
      """
      {"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search","arguments":{"query":""}}}
      """
    Then the JSON response field "result.isError" should be true
    And the JSON response field "result.content[0].text" should contain "the X-Codemode-Sources header is missing"
    # Positive control: the proxy that names its sources answers on the same executor.
    When I send a "POST" request to "${CTX:cmContext}/mcp" with body:
      """
      {"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search","arguments":{"query":""}}}
      """
    Then the response status code should be 200
    And the JSON response field "result.structuredContent.matches" should be 5
