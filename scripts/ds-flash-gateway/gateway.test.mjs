import { test, expect } from "bun:test";
import { normalize, Sessions, makeHandler, guardStream, upstreamHeaders, isForwardableSession, MODEL } from "./gateway.mjs";
const body = { model: MODEL, messages: [{ role: "system", content: "CLIENT_POLICY_ONE" },
  { role: "system", content: "CLIENT_POLICY_TWO" }, { role: "user", content: "task" }],
  tools: [{ type: "function", function: { name: "Write", parameters: { type: "object" } } }], max_tokens: 100 };

test("late stream failure keeps committed output once and emits an explicit error without replay", async () => {
  let reads = 0, failures = 0;
  const original = new Response(new ReadableStream({ pull(c) {
    if (reads++ === 0) c.enqueue(new TextEncoder().encode('data: {"choices":[{"delta":{"content":"once"}}]}\n\n'));
    else c.error(new Error("fixture connection reset"));
  } }), { headers: { "content-type": "text/event-stream" } });
  const output = await guardStream(original, new AbortController().signal, () => failures++).text();
  expect(output.match(/once/g)?.length).toBe(1);
  expect(output).toContain('"error":{"type":"api_error"');
  expect(output).not.toContain("[DONE]");
  expect(failures).toBe(1);
});

test("healthy SSE passes unchanged and cancellation does not report network failure", async () => {
  let failures = 0;
  const data = 'data: {"choices":[]}\n\ndata: [DONE]\n\n';
  const response = new Response(data, { headers: { "content-type": "text/event-stream" } });
  expect(await guardStream(response, new AbortController().signal, () => failures++).text()).toBe(data);
  expect(failures).toBe(0);
});

test("compatibility keeps client instructions, tools and history and is idempotent", () => {
  const result = JSON.parse(normalize(JSON.stringify(body)));
  expect(result.messages[0].content).toContain("CLIENT_POLICY_ONE\n\nCLIENT_POLICY_TWO");
  expect(result.messages.slice(1)).toEqual(body.messages.slice(2));
  expect(result.tools).toEqual(body.tools);
  expect(result.max_tokens).toBe(100);
  expect(normalize(JSON.stringify(result))).toBe(JSON.stringify(result));
  expect(() => normalize(JSON.stringify({ ...body, model: "other" }))).toThrow();
});

test("tool turns keep affinity; credentials are isolated; protocol time refresh is bounded", () => {
  const sessions = new Sessions();
  const a = new Headers({ authorization: "Bearer first", "x-opencode-session": "conversation" });
  const id = sessions.get(a, 1000000);
  expect(sessions.get(a, 1001000)).toBe(id);
  expect(sessions.get(new Headers({ authorization: "Bearer second", "x-opencode-session": "conversation" }), 1001000)).not.toBe(id);
  expect(sessions.get(a, 1000000 + 12 * 60 * 60 * 1000 - 1)).toBe(id);
  expect(sessions.get(a, 1000000 + 12 * 60 * 60 * 1000)).not.toBe(id);
});

test("a canonical session from the relay is preserved instead of rotated", () => {
  const relaySession = "ses_f534dfae8ffeCy4Ee4tLWNygDc";
  expect(isForwardableSession(relaySession)).toBe(true);
  expect(isForwardableSession("ses_" + "f".repeat(26))).toBe(false);
  const headers = upstreamHeaders(new Headers({
    authorization: "Bearer fixture",
    "x-opencode-session": relaySession,
  }), new Sessions());
  expect(headers.get("x-opencode-session")).toBe(relaySession);
  expect(headers.get("x-opencode-session-id")).toBe(relaySession);
  expect(headers.get("x-session-affinity")).toBe(relaySession);
  expect(headers.get("x-session-id")).toBe(relaySession);
});

test("an invalid or missing session still gets a canonical generated id", () => {
  const headers = upstreamHeaders(new Headers({ authorization: "Bearer fixture" }), new Sessions());
  expect(headers.get("x-opencode-session")).toMatch(/^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$/);
});

test("network rejection fails over with the same credential/model, and pins the successful route", async () => {
  const calls = [];
  const handler = makeHandler({ routes: [{ name: "phone", proxy: "phone" }, { name: "direct", proxy: false }],
    fetcher: async (_url, init) => {
      calls.push({ proxy: init.proxy, key: init.headers.get("authorization"), model: JSON.parse(init.body).model });
      return init.proxy ? Response.json({ error: { type: "FreeUsageLimitError" } }, { status: 429 }) : new Response("data: real reply\n\n");
    } });
  const request = () => new Request("http://localhost/v1/chat/completions", { method: "POST", headers: { authorization: "Bearer fixture" }, body: JSON.stringify(body) });
  expect(await (await handler(request())).text()).toContain("real reply");
  await (await handler(request())).text();
  expect(calls.map(c => c.proxy)).toEqual(["phone", false, false]);
  expect(calls.every(c => c.key === "Bearer fixture" && c.model === MODEL)).toBe(true);
});

test("rotate mode cycles WARP routes per request and keeps direct as fallback-only", async () => {
  const calls = [];
  const routes = [{ name: "warp1", proxy: "w1" }, { name: "warp2", proxy: "w2" },
    { name: "direct", proxy: false, fallback: true }];
  const handler = makeHandler({ routes, rotate: true, fetcher: async (_url, init) => {
    calls.push(init.proxy);
    return new Response("data: rotated reply\n\n");
  } });
  const request = () => new Request("http://localhost/v1/chat/completions", {
    method: "POST", headers: { authorization: "Bearer fixture" }, body: JSON.stringify(body) });
  for (let i = 0; i < 4; i++) expect(await (await handler(request())).text()).toContain("rotated reply");
  expect(calls).toEqual(["w1", "w2", "w1", "w2"]);
});

test("rotate mode falls back to direct only when every WARP route rejects", async () => {
  const calls = [];
  const routes = [{ name: "warp1", proxy: "w1" }, { name: "warp2", proxy: "w2" },
    { name: "direct", proxy: false, fallback: true }];
  const handler = makeHandler({ routes, rotate: true, fetcher: async (_url, init) => {
    calls.push(init.proxy);
    return init.proxy ? Response.json({ error: { type: "FreeUsageLimitError" } }, { status: 429 })
      : new Response("data: direct reply\n\n");
  } });
  const request = () => new Request("http://localhost/v1/chat/completions", {
    method: "POST", headers: { authorization: "Bearer fixture" }, body: JSON.stringify(body) });
  expect(await (await handler(request())).text()).toContain("direct reply");
  expect(await (await handler(request())).text()).toContain("direct reply");
  expect(calls).toEqual(["w1", "w2", false, "w1", "w2", false]);
});

test("quota exhaustion rotates egress and retries with a fresh session", async () => {
  let calls = 0, recoveries = 0, exhausted = 0;
  const sessionsSeen = [];
  const routes = [{ name: "warp1", proxy: "w1" }, { name: "warp2", proxy: "w2" },
    { name: "direct", proxy: false, fallback: true }];
  const handler = makeHandler({ routes, rotate: true, recoverRetries: 1,
    onExhausted: () => exhausted++,
    recover: async () => { recoveries++; return true; },
    fetcher: async (_url, init) => {
      calls++;
      sessionsSeen.push(init.headers.get("x-opencode-session"));
      if (calls <= 3) return Response.json({ error: { type: "FreeUsageLimitError" } },
        { status: 429, headers: { "Retry-After": "5" } });
      return new Response("data: recovered reply\n\n");
    } });
  const request = () => new Request("http://localhost/v1/chat/completions", {
    method: "POST", headers: { authorization: "Bearer fixture", "x-opencode-session": "conversation" }, body: JSON.stringify(body) });
  const response = await handler(request());
  expect(await response.text()).toContain("recovered reply");
  expect(recoveries).toBe(1);
  expect(exhausted).toBe(0);
  expect(calls).toBe(4);
  expect(sessionsSeen[0]).not.toBe(sessionsSeen[3]);
});

test("failed rotation preserves the real quota response and Retry-After", async () => {
  let calls = 0, exhausted = 0;
  const routes = [{ name: "warp1", proxy: "w1" }, { name: "warp2", proxy: "w2" },
    { name: "direct", proxy: false, fallback: true }];
  const handler = makeHandler({ routes, rotate: true, recoverRetries: 1,
    onExhausted: () => exhausted++,
    recover: async () => false,
    fetcher: async () => { calls++; return Response.json({ error: { type: "FreeUsageLimitError" } }, { status: 429, headers: { "Retry-After": "321" } }); } });
  const response = await handler(new Request("http://localhost/v1/chat/completions", {
    method: "POST", headers: { authorization: "Bearer fixture" }, body: JSON.stringify(body) }));
  expect(response.status).toBe(429);
  expect(response.headers.get("Retry-After")).toBe("321");
  expect(calls).toBe(3);
  expect(exhausted).toBe(1);
  await response.text();
});

test("auth errors do not rotate routes; genuine all-route quota responses keep Retry-After", async () => {
  for (const status of [401, 403, 429]) {
    let calls = 0, exhausted = 0;
    const handler = makeHandler({ routes: [{ name: "one", proxy: false }, { name: "two", proxy: false }],
      onExhausted: () => exhausted++, fetcher: async () => { calls++; return Response.json({ error: { type: status === 429 ? "FreeUsageLimitError" : "Unauthorized" } }, { status, headers: { "Retry-After": "123", "Content-Encoding": "gzip" } }); } });
    const result = await handler(new Request("http://localhost/v1/chat/completions", { method: "POST", headers: { authorization: "Bearer fixture" }, body: JSON.stringify(body) }));
    expect(result.status).toBe(status);
    expect(result.headers.get("Retry-After")).toBe("123");
    expect(result.headers.get("Content-Encoding")).toBe(null);
    expect(calls).toBe(status === 429 ? 2 : 1);
    expect(exhausted).toBe(status === 429 ? 1 : 0);
    await result.text();
  }
});
