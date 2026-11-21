import type { Config } from "../../config/index.js";
import { createStubChannel } from "./stub.js";
import type { ChatChannel } from "./types.js";

export { ChannelNotStartedError, createStubChannel, type StubChannel } from "./stub.js";
export type * from "./types.js";

export function createChannel(config: Config["channel"]): ChatChannel {
  switch (config.provider) {
    case "stub":
      return createStubChannel();
  }
}
