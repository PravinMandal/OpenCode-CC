// Capture the canonical opencode request identity.
//
// The upstream free tier matches the exact system prompt and tool list the
// stock client sends, and the system prompt embeds the model name. The default
// is the DeepSeek model this deployment serves; set CAPTURE_MODELS to a
// comma-separated list to capture others explicitly.
//
// Run it again after upgrading opencode; the bridge picks the new capture up.
import { $ } from "bun";
import { chmod, mkdir, writeFile } from "node:fs/promises";
import { homedir } from "node:os";
import { join } from "node:path";

const HOME = homedir();
const OUT_DIR = join(HOME, ".local", "share", "opencode-native");
const PRIMARY = "deepseek-v4.1-flash-free";
const ENDPOINT = "https://opencode.ai/inference/openai/v1/chat/completions";
const MODELS = (process.env.CAPTURE_MODELS || PRIMARY)
  .split(",").map(m => m.trim()).filter(Boolean);

const outPath = model => join(OUT_DIR, `identity-${model}.json`);

let version = "unknown";
try {
  version = (await $.env({ ...process.env, OPENCODE_CONFIG_CONTENT: "" })`opencode --version`.text()).trim().split("\n")[0];
} catch {}

async function capture(model) {
  const captured = { value: null };
  const server = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    async fetch(req) {
      const url = new URL(req.url);
      if (!url.pathname.endsWith("/chat/completions")) return new Response("not found", { status: 404 });
      const raw = await req.text();
      if (!captured.value) {
        const parsed = JSON.parse(raw);
        const first = parsed.messages?.[0];
        const tools = parsed.tools || null;
        if (first?.role === "system" && typeof first.content === "string" && tools?.length) {
          captured.value = {
            system: first.content,
            tools,
            headers: Object.fromEntries(
              [...req.headers.entries()].filter(([k]) =>
                k === "user-agent" || k.startsWith("x-opencode") ||
                k === "x-session-affinity" || k === "x-session-id")),
          };
        }
      }
      const stream = new ReadableStream({
        start(c) {
          const chunk = { id: "c1", object: "chat.completion.chunk", created: 1, model,
            choices: [{ index: 0, delta: { role: "assistant", content: "ok" }, finish_reason: "stop" }] };
          c.enqueue(new TextEncoder().encode(`data: ${JSON.stringify(chunk)}\n\ndata: [DONE]\n\n`));
          c.close();
        },
      });
      return new Response(stream, { headers: { "content-type": "text/event-stream" } });
    },
  });

  const config = JSON.stringify({
    providers: { opencode: { settings: { baseURL: `http://127.0.0.1:${server.port}/v1` } } },
  });

  try {
    await $`opencode --standalone run --model opencode/${model} --format json "capture identity"`
      .env({ ...process.env, OPENCODE_CONFIG_CONTENT: config })
      .quiet().nothrow();
  } catch {}

  server.stop(true);
  if (!captured.value) return { model, ok: false, reason: "no canonical request observed" };

  const identity = {
    version,
    capturedAt: new Date().toISOString(),
    model,
    endpoint: ENDPOINT,
    system: captured.value.system,
    tools: captured.value.tools,
    headers: captured.value.headers,
  };
  await writeFile(outPath(model), JSON.stringify(identity, null, 1) + "\n");
  await chmod(outPath(model), 0o600);
  return { model, ok: true, systemLen: identity.system.length, tools: identity.tools.length };
}

await mkdir(OUT_DIR, { recursive: true, mode: 0o700 });
const summary = [];
for (const model of MODELS) summary.push(await capture(model));

// identity.json stays the primary/default capture.
try {
  await writeFile(join(OUT_DIR, "identity.json"), await Bun.file(outPath(PRIMARY)).text());
  await chmod(join(OUT_DIR, "identity.json"), 0o600);
} catch {}

console.log(JSON.stringify({ ok: summary.every(s => s.ok), version, summary }, null, 1));
process.exit(summary.every(s => s.ok) ? 0 : 1);
