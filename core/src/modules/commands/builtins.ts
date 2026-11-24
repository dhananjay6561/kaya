import type { PauseSwitch } from "../pause/index.js";

export const builtinNames = ["help", "status", "pause", "resume"] as const;
export type BuiltinName = (typeof builtinNames)[number];

export type SlashCommand = Readonly<
  | { kind: "builtin"; name: BuiltinName }
  | { kind: "extra_text"; name: BuiltinName }
  | { kind: "unknown"; name: string }
>;

const isBuiltin = (name: string): name is BuiltinName =>
  (builtinNames as readonly string[]).includes(name);

const graphemes = new Intl.Segmenter(undefined, { granularity: "grapheme" });

function firstCharacters(text: string, count: number): string {
  return Array.from(graphemes.segment(text), (part) => part.segment)
    .slice(0, count)
    .join("");
}

export function parseSlash(text: string): SlashCommand | undefined {
  const cleaned = text
    .normalize("NFKC")
    .replace(/(?!\u200d)\p{Cf}/gu, "")
    .trim();
  if (!cleaned.startsWith("/")) return undefined;
  const [word = "", ...rest] = cleaned.slice(1).split(/\s+/);
  const name = word.toLowerCase().replace(/[.!?]+$/, "");
  if (!isBuiltin(name)) return { kind: "unknown", name: firstCharacters(word, 30) };
  return rest.join("") === "" ? { kind: "builtin", name } : { kind: "extra_text", name };
}

export const isBuiltinCommand = (text: string) => parseSlash(text)?.kind === "builtin";

export type BuiltinDeps = Readonly<{
  pause: PauseSwitch;
  status(): Promise<string>;
}>;

const helpText = [
  "Commands:",
  "/help: this list",
  "/status: how Kaya is doing",
  "/pause: stop answering until /resume",
  "/resume: start answering again",
  "Anything else you send here I repeat back, for now.",
].join("\n");

export async function answerSlash(
  command: SlashCommand,
  commandId: string,
  deps: BuiltinDeps,
): Promise<string> {
  switch (command.kind) {
    case "unknown":
      return command.name
        ? `/${command.name} is not a command I know. Send /help to see what I can do.`
        : "Send /help to see what I can do.";
    case "extra_text":
      return `/${command.name} takes nothing after it, so nothing was done. Send just /${command.name}.`;
    case "builtin":
      break;
  }
  switch (command.name) {
    case "help": {
      const state = await deps.pause.state();
      return state.paused
        ? `${helpText}\nI am paused: only slash commands get an answer.`
        : helpText;
    }
    case "status":
      return deps.status();
    case "pause":
      return (await deps.pause.pause(commandId)) === "paused"
        ? "Paused. Until you send /resume I answer only slash commands, like /status."
        : "Already paused. Send /resume to start again.";
    case "resume":
      return (await deps.pause.resume(commandId)) === "resumed"
        ? "Resumed. I am answering again."
        : "I was not paused.";
  }
}
