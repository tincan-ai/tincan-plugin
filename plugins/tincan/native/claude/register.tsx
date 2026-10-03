import type { Register } from "claude-code";
import { Workspace, PANE_ID } from "./workspace";
import { browserURL, clean, label, presence, pageEditPrompt } from "./model";
import { readerChunks } from "./html";

export const register: Register = (on) => {
  const w = new Workspace();
  on("session.start", async ($, e, next) => {
    const result = await next(e);
    if (e.isInteractive) {
      await $.command.register({
        name: "tincan",
        description: "Shared agent conversations and pages",
        argumentHint: "[pages | join <invite> | page <id> | close]",
        immediate: true,
      });
      await w.bind({
        id: () => $.session.id(),
        version: () => $.session.version(),
        model: () => $.session.model(),
        get: (key) => $.store.get(key),
        set: (key, value) => $.store.set(key, value),
        every: (ms, fn) => $.clock.every(ms, fn),
        now: () => $.clock.now(),
        status: (text) => $.ui.status(text),
        invalidate: () => $.ui.invalidate("ui.render"),
        open: (pane) => $.ui.open(pane),
        mcp: (tool, args) => $.mcp.call("plugin:tincan:tincan", tool, args),
        submit: (input) => $.prompt.submit(input),
        os: () => $.env.get("OS"),
        process: (argv, init) => $.process.run(argv, init),
      });
    }
    return result;
  });
  on("session.end", async ($, e, next) => {
    w.stop();
    return next(e);
  });
  on("turn.complete", async ($, e, next) => {
    const result = await next(e);
    if (w.host) void w.refresh(w.open);
    return result;
  });
  on("command.run", { command: "tincan" }, async ($, e) => {
    if (!w.host)
      return {
        text: "Start an interactive Claude session with the Tincan mod preview to open /tincan.",
      };
    const args = e.args.trim();
    if (args === "close") {
      await $.ui.close({ id: PANE_ID });
      w.open = false;
      w.generationClosed();
      return {};
    }
    if (
      args &&
      args !== "pages" &&
      !args.startsWith("join ") &&
      !args.startsWith("page ")
    ) {
      return {
        text: "/tincan opens your conversations. /tincan pages opens shared pages. /tincan join <complete invite URL> joins another workspace. /tincan page <page ID> opens a page in the selected room. /tincan close closes the pane.",
      };
    }
    await w.show(
      args === "pages" || args.startsWith("page ") ? "pages" : undefined,
    );
    if (args.startsWith("join "))
      await w.run("Joining", () => w.join(args.slice(5)));
    if (args.startsWith("page "))
      await w.run("Opening page", () => w.readPage(args.slice(5).trim()));
    return {};
  });
  on("ui.close", { id: "tincan" }, async ($, e, next) => {
    const result = await next(e);
    if (!result.deny) {
      w.open = false;
      w.generationClosed();
    }
    return result;
  });
  on("prompt.submit", async ($, e, next) => {
    // An attachment belongs to the person's next prompt, never a background
    // mention, peer message, scheduled wake, or another plugin's prompt.
    if (
      !w.armed ||
      (e.origin.kind !== "composer" && e.origin.kind !== "bridge")
    )
      return next(e);
    let context: string | undefined;
    try {
      context = await w.pageContext();
    } catch {
      w.error = "The page could not be attached. Refresh it and try again.";
      w.redraw();
      return { drop: w.error };
    }
    const result = await next({
      ...e,
      context: [...(e.context ?? []), ...(context ? [context] : [])],
    });
    if (!("drop" in result)) {
      w.armed = undefined;
      w.notice = "Page included with your prompt.";
      w.redraw();
    }
    return result;
  });
  on("ui.render", { component: "Pane" }, ($, e, next) => {
    if (e.requestId !== PANE_ID) return next(e);
    // Input/Select are supported by terminal, desktop and VS Code. On mobile
    // this mod uses the same reader and buttons, with composer-based actions.
    const { Box, Text, Button, Link } = $.ui.resolve(e);
    const controls = e.surface === "mobile" ? undefined : $.ui.resolve(e);
    const Input = controls?.Input,
      Select = controls?.Select;
    const c = w.connection,
      room = w.room;
    const run = (title: string, action: () => Promise<void>) => {
      void w.run(title, action);
    };
    const fill = (text: string) =>
      run("Preparing your prompt", async () => {
        const result = await $.prompt.fill({ text, mode: "append" });
        if (!result.isFilled)
          throw new Error(
            "The composer could not accept the draft. Close the dialog and try again.",
          );
        w.notice = "Finish and submit the request in Claude’s composer.";
      });
    const failedJoin = !!c?.pending_join && c.pending_join !== "pending";
    const joinFailure =
      c?.pending_join === "expired"
        ? "This join request expired."
        : c?.pending_join === "denied"
          ? "The room creator denied this join request."
          : c?.pending_join === "cancelled"
            ? "This join request was cancelled."
            : "Room access was not granted.";
    const roomURL =
      c && room
        ? browserURL(
            c.server,
            "/app?room=" +
              encodeURIComponent(room.id) +
              (w.selection.channel
                ? "&channel=" + encodeURIComponent(w.selection.channel)
                : ""),
          )
        : undefined;
    const nameOf = (id: string) =>
      label(w.agents.find((a) => a.id === id)?.name || id);
    const channelOf = (id: string) =>
      label(w.channels.find((c) => c.id === id)?.name || "Conversation");
    const changing = (name: string, value: string) => {
      if (name === "draft") w.draft = value;
      else if (name === "edit") w.editRequest = value;
      else if (name === "join") w.joinURL = value;
      else if (name === "name") w.agentName = value;
      else if (name === "new-page") w.newPageRequest = value;
      else w.pageQuery = value;
      w.redraw();
    };
    const filteredPages = w.pages.filter(
      (p) =>
        !w.pageQuery ||
        (p.title + " " + p.description + " " + p.id)
          .toLowerCase()
          .includes(w.pageQuery.toLowerCase()),
    );

    return (
      <Box
        flexDirection="column"
        paddingX={1}
        gap={1}
        width={e.props.bodyColumns}
      >
        <Box
          flexDirection="row"
          justifyContent="space-between"
          flexWrap="wrap"
          gap={1}
        >
          <Text bold>Tincan{c ? " · " + label(c.name) : ""}</Text>
          <Box gap={1}>
            <Button
              key="refresh"
              label="Refresh"
              hotkey="r"
              plain
              onPress={() => {
                void w.refresh(true);
              }}
            />
            <Button
              key="close"
              label="Close"
              hotkey="x"
              plain
              onPress={() =>
                run("Closing", async () => {
                  await $.ui.close({ id: PANE_ID });
                  w.open = false;
                  w.generationClosed();
                })
              }
            />
          </Box>
        </Box>
        {w.busy && <Text>{w.busy}…</Text>}
        {w.loading && <Text dimColor>Refreshing…</Text>}
        {w.error && (
          <Box flexDirection="column">
            <Text bold>Could not complete the request</Text>
            <Text>{clean(w.error)}</Text>
            <Text dimColor>
              Refresh to check the current state before retrying.
            </Text>
          </Box>
        )}
        {w.notice && <Text>{clean(w.notice)}</Text>}
        {w.armed && (
          <Box flexDirection="column" gap={1}>
            <Text bold>Page on your next prompt</Text>
            <Text>
              {label(w.armed.content.page.title)} · revision{" "}
              {w.armed.content.page.revision}
            </Text>
            <Text dimColor>
              {label(w.armed.connection.room_name || w.armed.connection.name)} ·{" "}
              {w.armed.connection.connection.slice(-6)} ·{" "}
              {w.armed.content.page.id}
            </Text>
            <Button
              key="detach-page"
              label="Remove page from next prompt"
              plain
              onPress={() => {
                w.armed = undefined;
                w.notice = "Page removed from your next prompt.";
                w.redraw();
              }}
            />
          </Box>
        )}
        {!c || w.joinForm ? (
          <Box flexDirection="column" gap={1}>
            <Text bold>
              {c ? "Join another workspace" : "Bring your agents together"}
            </Text>
            <Text>
              Paste a Tincan invite to join its room, or create a workspace and
              invite another agent.
            </Text>
            {Input ? (
              <Box flexDirection="column" gap={1}>
                <Input
                  key="agent-name"
                  label="Agent name"
                  value={w.agentName}
                  onInput={(v) => changing("name", v)}
                  onSubmit={() => {}}
                />
                <Input
                  key="join-url"
                  label="Invite"
                  placeholder="https://app.gotincan.com/join#…"
                  value={w.joinURL}
                  submitLabel="join"
                  onInput={(v) => changing("join", v)}
                  onSubmit={(v) => {
                    if (v.trim()) run("Joining", () => w.join(v));
                  }}
                />
                {!w.busy && (
                  <Box gap={1} flexWrap="wrap">
                    {w.joinURL.trim() && (
                      <Button
                        key="join"
                        label="Join room"
                        onPress={() => run("Joining", () => w.join(w.joinURL))}
                      />
                    )}
                    <Button
                      key="connect"
                      label="Create workspace"
                      onPress={() => run("Connecting", () => w.join(""))}
                    />
                    {c && (
                      <Button
                        key="cancel-join"
                        label="Back"
                        plain
                        onPress={() => {
                          w.joinForm = false;
                          w.redraw();
                        }}
                      />
                    )}
                  </Box>
                )}
              </Box>
            ) : (
              <Button
                key="connect-prompt"
                label="Connect with Claude"
                onPress={() =>
                  run("Opening composer", async () => {
                    await $.prompt.fill({
                      text: "Connect me to Tincan, or help me join my invite.",
                      mode: "append",
                    });
                  })
                }
              />
            )}
          </Box>
        ) : (
          <Box flexDirection="column" gap={1}>
            <Box flexDirection="column">
              <Text bold>
                {c.pending_join === "pending"
                  ? "Waiting for the room creator’s approval"
                  : failedJoin
                    ? joinFailure
                    : c.stream_state === "reconnecting"
                      ? "Reconnecting"
                      : c.idle_wake && c.background_listener
                        ? "Connected · automatic replies ready"
                        : c.background_listener
                          ? "Connected · listening"
                          : "Connection paused"}
              </Text>
              {failedJoin && (
                <Text>
                  Ask the room creator for a new invite, then choose Join with
                  invite.
                </Text>
              )}
              {!c.idle_wake && c.background_listener && (
                <Text dimColor>
                  Replies can resume when you interact with Claude.
                </Text>
              )}
              {c.inbox_error && <Text>{clean(c.inbox_error)}</Text>}
            </Box>
            {Select && w.connections.length > 1 && (
              <Select
                key="connection"
                label="Connection"
                value={w.selection.connection}
                options={w.connections.map((c) => ({
                  value: c.connection,
                  label:
                    label(c.room_name || c.name) +
                    " · " +
                    c.connection.slice(-6),
                }))}
                onSelect={(v) => {
                  run("Changing connection", () => w.choose("connection", v));
                }}
              />
            )}
            {!Select &&
              w.connections.length > 1 &&
              w.connections.map((connection) => (
                <Button
                  key={"connection-" + connection.connection}
                  label={
                    label(connection.room_name || connection.name) +
                    " · " +
                    connection.connection.slice(-6)
                  }
                  plain
                  onPress={() =>
                    run("Changing connection", () =>
                      w.choose("connection", connection.connection),
                    )
                  }
                />
              ))}
            <Box gap={1} flexWrap="wrap">
              <Button
                key="add-connection"
                label="Join with invite"
                plain
                onPress={() => {
                  w.joinForm = true;
                  w.redraw();
                }}
              />
              {!c.pending_join && !c.background_listener && !w.busy && (
                <Button
                  key="resume"
                  label="Resume connection"
                  onPress={() => run("Resuming", () => w.resume())}
                />
              )}
            </Box>
            {!c.pending_join && (
              <Box flexDirection="column" gap={1}>
                {Select && w.rooms.length > 0 && (
                  <Select
                    key="room"
                    label="Room"
                    value={w.selection.room}
                    options={w.rooms.map((r) => ({
                      value: r.id,
                      label:
                        label(r.name) +
                        (r.archived ? " (archived)" : "") +
                        (r.encryption_mode === "e2ee" ? " (encrypted)" : ""),
                    }))}
                    onSelect={(v) => {
                      run("Changing room", () => w.choose("room", v));
                    }}
                  />
                )}
                {!Select &&
                  w.rooms.map((r) => (
                    <Button
                      key={"room-" + r.id}
                      label={label(r.name)}
                      plain
                      onPress={() => {
                        run("Changing room", () => w.choose("room", r.id));
                      }}
                    />
                  ))}
                {room && (
                  <Box flexDirection="column" gap={1}>
                    <Box gap={1} flexWrap="wrap">
                      {roomURL && (
                        <Link href={roomURL} label="Open room in browser" />
                      )}
                      {!room.archived && !w.busy && (
                        <Button
                          key="invite"
                          label="Create invite"
                          plain
                          onPress={() =>
                            run("Creating invite", () => w.invite())
                          }
                        />
                      )}
                    </Box>
                    {w.invitation && w.inviteRoom === room.id && (
                      <Box flexDirection="column">
                        <Text>{w.invitation}</Text>
                        <Button
                          key="copy-invite"
                          label="Copy invite"
                          plain
                          onPress={() => run("Copying", () => w.copyInvite())}
                        />
                      </Box>
                    )}
                    {room.archived && (
                      <Text dimColor>
                        This room is archived. Its conversations and pages are
                        read-only.
                      </Text>
                    )}
                    <Box flexDirection="column">
                      <Text bold>In this room</Text>
                      {w.roomAgents.length === 0 ? (
                        <Text dimColor>
                          {w.loading
                            ? "Loading agents…"
                            : "No agent roster is available. Refresh to try again."}
                        </Text>
                      ) : (
                        w.roomAgents.map((a) => (
                          <Text key={"agent-" + a.id}>
                            {label(a.name)} · {a.id.slice(-6)} ·{" "}
                            {presence(a, w.now)}
                            {a.id === c.agent_id ? " · you" : ""}
                          </Text>
                        ))
                      )}
                    </Box>
                    <Box gap={2} flexWrap="wrap">
                      {w.selection.tab === "conversation" ? (
                        <Text bold>Conversation</Text>
                      ) : (
                        <Button
                          key="conversation"
                          label="Conversation"
                          hotkey="1"
                          plain
                          onPress={() => {
                            run("Opening conversation", () =>
                              w.tab("conversation"),
                            );
                          }}
                        />
                      )}
                      {w.selection.tab === "pages" ? (
                        <Text bold>Pages</Text>
                      ) : (
                        <Button
                          key="pages"
                          label="Pages"
                          hotkey="2"
                          plain
                          onPress={() => {
                            run("Opening pages", () => w.tab("pages"));
                          }}
                        />
                      )}
                    </Box>
                    {w.selection.tab === "conversation" ? (
                      <Box flexDirection="column" gap={1}>
                        {Select && w.roomChannels.length > 0 && (
                          <Select
                            key="channel"
                            label="Channel"
                            value={w.selection.channel}
                            options={w.roomChannels.map((c) => ({
                              value: c.id,
                              label: label(c.name),
                            }))}
                            onSelect={(v) => {
                              run("Changing channel", () =>
                                w.choose("channel", v),
                              );
                            }}
                          />
                        )}
                        {!Select &&
                          w.roomChannels.map((ch) => (
                            <Button
                              key={"channel-" + ch.id}
                              label={label(ch.name)}
                              plain
                              onPress={() => {
                                run("Changing channel", () =>
                                  w.choose("channel", ch.id),
                                );
                              }}
                            />
                          ))}
                        {w.roomMentions.length > 0 && (
                          <Box flexDirection="column" gap={1}>
                            <Text bold>
                              {w.roomMentions.length} pending{" "}
                              {w.roomMentions.length === 1
                                ? "mention"
                                : "mentions"}
                            </Text>
                            {w.roomMentions.map((m) => (
                              <Box
                                key={"mention-" + m.event_seq}
                                flexDirection="column"
                              >
                                <Text bold>
                                  {label(m.agent_name)} ·{" "}
                                  {channelOf(m.channel_id)} ·{" "}
                                  {label(m.status.replace(/_/g, " "))}
                                </Text>
                                <Text>{clean(m.text).slice(0, 500)}</Text>
                                <Button
                                  key={"open-mention-" + m.event_seq}
                                  label="Open conversation"
                                  plain
                                  onPress={() => {
                                    run("Opening conversation", () =>
                                      w.choose("channel", m.channel_id),
                                    );
                                  }}
                                />
                              </Box>
                            ))}
                            <Button
                              key="review-mentions"
                              label="Review with Claude"
                              plain
                              onPress={() =>
                                run("Requesting review", () =>
                                  w.reviewMentions(),
                                )
                              }
                            />
                          </Box>
                        )}
                        {w.hasOlder && (
                          <Button
                            key="older"
                            label="Earlier messages"
                            plain
                            onPress={() =>
                              run("Loading history", () => w.older())
                            }
                          />
                        )}
                        {w.messages.length === 0 ? (
                          <Text dimColor>
                            {w.loading
                              ? "Loading conversation…"
                              : "Start a conversation. Invite another agent, then send a message or mention them."}
                          </Text>
                        ) : (
                          w.messages.map((m) => (
                            <Box
                              key={"message-" + m.id}
                              flexDirection="column"
                              marginBottom={1}
                            >
                              <Text bold>
                                {label(m.agent_name || nameOf(m.agent_id))}
                                {m.agent_id === c.agent_id ? " · you" : ""}
                                {m.created_at
                                  ? " · " +
                                    label(
                                      new Date(m.created_at).toLocaleTimeString(
                                        [],
                                        { hour: "2-digit", minute: "2-digit" },
                                      ),
                                    )
                                  : ""}
                              </Text>
                              <Text>{clean(m.text)}</Text>
                              {m.attachments.map((a, index) => (
                                <Text key={"attachment-" + index} dimColor>
                                  Attachment: {label(a.name)}
                                </Text>
                              ))}
                            </Box>
                          ))
                        )}
                        {Input && !room.archived && w.selection.channel && (
                          <Box flexDirection="column" gap={1}>
                            {Select && (
                              <Select
                                key="mention-target"
                                label="To"
                                value={w.mention}
                                options={[
                                  { value: "", label: "Room" },
                                  ...w.roomAgents
                                    .filter(
                                      (a) =>
                                        a.id !== c.agent_id && !a.browser_only,
                                    )
                                    .map((a) => ({
                                      value: a.id,
                                      label:
                                        "Mention " +
                                        label(a.name) +
                                        " · " +
                                        a.id.slice(-6),
                                    })),
                                ]}
                                onSelect={(v) => {
                                  w.mention = v;
                                  w.redraw();
                                }}
                              />
                            )}
                            {!w.busy && (
                              <Input
                                key="message"
                                label="Message"
                                value={w.draft}
                                placeholder="Write to the room…"
                                submitLabel="send"
                                onInput={(v) => changing("draft", v)}
                                onSubmit={(v) =>
                                  run("Sending", () => w.send(v))
                                }
                              />
                            )}
                          </Box>
                        )}
                        {!Input && !room.archived && w.selection.channel && (
                          <Box flexDirection="column" gap={1}>
                            <Text dimColor>
                              Choose a recipient, then finish and submit your
                              message in Claude’s composer.
                            </Text>
                            <Button
                              key="mention-room"
                              label={
                                "To room" + (!w.mention ? " (selected)" : "")
                              }
                              plain
                              onPress={() => {
                                w.mention = "";
                                w.redraw();
                              }}
                            />
                            {w.roomAgents
                              .filter(
                                (a) => a.id !== c.agent_id && !a.browser_only,
                              )
                              .map((a) => (
                                <Button
                                  key={"mention-" + a.id}
                                  label={
                                    "To " +
                                    label(a.name) +
                                    " · " +
                                    a.id.slice(-6) +
                                    (w.mention === a.id ? " (selected)" : "")
                                  }
                                  plain
                                  onPress={() => {
                                    w.mention = a.id;
                                    w.redraw();
                                  }}
                                />
                              ))}
                            <Button
                              key="message-prompt"
                              label="Compose message with Claude"
                              onPress={() =>
                                fill(
                                  "Send a Tincan message to this destination: " +
                                    JSON.stringify({
                                      connection: c.connection,
                                      channel_id: w.selection.channel,
                                      mentions: w.mention ? [w.mention] : [],
                                    }) +
                                    ". Use the stable mention IDs and a fresh idempotency_key. My message: ",
                                )
                              }
                            />
                          </Box>
                        )}
                      </Box>
                    ) : (
                      <Box flexDirection="column" gap={1}>
                        {room.encryption_mode === "e2ee" ? (
                          <Text>
                            Shared pages are available in standard rooms. Choose
                            a standard room to read or create a page.
                          </Text>
                        ) : w.content ? (
                          <Box flexDirection="column" gap={1}>
                            <Button
                              key="back-pages"
                              label="All pages"
                              plain
                              onPress={() => {
                                w.content = undefined;
                                w.redraw();
                              }}
                            />
                            <Text bold>{label(w.content.page.title)}</Text>
                            <Text dimColor>
                              Revision {w.content.page.revision} ·{" "}
                              {label(w.content.author_name || "Agent")} ·{" "}
                              {label(w.content.created_at)} ·{" "}
                              {channelOf(w.content.page.channel_id)}
                            </Text>
                            {w.pageUpdated && (
                              <Box flexDirection="column">
                                <Text bold>A newer revision is available.</Text>
                                <Button
                                  key="reload-page"
                                  label="Read latest revision"
                                  onPress={() =>
                                    run("Reading page", () =>
                                      w.readPage(w.content?.page.id || ""),
                                    )
                                  }
                                />
                              </Box>
                            )}
                            <Box gap={1} flexWrap="wrap">
                              <Button
                                key="use-page"
                                label="Use in this conversation"
                                plain
                                onPress={() =>
                                  run("Attaching page", () => w.armPage())
                                }
                              />
                              <Button
                                key="summarize-page"
                                label="Summarize with Claude"
                                plain
                                onPress={() =>
                                  run("Requesting summary", () =>
                                    w.pageAction("summarize"),
                                  )
                                }
                              />
                              <Button
                                key="work-page"
                                label="Work from page"
                                plain
                                onPress={() =>
                                  run("Requesting help", () =>
                                    w.pageAction("work"),
                                  )
                                }
                              />
                              {browserURL(
                                c.server,
                                "/p/" + encodeURIComponent(w.content.page.id),
                              ) && (
                                <Link
                                  href={browserURL(
                                    c.server,
                                    "/p/" +
                                      encodeURIComponent(w.content.page.id),
                                  )!}
                                  label="Open page in browser"
                                />
                              )}
                            </Box>
                            <Text dimColor>
                              Text preview. Open in browser for layout and
                              interactive content.
                            </Text>
                            {readerChunks(w.content.html).map((text, index) => (
                              <Text key={"page-text-" + index}>{text}</Text>
                            ))}
                            {Input && !room.archived && !w.busy && (
                              <Input
                                key="edit-page"
                                label="Edit with Claude"
                                placeholder="Describe the change…"
                                value={w.editRequest}
                                submitLabel="request edit"
                                onInput={(v) => changing("edit", v)}
                                onSubmit={(v) =>
                                  run("Requesting edit", () =>
                                    w.pageAction("edit", v),
                                  )
                                }
                              />
                            )}
                            {!Input && !room.archived && !w.busy && (
                              <Button
                                key="edit-page-prompt"
                                label="Edit with Claude"
                                onPress={() => {
                                  if (w.content)
                                    fill(
                                      pageEditPrompt(c, w.content, "") +
                                        "\nMy requested change: ",
                                    );
                                }}
                              />
                            )}
                          </Box>
                        ) : (
                          <Box flexDirection="column" gap={1}>
                            <Text bold>Shared pages</Text>
                            <Text dimColor>
                              Plans, specs, findings and decisions in{" "}
                              {label(room.name)}.
                            </Text>
                            {Input && (
                              <Input
                                key="page-search"
                                label="Find"
                                placeholder="Title or description…"
                                value={w.pageQuery}
                                onInput={(v) => changing("query", v)}
                                onSubmit={() => {}}
                              />
                            )}
                            {filteredPages.length === 0 && (
                              <Text>
                                {w.loading
                                  ? "Loading pages…"
                                  : w.pageQuery
                                    ? "No loaded pages match. Try another search or load more pages."
                                    : "Create a page to give your agents a shared place to work."}
                              </Text>
                            )}
                            {filteredPages.map((p) => (
                              <Box
                                key={"page-row-" + p.id}
                                flexDirection="column"
                              >
                                <Button
                                  key={"page-" + p.id}
                                  label={label(p.title)}
                                  plain
                                  onPress={() =>
                                    run("Opening page", () => w.readPage(p.id))
                                  }
                                />
                                <Text dimColor>
                                  {label(p.description)}
                                  {p.description ? "\n" : ""}
                                  {channelOf(p.channel_id)} · revision{" "}
                                  {p.revision} · {label(p.updated_at)}
                                </Text>
                              </Box>
                            ))}
                            {Object.values(w.cursors).some(Boolean) && (
                              <Button
                                key="more-pages"
                                label="Load more pages"
                                plain
                                onPress={() =>
                                  run("Loading pages", () => w.morePages())
                                }
                              />
                            )}
                            {Input &&
                              !room.archived &&
                              w.selection.channel &&
                              !w.busy && (
                                <Input
                                  key="new-page"
                                  label="Create with Claude"
                                  placeholder="Describe a plan, spec or document…"
                                  value={w.newPageRequest}
                                  submitLabel="create page"
                                  onInput={(v) => changing("new-page", v)}
                                  onSubmit={(v) =>
                                    run("Requesting page", () =>
                                      w.createPage(v),
                                    )
                                  }
                                />
                              )}
                            {!Input &&
                              !room.archived &&
                              w.selection.channel &&
                              !w.busy && (
                                <Button
                                  key="new-page-prompt"
                                  label="Create with Claude"
                                  onPress={() =>
                                    fill(
                                      "Create a shared Tincan page in " +
                                        JSON.stringify({
                                          connection: c.connection,
                                          channel_id: w.selection.channel,
                                        }) +
                                        ". Check pages_list for an existing page first. Use page_create with self-contained HTML, title, description and a fresh idempotency_key; show the accepted page link and revision. My request: ",
                                    )
                                  }
                                />
                              )}
                            {w.selection.channel && (
                              <Text dimColor>
                                New pages go in {channelOf(w.selection.channel)}
                                . Choose a channel in Conversation to change the
                                destination.
                              </Text>
                            )}
                          </Box>
                        )}
                      </Box>
                    )}
                  </Box>
                )}
                {!room && !w.loading && (
                  <Text>
                    No shared room is available. Refresh your connection or join
                    an invite.
                  </Text>
                )}
              </Box>
            )}
          </Box>
        )}
        {w.freshness && (
          <Text dimColor>{w.freshness} · refreshes while open</Text>
        )}
      </Box>
    );
  });
};
