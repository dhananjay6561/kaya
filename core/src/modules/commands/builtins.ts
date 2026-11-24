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

/** Anything that starts with a slash is meant for Kaya itself; everything else is not a command. */
export function parseSlash(text: string): SlashCommand | undefined {
  const trimmed = text.trim();
  if (!trimmed.startsWith("/")) return undefined;
  const [word = "", ...rest] = trimmed.slice(1).split(/\s+/);
  const name = word.toLowerCase();
  if (!isBuiltin(name)) return { kind: "unknown", name: Array.from(word).slice(0, 30).join("") };
  return rest.join("") === "" ? { kind: "builtin", name } : { kind: "extra_text", name };
}

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
        ? `I don't know /${command.name}. Send /help to see what I can do.`
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
        ? `${helpText}\nI am paused: only these commands get an answer.`
        : helpText;
    }
    case "status":
      return deps.status();
    case "pause":
      return (await deps.pause.pause(commandId)) === "paused"
        ? "Paused. Until you send /resume I answer only /help, /status and /resume."
        : "Already paused. Send /resume to start again.";
    case "resume":
      return (await deps.pause.resume(commandId)) === "resumed"
        ? "Resumed. I am answering again."
        : "I was not paused.";
  }
}
