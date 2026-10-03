import { describe, expect, test, tier } from "claude-code/testing";
import { readableHTML, readerChunks } from "../html";
import { agents, browserURL, presence } from "../model";

tier("user");
describe("html", () => {
  test("extracts readable headings and lists without scripts, hidden markup or terminal controls", () => {
    const text = readableHTML(
      "<head><title>Hidden</title></head><h2>Plan &amp; scope</h2><ul><li>Ship</li><li>Review &#x1F44D;</li></ul><iframe>hidden</iframe><script>evil</script><style>hidden</style><p>\u001b]52;secret\u0007</p>",
    );
    expect(text).toContain("## Plan & scope");
    expect(text).toContain("- Ship");
    expect(text).not.toContain("<script>");
    expect(text).not.toContain("Hidden");
    expect(text).not.toContain("evil");
    expect(text).not.toContain("\u001b");
    expect(
      readerChunks("<p>" + "a".repeat(20000) + "</p>").every(
        (s) => s.length <= 8000,
      ),
    ).toBe(true);
  });
  test("expiry downgrades stale presence and browser links stay on the connection origin", () => {
    const agent = agents([
      {
        id: "peer",
        presence: "available",
        presence_expires_at: "2026-10-01T11:00:00Z",
      },
    ])[0]!;
    expect(presence(agent, Date.parse("2026-10-01T12:00:00Z"))).toBe("Offline");
    expect(browserURL("https://app.gotincan.com", "/p/example")).toBe(
      "https://app.gotincan.com/p/example",
    );
    expect(
      browserURL(
        "https://app.gotincan.com",
        "https://attacker.example/p/example",
      ),
    ).toBeUndefined();
    expect(
      browserURL("https://user:secret@app.gotincan.com", "/p/example"),
    ).toBeUndefined();
    expect(browserURL("javascript:alert(1)", "/p/example")).toBeUndefined();
  });
});
