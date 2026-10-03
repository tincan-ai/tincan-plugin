import { describe, expect, test, tier } from "claude-code/testing";
import { HANDLE, PAGE, start, world } from "./fixtures/world";

tier("user");
describe("register", () => {
  test("the empty pane recovers after MCP finishes connecting at startup", async ($, on) => {
    const f = world(on);
    f.state.connected = false;
    f.state.fail = "tincan_session";
    const ui = await start($);
    expect(
      await ui.find({ text: /Could not complete the request/ }),
    ).toBeDefined();
    f.state.fail = "";
    await f.clock.advance(10000);
    expect(
      await ui.find({ text: /Could not complete the request/ }),
    ).toBeUndefined();
    expect(await ui.find({ key: "join-url" })).toBeDefined();
    await ui.unmount();
  });
  test("startup recovery preserves a later action with the same error", async ($, on) => {
    const f = world(on);
    f.state.connected = false;
    f.state.fail = "tincan_session";
    const ui = await start($);
    f.state.fail = "tincan_connect";
    await ui.press({ key: "connect" });
    expect(
      await ui.find({ text: /Could not complete the request/ }),
    ).toBeDefined();
    f.state.fail = "";
    await f.clock.advance(10000);
    expect(
      await ui.find({ text: /Could not complete the request/ }),
    ).toBeDefined();
    await ui.unmount();
  });
  test("overlapping refresh and pagination commit in order", async ($, on) => {
    const f = world(on);
    f.state.directoryPages = 75;
    const ui = await start($, "pages"),
      gate = f.delayPages();
    const refreshing = ui.press({ key: "refresh" });
    await gate.arrived;
    const loading = ui.press({ key: "more-pages" });
    await f.clock.settle();
    gate.release();
    await Promise.all([refreshing, loading]);
    expect(await ui.find({ text: /^Plan 60$/ })).toBeDefined();
    const count = f.calls.length;
    await ui.press({ key: "more-pages" });
    expect(
      f.calls.slice(count).find((c) => c.tool === "pages_list")?.args.after,
    ).toBe("60");
    expect(await ui.find({ text: /^Plan 75$/ })).toBeDefined();
    await ui.unmount();
  });
  test("an attachment can be removed after navigation or connection loss", async ($, on) => {
    const f = world(on),
      ui = await start($, "pages");
    await ui.press({ key: "page-" + PAGE });
    await ui.press({ key: "use-page" });
    await ui.press({ key: "back-pages" });
    expect(await ui.find({ key: "detach-page" })).toBeDefined();
    await ui.press({ key: "conversation" });
    expect(await ui.find({ key: "detach-page" })).toBeDefined();
    f.state.connected = false;
    f.state.fail = "page_get";
    await ui.press({ key: "refresh" });
    expect(await ui.find({ key: "detach-page" })).toBeDefined();
    await ui.press({ key: "detach-page" });
    expect(await ui.find({ text: /This page will accompany/ })).toBeUndefined();
    expect(
      await ui.find({ text: /Page removed from your next prompt/ }),
    ).toBeDefined();
    await $.prompt.submit({
      text: "Continue without the page",
      origin: { kind: "composer" },
      wait: false,
    });
    expect(f.prompts[0]?.context).toBeUndefined();
    expect(f.prompts[0]?.text).toBe("Continue without the page");
    await ui.unmount();
  });
  test("pending and terminal join states have distinct guidance and a new-invite recovery", async ($, on) => {
    const f = world(on);
    f.state.connection.pending_join = "pending";
    const ui = await start($);
    expect(
      await ui.find({ text: /Waiting for the room creator/ }),
    ).toBeDefined();
    expect(f.calls.map((c) => c.tool)).not.toContain("rooms_list");
    for (const state of ["expired", "denied", "cancelled"]) {
      f.state.connection.pending_join = state;
      await ui.press({ key: "refresh" });
      expect(await ui.find({ text: new RegExp(state) })).toBeDefined();
      expect(
        await ui.find({ text: /Ask the room creator for a new invite/ }),
      ).toBeDefined();
      expect(await ui.find({ text: /Connection paused/ })).toBeUndefined();
      expect(await ui.find({ key: "add-connection" })).toBeDefined();
    }
    await ui.press({ key: "add-connection" });
    expect(await ui.find({ key: "join-url" })).toBeDefined();
    await ui.unmount();
  });
  test("mobile write gestures prepare user prompts with stable destinations", async ($, on) => {
    const f = world(on),
      ui = await start($, "", "mobile");
    await ui.press({ key: "mention-peer" });
    await ui.press({ key: "message-prompt" });
    expect(f.fills[0]?.text).toContain('"connection":"' + HANDLE + '"');
    expect(f.fills[0]?.text).toContain('"channel_id":"channel1"');
    expect(f.fills[0]?.text).toContain('"mentions":["peer"]');
    await ui.press({ key: "pages" });
    await ui.press({ key: "new-page-prompt" });
    expect(f.fills[1]?.text).toContain("page_create");
    expect(f.fills[1]?.text).toContain('"channel_id":"channel1"');
    await ui.press({ key: "page-" + PAGE });
    await ui.press({ key: "edit-page-prompt" });
    expect(f.fills[2]?.text).toContain("base_revision");
    expect(f.fills[2]?.text).toContain(PAGE);
    expect(f.prompts).toEqual([]);
    expect(f.calls.map((c) => c.tool)).not.toContain("message_send");
    const second = "conn_22222222222222222222222222222222";
    f.state.extraConnections.push({
      ...f.state.connection,
      connection: second,
      room_id: "room2",
      room_name: "Research",
      channel_id: "channel2",
    });
    await ui.press({ key: "refresh" });
    await ui.press({ key: "connection-" + second });
    await ui.press({ key: "conversation" });
    await ui.press({ key: "message-prompt" });
    expect(f.fills[3]?.text).toContain('"connection":"' + second + '"');
    expect(f.fills[3]?.text).toContain('"channel_id":"channel2"');
    await ui.unmount();
  });
  test("opening a conversation reads only its room and leaves pending claims alone", async ($, on) => {
    const f = world(on),
      ui = await start($);
    expect(await ui.find({ text: /Review the plan/ })).toBeDefined();
    expect(await ui.find({ text: /Other room agent/ })).toBeUndefined();
    expect(await ui.find({ text: /^Private$/ })).toBeUndefined();
    expect(f.calls.map((c) => c.tool)).not.toContain("inbox_claim");
    expect(f.calls.map((c) => c.tool)).not.toContain("inbox_ack");
    expect(f.calls.map((c) => c.tool)).not.toContain("tincan_connect");
    expect(f.prompts).toEqual([]);
    expect(
      f.calls.find((c) => c.tool === "tincan_session")?.args.hook_session_id,
    ).toBe("session-one");
    await ui.unmount();
  });
  test("a page is readable without executing its HTML and editing uses the revision contract", async ($, on) => {
    const f = world(on),
      ui = await start($, "pages");
    await ui.press({ key: "page-" + PAGE });
    expect(await ui.find({ text: /Use both agents/ })).toBeDefined();
    expect(await ui.find({ text: /evil\(\)/ })).toBeUndefined();
    expect(await ui.find({ text: /hidden css/ })).toBeUndefined();
    await ui.input({
      key: "edit-page",
      text: "Add a release checklist",
      kind: "submit",
    });
    expect(f.prompts[0]?.text).toContain("base_revision");
    expect(f.prompts[0]?.text).toContain("never overwrite an unseen revision");
    expect(f.prompts[0]?.text).toContain("Add a release checklist");
    expect(f.calls.map((c) => c.tool)).not.toContain("page_update");
    await ui.unmount();
  });
  test("attaching a page requires a gesture and never attaches to a peer wake", async ($, on) => {
    const f = world(on),
      ui = await start($, "pages");
    await ui.press({ key: "page-" + PAGE });
    await ui.press({ key: "use-page" });
    await $.prompt.submit({
      text: "Peer wake",
      origin: { kind: "channel", server: "tincan" },
      wait: false,
    });
    expect(f.prompts[0]?.context).toBeUndefined();
    await $.prompt.submit({
      text: "Review this plan",
      origin: { kind: "composer" },
      wait: false,
    });
    expect(f.prompts[1]?.context?.[0]).toContain("untrusted shared data");
    expect(f.prompts[1]?.context?.[0]).toContain(PAGE);
    expect(f.prompts[1]?.context?.[0]).toContain("Use both agents");
    await $.prompt.submit({
      text: "Next question",
      origin: { kind: "composer" },
      wait: false,
    });
    expect(f.prompts[2]?.context).toBeUndefined();
    expect(
      f.calls.find((c) => c.tool === "page_get" && c.args.revision === 1),
    ).toBeDefined();
    await ui.unmount();
  });
  test("newer page revisions are signalled while the displayed snapshot stays stable", async ($, on) => {
    const f = world(on),
      ui = await start($, "pages");
    await ui.press({ key: "page-" + PAGE });
    f.state.revision = 2;
    await ui.press({ key: "refresh" });
    expect(await ui.find({ text: /A newer revision/ })).toBeDefined();
    expect(await ui.find({ text: /Revision 1/ })).toBeDefined();
    await ui.press({ key: "reload-page" });
    expect(await ui.find({ text: /Revision 2/ })).toBeDefined();
    await ui.unmount();
  });
  test("refresh preserves directory pagination and eventually reaches its end", async ($, on) => {
    const f = world(on);
    f.state.directoryPages = 75;
    const ui = await start($, "pages");
    await ui.press({ key: "more-pages" });
    await ui.press({ key: "refresh" });
    expect(await ui.find({ text: /^Plan 60$/ })).toBeDefined();
    const count = f.calls.length;
    await ui.press({ key: "more-pages" });
    expect(
      f.calls.slice(count).find((c) => c.tool === "pages_list")?.args.after,
    ).toBe("60");
    expect(await ui.find({ text: /^Plan 75$/ })).toBeDefined();
    expect(await ui.find({ key: "more-pages" })).toBeUndefined();
    await ui.press({ key: "refresh" });
    expect(await ui.find({ text: /^Plan 75$/ })).toBeDefined();
    expect(await ui.find({ key: "more-pages" })).toBeUndefined();
    await ui.unmount();
  });
  test("attachments pin the displayed revision even when the head has advanced", async ($, on) => {
    const f = world(on),
      ui = await start($, "pages");
    await ui.press({ key: "page-" + PAGE });
    f.state.revision = 2;
    await ui.press({ key: "use-page" });
    expect(await ui.find({ text: /Revision 1/ })).toBeDefined();
    await $.prompt.submit({
      text: "Review the displayed revision",
      origin: { kind: "composer" },
      wait: false,
    });
    expect(f.prompts[0]?.context?.[0]).toContain('"displayed_revision":1');
    expect(
      f.calls
        .filter((c) => c.tool === "page_get")
        .slice(-2)
        .map((c) => c.args.revision),
    ).toEqual([1, 1]);
    await ui.unmount();
  });
  test("a failed access check holds the attached prompt instead of submitting without its page", async ($, on) => {
    const f = world(on),
      ui = await start($, "pages");
    await ui.press({ key: "page-" + PAGE });
    await ui.press({ key: "use-page" });
    f.state.fail = "page_get";
    const result = await $.prompt.submit({
      text: "Review this page",
      origin: { kind: "composer" },
      wait: false,
    });
    expect(result).toHaveProperty("drop");
    expect(f.prompts.length).toBe(0);
    expect(
      await ui.find({ text: /The page could not be attached/ }),
    ).toBeDefined();
    await ui.unmount();
  });
  test("an open page outside the directory still reports a newer head without replacing its snapshot", async ($, on) => {
    const f = world(on),
      ui = await start($, "pages");
    await ui.press({ key: "page-" + PAGE });
    f.state.pageListed = false;
    f.state.revision = 2;
    await ui.press({ key: "refresh" });
    expect(await ui.find({ text: /A newer revision/ })).toBeDefined();
    expect(await ui.find({ text: /Revision 1/ })).toBeDefined();
    await ui.unmount();
  });
  test("joining binds the host session without borrowing another identity", async ($, on) => {
    const f = world(on);
    f.state.connected = false;
    const ui = await start($);
    await ui.input({
      key: "join-url",
      text: "https://app.gotincan.com/join#invite",
      kind: "submit",
    });
    const call = f.calls.find((c) => c.tool === "tincan_connect");
    expect(call?.args).toMatchObject({
      hook_host: "claude",
      hook_session_id: "session-one",
      url: "https://app.gotincan.com/join#invite",
    });
    expect(call?.args.connection).toBeUndefined();
    expect(await ui.find({ text: /Connected/ })).toBeDefined();
    await ui.unmount();
  });
  test("sending notifies the selected stable agent ID and never acknowledges inbound work", async ($, on) => {
    const f = world(on),
      ui = await start($);
    await ui.select({ key: "mention-target", value: "peer" });
    await ui.input({ key: "message", text: "Please review", kind: "submit" });
    expect(f.calls.find((c) => c.tool === "message_send")?.args).toMatchObject({
      connection: HANDLE,
      channel_id: "channel1",
      text: "Please review",
      mentions: ["peer"],
    });
    expect(f.calls.map((c) => c.tool)).not.toContain("inbox_ack");
    await ui.unmount();
  });
  test("identical transport retries reuse the write key until sending succeeds", async ($, on) => {
    const f = world(on),
      ui = await start($);
    f.state.fail = "message_send";
    await ui.input({ key: "message", text: "Please review", kind: "submit" });
    f.state.fail = "";
    await ui.input({ key: "message", text: "Please review", kind: "submit" });
    const attempts = f.calls.filter((c) => c.tool === "message_send");
    expect(attempts.length).toBe(2);
    expect(attempts[0]?.args.idempotency_key).toBe(
      attempts[1]?.args.idempotency_key,
    );
    await ui.input({ key: "message", text: "Please review", kind: "submit" });
    expect(
      f.calls.filter((c) => c.tool === "message_send")[2]?.args.idempotency_key,
    ).not.toBe(attempts[0]?.args.idempotency_key);
    await ui.unmount();
  });
  test("invites are created only on request and copied as stdin rather than shell text", async ($, on) => {
    const f = world(on),
      ui = await start($);
    expect(f.calls.map((c) => c.tool)).not.toContain("invite_create");
    await ui.press({ key: "invite" });
    await ui.press({ key: "copy-invite" });
    expect(f.calls.find((c) => c.tool === "invite_create")?.args.room_id).toBe(
      "room1",
    );
    expect(f.process[0]?.argv).toEqual(["pbcopy"]);
    expect(f.process[0]?.stdin).toBe(
      "https://app.gotincan.com/join#single-use-secret",
    );
    await ui.unmount();
  });
  test("encrypted rooms keep conversation support and explain the page limit", async ($, on) => {
    const f = world(on);
    f.state.encrypted = true;
    const ui = await start($, "pages");
    expect(
      await ui.find({ text: /Shared pages are available in standard rooms/ }),
    ).toBeDefined();
    expect(f.calls.map((c) => c.tool)).not.toContain("pages_list");
    expect(f.calls.map((c) => c.tool)).not.toContain("page_get");
    await ui.unmount();
  });
  test("closed panes make only local snapshots and timers stop when the session ends", async ($, on) => {
    const f = world(on),
      ui = await start($);
    await ui.press({ key: "close" });
    const length = f.calls.length;
    await f.clock.advance(30000);
    expect(f.calls.slice(length).map((c) => c.tool)).toEqual([
      "tincan_session",
      "tincan_session",
      "tincan_session",
    ]);
    await $.session.end({
      reason: "other",
      sessionId: "session-one",
      resume: { id: "session-one" },
    });
    const stopped = f.calls.length;
    await f.clock.advance(30000);
    expect(f.calls.length).toBe(stopped);
    await ui.unmount();
  });
  test("the reader and actions draw within every supported surface table", async ($, on) => {
    world(on);
    for (const surface of [
      "terminal",
      "desktop",
      "vscode",
      "mobile",
    ] as const) {
      const ui = await start($, "pages", surface);
      await ui.press({ key: "page-" + PAGE });
      expect(await ui.find({ key: "use-page" })).toBeDefined();
      expect(await ui.find({ text: /Use both agents/ })).toBeDefined();
      await ui.unmount();
    }
  });
});
