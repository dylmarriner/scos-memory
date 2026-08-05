#!/usr/bin/env node
/**
 * SCOS Memory Fabric — stdio-to-Streamable-HTTP relay for Claude Desktop.
 *
 * Claude Desktop only launches local stdio MCP servers; mcp-gateway
 * (services/mcp-gateway) speaks MCP over Streamable HTTP. This process reads
 * one JSON-RPC message per line from stdin, POSTs it to the gateway with the
 * configured auth headers, and writes the JSON-RPC response back to stdout
 * as one line — mirroring what Claude Code's built-in "http" transport does
 * natively (see integrations/claude/plugins/scos-memory-fabric).
 *
 * All logging goes to stderr; stdout carries protocol traffic only.
 */
"use strict";

const readline = require("node:readline");

const GATEWAY_URL = process.env.SCOS_GATEWAY_URL;
const BEARER_TOKEN = process.env.SCOS_BEARER_TOKEN || "";
const TENANT_ID = process.env.SCOS_TENANT_ID || "";
const PRINCIPAL_ID = process.env.SCOS_PRINCIPAL_ID || "";

let sessionId = null;

function log(msg) {
  process.stderr.write(`[scos-mcp-relay] ${msg}\n`);
}

if (!GATEWAY_URL) {
  log("SCOS_GATEWAY_URL is not set — check the extension's configuration in Claude Desktop settings.");
  process.exit(1);
}

function baseHeaders() {
  const headers = {
    "Content-Type": "application/json",
    Accept: "application/json, text/event-stream",
  };
  if (BEARER_TOKEN) headers.Authorization = `Bearer ${BEARER_TOKEN}`;
  if (TENANT_ID) headers["X-SCOS-Tenant-ID"] = TENANT_ID;
  if (PRINCIPAL_ID) headers["X-SCOS-Principal-ID"] = PRINCIPAL_ID;
  if (sessionId) headers["Mcp-Session-Id"] = sessionId;
  return headers;
}

/** Parses a `text/event-stream` body into the JSON-RPC payloads carried in its `data:` lines. */
function parseSse(body) {
  const messages = [];
  for (const block of body.split("\n\n")) {
    const dataLines = block
      .split("\n")
      .filter((line) => line.startsWith("data:"))
      .map((line) => line.slice(5).trim());
    if (dataLines.length === 0) continue;
    try {
      messages.push(JSON.parse(dataLines.join("\n")));
    } catch (err) {
      log(`failed to parse SSE data block: ${err.message}`);
    }
  }
  return messages;
}

async function relay(line) {
  let request;
  try {
    request = JSON.parse(line);
  } catch {
    return; // not a JSON-RPC line, ignore
  }

  let response;
  try {
    response = await fetch(GATEWAY_URL, {
      method: "POST",
      headers: baseHeaders(),
      body: JSON.stringify(request),
    });
  } catch (err) {
    log(`transport error calling mcp-gateway: ${err.message}`);
    if (request.id !== undefined) {
      writeMessage({
        jsonrpc: "2.0",
        id: request.id,
        error: { code: -32001, message: `mcp-gateway unreachable: ${err.message}` },
      });
    }
    return;
  }

  const newSessionId = response.headers.get("mcp-session-id");
  if (newSessionId) sessionId = newSessionId;

  if (response.status === 202) {
    return; // notification acknowledged, no body expected
  }

  const contentType = response.headers.get("content-type") || "";
  const body = await response.text();
  if (!body) return;

  if (!response.ok) {
    log(`mcp-gateway HTTP ${response.status}: ${body.slice(0, 300)}`);
    if (request.id !== undefined) {
      writeMessage({
        jsonrpc: "2.0",
        id: request.id,
        error: { code: -32000, message: `mcp-gateway HTTP ${response.status}: ${body.slice(0, 500)}` },
      });
    }
    return;
  }

  if (contentType.includes("text/event-stream")) {
    for (const message of parseSse(body)) writeMessage(message);
    return;
  }

  try {
    writeMessage(JSON.parse(body));
  } catch (err) {
    log(`failed to parse JSON response: ${err.message}`);
  }
}

function writeMessage(message) {
  process.stdout.write(`${JSON.stringify(message)}\n`);
}

const rl = readline.createInterface({ input: process.stdin, terminal: false });
rl.on("line", (line) => {
  const trimmed = line.trim();
  if (!trimmed) return;
  relay(trimmed).catch((err) => log(`unhandled error: ${err.stack || err}`));
});

log(`relaying to ${GATEWAY_URL}`);
