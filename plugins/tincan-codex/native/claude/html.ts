import { clean } from "./model";

function entities(text: string): string {
  const names: Record<string, string> = {
    amp: "&",
    lt: "<",
    gt: ">",
    quot: '"',
    apos: "'",
    nbsp: " ",
    ndash: "–",
    mdash: "—",
    hellip: "…",
    copy: "©",
  };
  return text.replace(
    /&(#x[\da-f]+|#\d+|[a-z]+);/gi,
    (raw: string, key: string) => {
      if (!key.startsWith("#")) return names[key.toLowerCase()] ?? raw;
      const n =
        key[1]?.toLowerCase() === "x"
          ? parseInt(key.slice(2), 16)
          : parseInt(key.slice(1), 10);
      return n > 0 && n <= 0x10ffff && !(n >= 0xd800 && n <= 0xdfff)
        ? String.fromCodePoint(n)
        : "";
    },
  );
}

// A text reader, never an HTML runtime. No scripts, CSS, frames, remote assets,
// event attributes or shared URLs are executed by the mod. Browser links are
// generated separately from the trusted connection origin and stable page ID.
export function readableHTML(html: string): string {
  let source = html.slice(0, 256 * 1024);
  const body = /<body\b[^>]*>([\s\S]*?)(?:<\/body\s*>|$)/i.exec(source);
  if (body) source = body[1] ?? "";
  source = source.replace(/<!--[\s\S]*?(?:-->|$)/g, "");
  source = source.replace(
    /<(script|style|head|template|iframe|object|svg|canvas)\b[^>]*>[\s\S]*?(?:<\/\1\s*>|$)/gi,
    "",
  );
  source = source.replace(
    /<\/?(?:div|p|article|section|header|footer|main|aside|nav|table|tr|blockquote|ul|ol|pre)\b[^>]*>/gi,
    "\n\n",
  );
  source = source.replace(
    /<h([1-6])\b[^>]*>/gi,
    (_raw: string, level: string) => "\n\n" + "#".repeat(Number(level)) + " ",
  );
  source = source.replace(/<\/h[1-6]\s*>/gi, "\n\n");
  source = source.replace(/<li\b[^>]*>/gi, "\n- ").replace(/<\/li\s*>/gi, "\n");
  source = source
    .replace(/<br\b[^>]*>/gi, "\n")
    .replace(/<hr\b[^>]*>/gi, "\n\n---\n\n");
  source = source.replace(/<\/?(?:td|th)\b[^>]*>/gi, "  ");
  source = source.replace(
    /<img\b[^>]*\balt\s*=\s*(?:"([^"]*)"|'([^']*)')[^>]*>/gi,
    (_raw: string, a: string, b: string) => a || b || "",
  );
  source = source.replace(/<[^>]*(?:>|$)/g, "");
  return clean(entities(source))
    .replace(/[ \t]+/g, " ")
    .replace(/ *\n */g, "\n")
    .replace(/\n{3,}/g, "\n\n")
    .trim();
}

export function readerChunks(html: string): string[] {
  const text =
    readableHTML(html) ||
    "This page has no readable text. Open it in the browser to view its layout.";
  const chunks: string[] = [];
  // Text blocks stay below the engine's 10,000-character Markdown limit.
  for (let start = 0; start < text.length; start += 8000)
    chunks.push(text.slice(start, start + 8000));
  return chunks;
}
