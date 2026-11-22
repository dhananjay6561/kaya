import type { Config } from "../../config/index.js";
import { createGatewayChannel, type GatewayChannelDeps } from "./gateway.js";
import { createStubChannel } from "./stub.js";
import type { ChatChannel } from "./types.js";

export {
  createGatewayChannel,
  type GatewayChannelDeps,
  GatewayNotLinkedError,
  IdempotencyKeyReusedError,
  type IncomingEvents,
  requestIdFor,
  SendFailedError,
  type SendResults,
} from "./gateway.js";
export { ChannelNotStartedError, createStubChannel, type StubChannel } from "./stub.js";
export type * from "./types.js";

export function createChannel(
  config: Config["channel"],
  gateway?: GatewayChannelDeps,
): ChatChannel {
  switch (config.provider) {
    case "gateway":
      if (!gateway) throw new Error("the gateway chat channel needs a database");
      return createGatewayChannel(gateway);
    case "stub":
      return createStubChannel();
  }
}
