import type { On, RenderSurface } from "claude-code";
import type { Engine } from "claude-code/testing";
import { mock } from "claude-code/testing";

export const HANDLE = "conn_11111111111111111111111111111111";
export const PAGE = "page_11111111111111111111111111111111";
export function world(on: On) {
  const clock = mock.clock(on, { now: Date.parse("2026-10-01T12:00:00Z") });
  mock.store(on);
  mock.env(on, {});
  const calls: { tool: string; args: Record<string, unknown> }[] = [];
  const prompts: { text: string; context?: readonly string[] }[] = [];
  const fills: { text: string; mode?: string }[] = [];
  const status: (string | undefined)[] = [];
  const process: { argv: readonly string[]; stdin?: string }[] = [];
  const state = {
    connected: true,
    revision: 1,
    fail: "",
    encrypted: false,
    directoryPages: 1,
    pageListed: true,
    extraConnections: [] as unknown[],
    html: "<html><head><style>hidden css</style></head><body><h1>Shared plan</h1><p>Use both agents.</p><script>evil()</script></body></html>",
    connection: {
      connection: HANDLE,
      agent_id: "self",
      name: "Claude",
      server: "https://app.gotincan.com",
      room_id: "room1",
      room_name: "Product",
      channel_id: "channel1",
      background_listener: true,
      idle_wake: true,
      stream_state: "streaming",
      pending_count: 1,
      pending_join: "",
      pending_mentions: [
        {
          event_seq: 7,
          channel_id: "channel1",
          agent_name: "Codex",
          text: "Review the plan.",
          status: "running",
        },
      ],
    },
  };
  let pageGate: { entered: () => void; wait: Promise<void> } | undefined;
  function delayPages() {
    let entered!: () => void, release!: () => void;
    const arrived = new Promise<void>((resolve) => {
      entered = resolve;
    });
    const wait = new Promise<void>((resolve) => {
      release = resolve;
    });
    pageGate = { entered, wait };
    return { arrived, release };
  }
  const p = () => ({
    id: PAGE,
    channel_id: "channel1",
    title: "Shared plan",
    description: "A plan for both agents",
    revision: state.revision,
    updated_at: "2026-10-01T12:00:00Z",
  });
  on("session.start", ($, e) => ({ cwd: e.cwd }));
  on("session.id", () => ({ value: "session-one" }));
  on("session.version", () => ({ value: { version: "2.1.287" } }));
  on("session.model", () => ({ value: "claude-opus-4-6" }));
  on("command.register", ($, e) => ({ value: { command: e.name } }));
  on("ui.status", ($, e) => {
    status.push(e.text);
    return { value: undefined };
  });
  on("ui.invalidate", ($, e, next) => next(e));
  on("ui.open", () => ({ value: undefined }));
  on("ui.close", () => ({ value: undefined }));
  on("session.end", () => ({ sessionId: "session-one" }));
  on("prompt.submit", ($, e) => {
    prompts.push({ text: e.text, context: e.context });
    return { text: e.text, context: e.context, origin: e.origin };
  });
  on("prompt.fill", ($, e) => {
    fills.push({ text: e.text, mode: e.mode });
    return { isFilled: true };
  });
  on("process.run", ($, e) => {
    process.push({ argv: e.argv, stdin: e.init?.stdin });
    return { value: { exitCode: 0, stdout: "", stderr: "" } };
  });
  on("mcp.call", async ($, e) => {
    if (e.server !== "plugin:tincan:tincan")
      throw new Error("The mod must call its bundled, scoped MCP server.");
    calls.push({ tool: e.tool, args: e.args });
    if (state.fail === e.tool)
      return {
        value: {
          isError: true,
          content: [{ type: "text", text: "Connection unavailable" }],
        },
      };
    if (e.tool === "pages_list" && pageGate) {
      const gate = pageGate;
      pageGate = undefined;
      gate.entered();
      await gate.wait;
    }
    let value: unknown;
    switch (e.tool) {
      case "tincan_session":
        value = {
          connections: state.connected
            ? [state.connection, ...state.extraConnections]
            : [],
        };
        break;
      case "rooms_list":
        value = [
          {
            id: "room1",
            name: "Product",
            encryption_mode: state.encrypted ? "e2ee" : "standard",
          },
          { id: "room2", name: "Research", encryption_mode: "standard" },
          { id: "vault", name: "Private", private: true },
        ];
        break;
      case "channels_list":
        value = [
          { id: "channel1", room_id: "room1", name: "general" },
          { id: "channel2", room_id: "room2", name: "research" },
          { id: "notes", room_id: "vault", private: true, name: "private" },
        ];
        break;
      case "agents_list":
        value = [
          {
            id: "self",
            name: "Claude",
            presence: "available",
            presence_expires_at: "2026-10-01T12:01:30Z",
          },
          {
            id: "peer",
            name: "Codex",
            presence: "available",
            presence_expires_at: "2026-10-01T12:01:30Z",
          },
          { id: "stranger", name: "Other room agent", presence: "available" },
        ];
        break;
      case "room_members":
        value =
          e.args.room_id === "room1"
            ? [{ id: "self" }, { id: "peer" }]
            : [{ id: "self" }];
        break;
      case "messages_search":
        value = [
          {
            id: "msg1",
            seq: 7,
            channel_id: e.args.channel_id,
            agent_id: "peer",
            agent_name: "Codex",
            text: "Review the plan.",
            mentions: ["self"],
            attachments: [],
          },
        ];
        break;
      case "pages_list": {
        const all =
          state.directoryPages === 1
            ? state.pageListed
              ? [p()]
              : []
            : Array.from({ length: state.directoryPages }, (_, i) => ({
                ...p(),
                id: "page_" + (i + 1).toString(16).padStart(32, "0"),
                title: "Plan " + (i + 1),
              }));
        const after = Number(e.args.after || 0),
          end = after + Number(e.args.limit);
        value = {
          pages: e.args.channel_id === "channel1" ? all.slice(after, end) : [],
          next_cursor:
            e.args.channel_id === "channel1" && end < all.length
              ? String(end)
              : "",
        };
        break;
      }
      case "page_get":
        value = {
          page: p(),
          content: {
            revision: e.args.revision || state.revision,
            html: state.html,
            author_name: "Codex",
            created_at: "2026-10-01T12:00:00Z",
          },
        };
        break;
      case "tincan_connect":
        state.connected = true;
        value = { connection: HANDLE, user_message: "Connected." };
        break;
      case "invite_create":
        value = {
          url: "https://app.gotincan.com/join#single-use-secret",
          expires_at: "2026-10-02T12:00:00Z",
        };
        break;
      case "message_send":
        value = { id: "sent" };
        break;
      default:
        throw new Error("Unexpected tool: " + e.tool);
    }
    return {
      value: {
        isError: false,
        content: [{ type: "text", text: JSON.stringify(value) }],
      },
    };
  });
  return { state, calls, prompts, fills, status, process, clock, delayPages };
}

export async function start<P extends RenderSurface = "terminal">(
  $: Engine,
  tab = "",
  surface: P = "terminal" as P,
) {
  await $.session.start({ cwd: "/work", surface, isInteractive: true });
  await $.command.run({
    command: "tincan",
    args: tab,
    origin: { kind: "composer" },
    presentation: { columns: 160, isFullscreen: true },
  });
  return $.ui.mount({
    plugin: "tincan",
    surface,
    component: "Pane",
    requestId: "tincan",
    viewport: { columns: 160, rows: 48, isFullscreen: true },
    props: {
      title: "Tincan",
      isFocused: true,
      bodyColumns: 54,
      placement: "dock",
      scroll: { offset: 0, bodyRows: 40 },
      view: {},
    },
  });
}
