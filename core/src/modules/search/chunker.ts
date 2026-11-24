import { createHash } from "node:crypto";

export type ChatLine = Readonly<{ messageId: string; sentAt: Date; sender: string; text: string }>;

export type Chunk = Readonly<{
  messageIds: readonly string[];
  firstSentAt: Date;
  lastSentAt: Date;
  text: string;
  hash: string;
}>;

export type ChunkOptions = Readonly<{
  timeZone: string;
  gapMs?: number;
  maxChars?: number;
  maxLines?: number;
}>;

function dayAndTime(at: Date, timeZone: string): { day: string; time: string } {
  const parts = Object.fromEntries(
    new Intl.DateTimeFormat("en-GB", {
      timeZone,
      day: "numeric",
      month: "short",
      year: "numeric",
      hour: "2-digit",
      minute: "2-digit",
      hourCycle: "h23",
    })
      .formatToParts(at)
      .map((part) => [part.type, part.value]),
  );
  return {
    day: `${parts.day} ${parts.month} ${parts.year}`,
    time: `${parts.hour}:${parts.minute}`,
  };
}

const oneLine = (text: string, max: number) => {
  const flat = text.replace(/\s+/g, " ").trim();
  const characters = Array.from(flat);
  return characters.length > max ? `${characters.slice(0, max).join("")}…` : flat;
};

/** Groups one chat's lines, oldest first, into chunks of messages sent close together. */
export function chunkLines(
  chatTitle: string,
  lines: readonly ChatLine[],
  options: ChunkOptions,
): Chunk[] {
  const gapMs = options.gapMs ?? 10 * 60_000;
  const maxChars = options.maxChars ?? 1_500;
  const maxLines = options.maxLines ?? 30;
  const header = `Chat: ${oneLine(chatTitle, 80)}`;
  const chunks: Chunk[] = [];
  let group: ChatLine[] = [];
  let size = 0;

  const close = () => {
    const first = group[0];
    const last = group[group.length - 1];
    if (!first || !last) return;
    const out = [header];
    let day = "";
    for (const line of group) {
      const when = dayAndTime(line.sentAt, options.timeZone);
      if (when.day !== day) {
        out.push(when.day);
        day = when.day;
      }
      out.push(`${when.time} ${oneLine(line.sender, 60)}: ${oneLine(line.text, maxChars)}`);
    }
    const text = out.join("\n");
    chunks.push(
      Object.freeze({
        messageIds: group.map((line) => line.messageId),
        firstSentAt: first.sentAt,
        lastSentAt: last.sentAt,
        text,
        hash: createHash("sha256").update(text).digest("hex"),
      }),
    );
    group = [];
    size = 0;
  };

  for (const line of lines) {
    const last = group[group.length - 1];
    const length = Math.min(Array.from(line.text).length, maxChars) + line.sender.length + 8;
    const apart = last ? line.sentAt.getTime() - last.sentAt.getTime() > gapMs : false;
    if (last && (apart || size + length > maxChars || group.length >= maxLines)) close();
    group.push(line);
    size += length;
  }
  close();
  return chunks;
}
