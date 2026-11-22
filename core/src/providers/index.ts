import type { Config } from "../config/index.js";
import { type ChatChannel, createChannel, type GatewayChannelDeps } from "./channel/index.js";
import { createLlm, type LlmProvider } from "./llm/index.js";
import { createTelephony, type TelephonyProvider } from "./telephony/index.js";

export { ProviderNotReadyError } from "./errors.js";

export type Providers = Readonly<{
  llm: LlmProvider;
  channel: ChatChannel;
  telephony: TelephonyProvider;
}>;

export type ProviderDeps = Readonly<{ gateway?: GatewayChannelDeps }>;

export function createProviders(
  config: Pick<Config, "llm" | "channel" | "telephony">,
  deps: ProviderDeps = {},
): Providers {
  return Object.freeze({
    llm: createLlm(config.llm),
    channel: createChannel(config.channel, deps.gateway),
    telephony: createTelephony(config.telephony),
  });
}
